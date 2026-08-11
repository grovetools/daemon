// Package sync provides the client-side state machine and HTTP client
// for the notebook sync protocol's Phase 1 push pipeline.
package sync

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"os"
	"strings"
	gosync "sync"
	"sync/atomic"
	"time"

	"github.com/grovetools/core/config"
	"github.com/grovetools/core/logging"
	"github.com/grovetools/core/pkg/devicekey"
	"github.com/grovetools/core/pkg/syncproto"
)

// ErrUnauthorized classifies a 401 from the sync server: the bearer token was
// REJECTED, as opposed to the server being unreachable. The distinction is the
// whole stale-token trap — a server destroyed and recreated mints fresh
// tokens, and every subsequent handshake fails with 401 forever. Without a
// machine-readable classification the transport loop cannot tell "not up yet"
// (retry quietly, it will fix itself) from "your token is dead" (retry forever
// and nothing will ever fix itself), so it logged both at debug and spun.
//
// Callers test with IsAuthError; every client method that talks to the server
// wraps its 401 through authStatusError so the sentinel survives the
// fmt.Errorf chains between the HTTP call and the transport loop.
var ErrUnauthorized = errors.New("sync server rejected the token")

// IsAuthError reports whether err was caused by the server rejecting the
// bearer token (HTTP 401), anywhere in its wrap chain.
func IsAuthError(err error) bool { return errors.Is(err, ErrUnauthorized) }

// isAuthStatus reports whether an HTTP status means "the token was rejected".
//
// 401 ONLY, deliberately. 403 is the server's AUTHORIZATION answer — a token
// it recognizes, for a user with no grant on this notespace (getUserPrefixes,
// sync/pkg/server/handlers.go) — and its remediation is a share grant, not a
// new token. Classifying it here was tried and reverted: it told operators of
// perfectly valid share-scoped clients to mint a replacement token, and it put
// their transport in a permanent tear-down-and-reconnect cycle (observed on
// the cluster's share-scoped dev-c, which 403s on every pull by design).
// Grant surfacing belongs to the share work, not to the stale-token path.
func isAuthStatus(code int) bool {
	return code == http.StatusUnauthorized
}

// authStatusError builds the error for a rejected request, carrying the
// ErrUnauthorized sentinel plus the server's own explanation.
func authStatusError(op string, code int, body []byte) error {
	detail := strings.TrimSpace(string(body))
	if detail == "" {
		detail = http.StatusText(code)
	}
	return fmt.Errorf("%s rejected with status %d (%s): %w", op, code, detail, ErrUnauthorized)
}

// DeviceSigner is the private-key capability needed for a v2 handshake.
// devicekey.Key implements it without exposing private key material.
type DeviceSigner interface {
	DeviceID() string
	Sign([]byte) []byte
}

// ClientConfig wraps the configuration needed to initialize a sync client.
type ClientConfig struct {
	ServerURL  string
	Token      string
	DeviceID   string
	OriginID   string
	Signer     DeviceSigner
	Logger     *logging.UnifiedLogger
	HTTPClient *http.Client
	// TLSConfig, when non-nil, is applied to BOTH the general client and the
	// long-poll client. It carries the pinned root pool for a self-signed
	// deployment (config.SyncConfig.CACert); nil means the system trust
	// store, which is what a publicly-trusted server wants.
	TLSConfig *tls.Config
}

// Client is an HTTP wrapper for grove-syncd push/pull endpoints.
type Client struct {
	serverURL  string
	deviceID   string
	originID   string
	signer     DeviceSigner
	log        *logging.UnifiedLogger
	httpClient *http.Client

	// bearer is either the configured legacy credential or a short-lived v2
	// session. Every request reads it under authMu so a refresh is immediately
	// visible to push, pull, snapshot, history, and blob goroutines alike.
	authMu      gosync.RWMutex
	bearer      string
	staticToken string
	session     bool

	// refreshMu coalesces only a currently in-flight refresh. Waiters share its
	// result, but a later call may retry after a transient failure clears.
	refreshMu       gosync.Mutex
	refreshBearer   string
	refreshInFlight *sessionRefresh
	// pollClient serves long-poll pulls: its timeout must exceed the
	// server-side wait or every quiet poll dies awaiting headers exactly
	// when the server would respond (the general client's 30s == wait).
	pollClient *http.Client

	// caps is the cached handshake response. Atomic because it is WRITTEN by
	// re-handshakes on a shared, live client — the anti-entropy pass and the
	// transport's server-epoch probe both re-run Capabilities — while push,
	// pull, and blob goroutines READ it for their size ceilings. A plain field
	// is a data race between them.
	caps atomic.Pointer[syncproto.CapabilitiesResponse]

	// onAuthFailure is notified whenever the server rejects this client's
	// token. The handshake happens before any owner can install a hook, so
	// this only ever fires for the LIVE pipelines (push/pull/snapshot/blob) —
	// which is exactly the case the transport loop cannot otherwise see: it
	// caches a connected client and never re-handshakes, so a token revoked
	// or invalidated mid-run left every pipeline 401-ing forever with no path
	// back short of a daemon restart. Must not block; SetAuthFailureHook's
	// callers only flag state.
	authHookMu    gosync.Mutex
	onAuthFailure func(error)
}

// SetAuthFailureHook installs the callback invoked when the server rejects
// this client's token on any request. Called by the transport owner right
// after construction; nil clears it. The hook runs on the calling pipeline's
// goroutine, so it must return promptly.
func (c *Client) SetAuthFailureHook(fn func(error)) {
	c.authHookMu.Lock()
	c.onAuthFailure = fn
	c.authHookMu.Unlock()
}

// rejectIfUnauthorized returns a classified error (and notifies the hook) when
// resp carries a token-rejection status, or nil to let the caller continue.
// It consumes resp.Body only in the rejection case.
func (c *Client) rejectIfUnauthorized(op string, resp *http.Response) error {
	if !isAuthStatus(resp.StatusCode) {
		return nil
	}
	body, _ := io.ReadAll(resp.Body)
	err := authStatusError(op, resp.StatusCode, body)
	c.authHookMu.Lock()
	hook := c.onAuthFailure
	c.authHookMu.Unlock()
	if hook != nil {
		hook(err)
	}
	return err
}

// NewClient constructs a new sync client.
func NewClient(cfg ClientConfig) *Client {
	// One transport for both clients so a pinned root pool cannot end up
	// applied to the general client but not to the long-poll one — the pull
	// pipeline lives entirely on pollClient, so a half-applied TLS config
	// would fail exactly the traffic that matters.
	var transport http.RoundTripper
	if cfg.TLSConfig != nil {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.TLSClientConfig = cfg.TLSConfig
		transport = t
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{
			Timeout:   30 * time.Second,
			Transport: transport,
		}
	} else if transport != nil && cfg.HTTPClient.Transport == nil {
		cfg.HTTPClient.Transport = transport
	}
	pollClient := &http.Client{Timeout: 90 * time.Second, Transport: transport}
	return &Client{
		serverURL:   cfg.ServerURL,
		deviceID:    cfg.DeviceID,
		originID:    cfg.OriginID,
		signer:      cfg.Signer,
		log:         cfg.Logger,
		httpClient:  cfg.HTTPClient,
		pollClient:  pollClient,
		bearer:      cfg.Token,
		staticToken: cfg.Token,
	}
}

// Capabilities performs the handshake with the server, caching the response
// for future blob-size decisions. A device signer prefers v2; v1 is attempted
// only when a real legacy token is available.
func (c *Client) Capabilities(ctx context.Context, clientVersion string) (*syncproto.CapabilitiesResponse, error) {
	if c.signer != nil {
		capResp, err := c.deviceCapabilities(ctx, clientVersion)
		if err == nil {
			return capResp, nil
		}
		if c.staticToken == "" {
			return nil, err
		}
	}
	return c.legacyCapabilities(ctx, clientVersion)
}

func (c *Client) legacyCapabilities(ctx context.Context, clientVersion string) (*syncproto.CapabilitiesResponse, error) {
	if c.staticToken == "" {
		return nil, fmt.Errorf("sync authentication not configured: no device key or legacy token")
	}
	offered := []int{syncproto.ProtocolVersionLegacy}
	req := &syncproto.CapabilitiesRequest{
		ClientName:       "groved",
		ClientVersion:    clientVersion,
		ProtocolVersions: offered,
		OriginID:         c.originID,
		DeviceID:         c.deviceID,
	}
	httpReq, err := c.jsonRequest(ctx, "POST", "/sync/capabilities", req)
	if err != nil {
		return nil, fmt.Errorf("failed to create capabilities request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.staticToken)
	capResp, err := c.readCapabilities("capabilities request", httpReq, offered)
	if err != nil {
		return nil, err
	}
	c.setBearer(c.staticToken, false)
	c.caps.Store(capResp)
	return capResp, nil
}

func (c *Client) deviceCapabilities(ctx context.Context, clientVersion string) (*syncproto.CapabilitiesResponse, error) {
	if c.signer == nil {
		return nil, fmt.Errorf("device signer not configured")
	}
	if c.deviceID != "" && c.signer.DeviceID() != c.deviceID {
		return nil, fmt.Errorf("device signer belongs to %q, client device is %q", c.signer.DeviceID(), c.deviceID)
	}

	identityReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.serverURL+"/sync/identity", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create identity request: %w", err)
	}
	identityResp, err := c.httpClient.Do(identityReq)
	if err != nil {
		return nil, fmt.Errorf("identity request failed: %w", err)
	}
	defer identityResp.Body.Close()
	if identityResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(identityResp.Body)
		return nil, fmt.Errorf("identity request failed with status %d: %s", identityResp.StatusCode, strings.TrimSpace(string(body)))
	}
	var identity syncproto.IdentityResponse
	if err := json.NewDecoder(identityResp.Body).Decode(&identity); err != nil {
		return nil, fmt.Errorf("failed to decode identity response: %w", err)
	}
	if identity.ServerEpoch == "" || !containsVersion(identity.ProtocolVersions, syncproto.ProtocolVersionDeviceSession) {
		return nil, fmt.Errorf("server does not advertise device-session protocol v%d", syncproto.ProtocolVersionDeviceSession)
	}

	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("failed to create capabilities nonce: %w", err)
	}
	offered := syncproto.SupportedProtocolVersions()
	req := syncproto.CapabilitiesRequest{
		ClientName:       "groved",
		ClientVersion:    clientVersion,
		ProtocolVersions: offered,
		OriginID:         c.originID,
		DeviceID:         c.signer.DeviceID(),
		ServerEpoch:      identity.ServerEpoch,
		Timestamp:        syncproto.CanonicalTimestamp(time.Now()),
		Nonce:            base64.StdEncoding.EncodeToString(nonce),
	}
	payload, err := syncproto.CanonicalCapabilities(req)
	if err != nil {
		return nil, fmt.Errorf("failed to canonicalize capabilities proof: %w", err)
	}
	if err := syncproto.SetCapabilitiesSignature(&req, c.signer.Sign(payload)); err != nil {
		return nil, fmt.Errorf("failed to sign capabilities proof: %w", err)
	}
	httpReq, err := c.jsonRequest(ctx, "POST", "/sync/capabilities", &req)
	if err != nil {
		return nil, fmt.Errorf("failed to create capabilities request: %w", err)
	}
	capResp, err := c.readCapabilities("device capabilities request", httpReq, offered)
	if err != nil {
		return nil, err
	}
	if capResp.ProtocolVersion < syncproto.ProtocolVersionDeviceSession || capResp.SessionToken == "" {
		return nil, fmt.Errorf("device capabilities response did not establish a device session")
	}
	if capResp.ServerEpoch != identity.ServerEpoch {
		return nil, fmt.Errorf("server epoch changed during device handshake")
	}
	c.setBearer(capResp.SessionToken, true)
	c.caps.Store(capResp)
	return capResp, nil
}

func (c *Client) readCapabilities(op string, req *http.Request, offered []int) (*syncproto.CapabilitiesResponse, error) {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s failed: %w", op, err)
	}
	defer resp.Body.Close()
	if err := c.rejectIfUnauthorized(op, resp); err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("%s failed with status %d: %s", op, resp.StatusCode, string(body))
	}
	var capResp syncproto.CapabilitiesResponse
	if err := json.NewDecoder(resp.Body).Decode(&capResp); err != nil {
		return nil, fmt.Errorf("failed to decode capabilities response: %w", err)
	}
	if capResp.Error != "" {
		return nil, fmt.Errorf("server capabilities error: %s", capResp.Error)
	}
	if !containsVersion(offered, capResp.ProtocolVersion) {
		return nil, fmt.Errorf("server selected unoffered protocol version %d", capResp.ProtocolVersion)
	}
	if !capResp.Capabilities.SupportsVersion(capResp.ProtocolVersion) {
		return nil, fmt.Errorf("server selected protocol version %d without advertising it", capResp.ProtocolVersion)
	}
	return &capResp, nil
}

func containsVersion(versions []int, wanted int) bool {
	for _, version := range versions {
		if version == wanted {
			return true
		}
	}
	return false
}

// Register establishes immutable notespace identity before any data pipeline starts.
func (c *Client) Register(ctx context.Context, req syncproto.RegisterRequest) (*syncproto.RegisterResponse, error) {
	if req.ProtocolVersion == 0 {
		req.ProtocolVersion = syncproto.ProtocolVersionNotespaceID
	}
	if req.DeviceID == "" {
		req.DeviceID = c.deviceID
	}
	httpReq, err := c.newRequest(ctx, http.MethodPost, "/sync/register", &req)
	if err != nil {
		return nil, err
	}
	resp, err := c.doAuthenticated(ctx, c.httpClient, "register request", replayableRequest(httpReq))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := c.rejectIfUnauthorized("register request", resp); err != nil {
		return nil, err
	}
	var out syncproto.RegisterResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode register response: %w", err)
	}
	if resp.StatusCode != http.StatusOK || out.Error != nil {
		if out.Error != nil {
			return &out, fmt.Errorf("registration %s: %s", out.Error.Code, out.Error.Message)
		}
		return &out, fmt.Errorf("registration failed with status %d", resp.StatusCode)
	}
	return &out, nil
}

// Push uploads a batch of outbox entries to the server. Returns the
// per-event results in the same order as the input events.
func (c *Client) Push(ctx context.Context, notespace string, events []syncproto.SyncEvent) (*syncproto.PushResponse, error) {
	if c.caps.Load() == nil {
		return nil, fmt.Errorf("capabilities handshake not performed; call Capabilities() first")
	}

	req := &syncproto.PushRequest{
		ProtocolVersion: syncproto.ProtocolVersionNotespaceID,
		NotespaceID:     syncproto.NotespaceID(notespace),
		OriginID:        c.originID,
		DeviceID:        c.deviceID,
		Events:          events,
	}

	httpReq, err := c.newRequest(ctx, "POST", "/sync/push", req)
	if err != nil {
		return nil, fmt.Errorf("failed to create push request: %w", err)
	}

	resp, err := c.doAuthenticated(ctx, c.httpClient, "push request", replayableRequest(httpReq))
	if err != nil {
		return nil, fmt.Errorf("push request failed: %w", err)
	}
	defer resp.Body.Close()

	if err := c.rejectIfUnauthorized("push request", resp); err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("push request failed with status %d: %s", resp.StatusCode, string(body))
	}

	var pushResp syncproto.PushResponse
	if err := json.NewDecoder(resp.Body).Decode(&pushResp); err != nil {
		return nil, fmt.Errorf("failed to decode push response: %w", err)
	}

	if pushResp.Error != nil {
		return nil, fmt.Errorf("server push error: %s", pushResp.Error)
	}

	return &pushResp, nil
}

// PushBlob uploads a content-addressed blob chunk to the server.
func (c *Client) PushBlob(ctx context.Context, hash string, data []byte) error {
	url := fmt.Sprintf("%s/sync/blob/%s", c.serverURL, hash)

	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("failed to create blob upload request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/octet-stream")

	resp, err := c.doAuthenticated(ctx, c.httpClient, "blob upload", replayableRequest(httpReq))
	if err != nil {
		return fmt.Errorf("blob upload request failed: %w", err)
	}
	defer resp.Body.Close()

	if err := c.rejectIfUnauthorized("blob upload", resp); err != nil {
		return err
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("blob upload failed with status %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

// Snapshot fetches the server's manifest snapshot for a notespace.
func (c *Client) Snapshot(ctx context.Context, notespace string) (*syncproto.SnapshotManifest, error) {
	url := fmt.Sprintf("%s/sync/snapshot?protocol_version=%d&notespace=%s", c.serverURL, syncproto.ProtocolVersionNotespaceID, neturl.QueryEscape(notespace))

	httpReq, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create snapshot request: %w", err)
	}

	resp, err := c.doAuthenticated(ctx, c.httpClient, "snapshot request", replayableRequest(httpReq))
	if err != nil {
		return nil, fmt.Errorf("snapshot request failed: %w", err)
	}
	defer resp.Body.Close()

	if err := c.rejectIfUnauthorized("snapshot request", resp); err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("snapshot request failed with status %d: %s", resp.StatusCode, string(body))
	}

	var manifest syncproto.SnapshotManifest
	if err := json.NewDecoder(resp.Body).Decode(&manifest); err != nil {
		return nil, fmt.Errorf("failed to decode snapshot response: %w", err)
	}

	return &manifest, nil
}

type requestFactory func() (*http.Request, error)

type sessionRefresh struct {
	done chan struct{}
	err  error
}

func replayableRequest(req *http.Request) requestFactory {
	return func() (*http.Request, error) {
		clone := req.Clone(req.Context())
		if req.GetBody != nil {
			body, err := req.GetBody()
			if err != nil {
				return nil, err
			}
			clone.Body = body
		}
		return clone, nil
	}
}

func (c *Client) currentBearer() (string, bool) {
	c.authMu.RLock()
	defer c.authMu.RUnlock()
	return c.bearer, c.session
}

func (c *Client) setBearer(bearer string, session bool) {
	c.authMu.Lock()
	c.bearer = bearer
	c.session = session
	c.authMu.Unlock()
}

// refreshSession coalesces all simultaneous 401s for the same session bearer
// into one signed handshake. Only the in-flight operation is shared: once it
// finishes, failures are forgotten so a later request can recover normally.
func (c *Client) refreshSession(ctx context.Context, rejected string) error {
	c.refreshMu.Lock()
	current, session := c.currentBearer()
	if session && current != rejected {
		c.refreshMu.Unlock()
		return nil
	}
	if !session {
		c.refreshMu.Unlock()
		return ErrUnauthorized
	}
	if c.refreshInFlight != nil && c.refreshBearer == rejected {
		call := c.refreshInFlight
		c.refreshMu.Unlock()
		select {
		case <-call.done:
			return call.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	call := &sessionRefresh{done: make(chan struct{})}
	c.refreshBearer = rejected
	c.refreshInFlight = call
	c.refreshMu.Unlock()

	_, call.err = c.deviceCapabilities(ctx, "")

	c.refreshMu.Lock()
	close(call.done)
	c.refreshInFlight = nil
	c.refreshBearer = ""
	c.refreshMu.Unlock()
	return call.err
}

// doAuthenticated applies the current synchronized bearer to one request. A
// rejected device session is refreshed and retried exactly once. Legacy 401s
// are returned unchanged for the existing classifier and failure hook.
func (c *Client) doAuthenticated(ctx context.Context, hc *http.Client, op string, makeRequest requestFactory) (*http.Response, error) {
	req, err := makeRequest()
	if err != nil {
		return nil, err
	}
	bearer, session := c.currentBearer()
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusUnauthorized || !session {
		return resp, nil
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if err := c.refreshSession(ctx, bearer); err != nil {
		return nil, fmt.Errorf("%s session refresh failed: %w", op, err)
	}
	retry, err := makeRequest()
	if err != nil {
		return nil, err
	}
	refreshed, _ := c.currentBearer()
	retry.Header.Set("Authorization", "Bearer "+refreshed)
	return hc.Do(retry)
}

func (c *Client) jsonRequest(ctx context.Context, method, path string, body interface{}) (*http.Request, error) {
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return nil, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.serverURL+path, &buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return req, nil
}

// newRequest constructs an authenticated JSON request. The bearer is applied
// by doAuthenticated at send time, never captured here.
func (c *Client) newRequest(ctx context.Context, method, path string, body interface{}) (*http.Request, error) {
	return c.jsonRequest(ctx, method, path, body)
}

// MaxInlineSize returns the server's maximum inline content size, or the
// protocol default if capabilities have not been negotiated.
func (c *Client) MaxInlineSize() int64 {
	if caps := c.caps.Load(); caps != nil && caps.Capabilities.MaxInlineSize > 0 {
		return caps.Capabilities.MaxInlineSize
	}
	return 256 * 1024 // 256KB default
}

// MaxBlobSize returns the server's advertised single-blob ceiling in bytes, or
// 0 when the server did not advertise one. Zero is the "unknown, don't enforce
// client-side" sentinel: DrainOutbox skips its oversize check entirely against
// such servers, preserving today's behavior.
func (c *Client) MaxBlobSize() int64 {
	if caps := c.caps.Load(); caps != nil {
		return caps.Capabilities.MaxBlobSize
	}
	return 0
}

// ServerEpoch returns the server's database-lifetime identity from the
// capabilities handshake, or "" before the handshake / against a pre-epoch
// server. CheckServerEpoch compares it with the persisted last-seen epoch to
// detect a recreated server.
func (c *Client) ServerEpoch() string {
	if caps := c.caps.Load(); caps != nil {
		return caps.ServerEpoch
	}
	return ""
}

// SupportsBlobs reports whether the server supports the blob tier.
func (c *Client) SupportsBlobs() bool {
	if caps := c.caps.Load(); caps != nil {
		return caps.Capabilities.Blobs
	}
	return false
}

// NewClientFromConfig constructs a Client from a SyncConfig. Device auth is
// attempted before a legacy credential is resolved, so a stale token command
// cannot block or leak through an otherwise valid v2 startup.
func NewClientFromConfig(ctx context.Context, cfg *config.SyncConfig, deviceID, originID, clientVersion string, logger *logging.UnifiedLogger) (*Client, error) {
	if cfg.Server == "" {
		return nil, fmt.Errorf("sync server URL not configured")
	}

	var signer DeviceSigner
	var err error
	if keyPath := devicekey.Path(); keyPath != "" {
		if _, statErr := os.Lstat(keyPath); statErr == nil {
			signer, err = devicekey.Load()
			if err != nil {
				return nil, fmt.Errorf("failed to load device key: %w", err)
			}
		} else if !os.IsNotExist(statErr) {
			return nil, fmt.Errorf("failed to inspect device key: %w", statErr)
		}
	}
	tlsCfg, err := cfg.TLSClientConfig()
	if err != nil {
		return nil, err
	}

	client := NewClient(ClientConfig{
		ServerURL: cfg.Server,
		DeviceID:  deviceID,
		OriginID:  originID,
		Signer:    signer,
		Logger:    logger,
		TLSConfig: tlsCfg,
	})

	if signer != nil {
		if _, deviceErr := client.deviceCapabilities(ctx, clientVersion); deviceErr == nil {
			return client, nil
		} else {
			token, tokenErr := cfg.ResolveToken()
			if tokenErr != nil {
				return nil, fmt.Errorf("legacy sync credential resolution failed after device authentication was unavailable: %w", tokenErr)
			}
			if token == "" {
				return nil, fmt.Errorf("capabilities handshake failed: %w", deviceErr)
			}
			client.staticToken = token
			client.setBearer(token, false)
			if _, legacyErr := client.legacyCapabilities(ctx, clientVersion); legacyErr != nil {
				return nil, fmt.Errorf("capabilities handshake failed: device authentication unavailable and legacy fallback failed: %w", legacyErr)
			}
			return client, nil
		}
	}

	token, err := cfg.ResolveToken()
	if err != nil {
		return nil, fmt.Errorf("failed to resolve legacy sync credential: %w", err)
	}
	if token == "" {
		return nil, fmt.Errorf("sync authentication not configured: no device key or legacy token")
	}
	client.staticToken = token
	client.setBearer(token, false)
	if _, err := client.legacyCapabilities(ctx, clientVersion); err != nil {
		return nil, fmt.Errorf("capabilities handshake failed: %w", err)
	}
	return client, nil
}

// PullEvents fetches a batch of events from the notespace event log, starting from the given cursor.
// It uses long-polling if wait is set to > 0 seconds.
func (c *Client) PullEvents(ctx context.Context, notespace string, cursor int64, limit int, wait time.Duration) (*syncproto.PullResponse, error) {
	req := &syncproto.PullRequest{
		ProtocolVersion: syncproto.ProtocolVersionNotespaceID,
		NotespaceID:     syncproto.NotespaceID(notespace),
		Cursor:          cursor,
		Limit:           limit,
	}
	if wait > 0 {
		req.Wait = wait.String()
	}

	waitStr := ""
	if wait > 0 {
		waitStr = wait.String()
	}
	httpReq, err := c.newRequest(ctx, "GET", fmt.Sprintf("/sync/events?protocol_version=%d&notespace=%s&cursor=%d&limit=%d&wait=%s&origin_id=%s&exclude_origin=%s",
		syncproto.ProtocolVersionNotespaceID, neturl.QueryEscape(notespace), cursor, limit, waitStr, neturl.QueryEscape(c.originID), neturl.QueryEscape(c.originID)), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create pull request: %w", err)
	}

	resp, err := c.doAuthenticated(ctx, c.pollClient, "pull request", replayableRequest(httpReq))
	if err != nil {
		return nil, fmt.Errorf("pull request failed: %w", err)
	}
	defer resp.Body.Close()

	// 410 Gone is a protocol answer, not a transport failure: the cursor
	// predates the notespace's GC watermark and the body is a decodable
	// PullResponse carrying snapshot_required=true (C20). Erroring here made
	// RunPullLoop's resync branch unreachable — a wiped client spun on
	// "pull failed ... status 410" forever instead of snapshot-resyncing.
	// Every other non-200 stays an error.
	if err := c.rejectIfUnauthorized("pull request", resp); err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusGone {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("pull request failed with status %d: %s", resp.StatusCode, string(body))
	}

	var pullResp syncproto.PullResponse
	if err := json.NewDecoder(resp.Body).Decode(&pullResp); err != nil {
		return nil, fmt.Errorf("failed to decode pull response: %w", err)
	}

	return &pullResp, nil
}

// FetchBlob fetches a blob by its SHA-256 hash.
// FetchBlob retrieves a document blob and verifies it end-to-end. Blob
// contract v1: payload is the raw content, addressed by sha256(payload).
func (c *Client) FetchBlob(ctx context.Context, hash string) ([]byte, error) {
	httpReq, err := c.newRequest(ctx, "GET", fmt.Sprintf("/sync/blob/%s", hash), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create blob fetch request: %w", err)
	}

	resp, err := c.doAuthenticated(ctx, c.httpClient, "blob fetch", replayableRequest(httpReq))
	if err != nil {
		return nil, fmt.Errorf("blob fetch failed: %w", err)
	}
	defer resp.Body.Close()

	if err := c.rejectIfUnauthorized("blob fetch", resp); err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("blob fetch failed with status %d: %s", resp.StatusCode, string(body))
	}

	content, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read blob content: %w", err)
	}

	// End-to-end integrity: the blob key IS the sha256 of the payload.
	sum := sha256.Sum256(content)
	if got := hex.EncodeToString(sum[:]); got != hash {
		return nil, fmt.Errorf("blob integrity check failed: got %s want %s", got, hash)
	}
	return content, nil
}

// HistoryEntry is one version row from the server's /sync/history endpoint.
type HistoryEntry struct {
	Seq        int64  `json:"seq"`
	Version    int64  `json:"version"`
	Actor      string `json:"actor"`
	ReceivedAt string `json:"received_at"`
}

// History returns the descending version history for a document path.
func (c *Client) History(ctx context.Context, notespace, path string) ([]HistoryEntry, error) {
	u := fmt.Sprintf("%s/sync/history?protocol_version=%d&notespace=%s&path=%s",
		c.serverURL, syncproto.ProtocolVersionNotespaceID, neturl.QueryEscape(notespace), neturl.QueryEscape(path))

	httpReq, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create history request: %w", err)
	}
	resp, err := c.doAuthenticated(ctx, c.httpClient, "history request", replayableRequest(httpReq))
	if err != nil {
		return nil, fmt.Errorf("history request failed: %w", err)
	}
	defer resp.Body.Close()

	if err := c.rejectIfUnauthorized("history request", resp); err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("history request failed with status %d: %s", resp.StatusCode, string(body))
	}

	var entries []HistoryEntry
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		return nil, fmt.Errorf("failed to decode history response: %w", err)
	}
	return entries, nil
}

// HistoryBlob returns the raw content of a document at a specific version.
func (c *Client) HistoryBlob(ctx context.Context, notespace, documentID string, version int64) ([]byte, error) {
	u := fmt.Sprintf("%s/sync/history/blob?protocol_version=%d&notespace=%s&document_id=%s&version=%d",
		c.serverURL, syncproto.ProtocolVersionNotespaceID, neturl.QueryEscape(notespace), neturl.QueryEscape(documentID), version)

	httpReq, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create history blob request: %w", err)
	}
	resp, err := c.doAuthenticated(ctx, c.httpClient, "history blob request", replayableRequest(httpReq))
	if err != nil {
		return nil, fmt.Errorf("history blob request failed: %w", err)
	}
	defer resp.Body.Close()

	if err := c.rejectIfUnauthorized("history blob request", resp); err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("version %d not found for document %s", version, documentID)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("history blob request failed with status %d: %s", resp.StatusCode, string(body))
	}

	return io.ReadAll(resp.Body)
}
