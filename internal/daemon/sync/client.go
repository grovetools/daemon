// Package sync provides the client-side state machine and HTTP client
// for the notebook sync protocol's Phase 1 push pipeline.
package sync

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"strings"
	gosync "sync"
	"sync/atomic"
	"time"

	"github.com/grovetools/core/config"
	"github.com/grovetools/core/logging"
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
// it recognizes, for a user with no grant on this workspace (getUserPrefixes,
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

// ClientConfig wraps the configuration needed to initialize a sync client.
type ClientConfig struct {
	ServerURL  string
	Token      string
	DeviceID   string
	OriginID   string
	Logger     *logging.UnifiedLogger
	HTTPClient *http.Client
}

// Client is an HTTP wrapper for grove-syncd push/pull endpoints.
type Client struct {
	serverURL  string
	token      string
	deviceID   string
	originID   string
	log        *logging.UnifiedLogger
	httpClient *http.Client
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
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{
			Timeout: 30 * time.Second,
		}
	}
	pollClient := &http.Client{Timeout: 90 * time.Second}
	return &Client{
		serverURL:  cfg.ServerURL,
		token:      cfg.Token,
		deviceID:   cfg.DeviceID,
		originID:   cfg.OriginID,
		log:        cfg.Logger,
		httpClient: cfg.HTTPClient,
		pollClient: pollClient,
	}
}

// Capabilities performs the handshake with the server, caching the response
// for future blob-size decisions.
func (c *Client) Capabilities(ctx context.Context, clientVersion string) (*syncproto.CapabilitiesResponse, error) {
	req := &syncproto.CapabilitiesRequest{
		ClientName:       "groved",
		ClientVersion:    clientVersion,
		ProtocolVersions: []int{syncproto.ProtocolVersion},
		OriginID:         c.originID,
		DeviceID:         c.deviceID,
	}

	httpReq, err := c.newRequest(ctx, "POST", "/sync/capabilities", req)
	if err != nil {
		return nil, fmt.Errorf("failed to create capabilities request: %w", err)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("capabilities request failed: %w", err)
	}
	defer resp.Body.Close()

	if err := c.rejectIfUnauthorized("capabilities request", resp); err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("capabilities request failed with status %d: %s", resp.StatusCode, string(body))
	}

	var capResp syncproto.CapabilitiesResponse
	if err := json.NewDecoder(resp.Body).Decode(&capResp); err != nil {
		return nil, fmt.Errorf("failed to decode capabilities response: %w", err)
	}

	if capResp.Error != "" {
		return nil, fmt.Errorf("server capabilities error: %s", capResp.Error)
	}

	if !capResp.Capabilities.SupportsVersion(syncproto.ProtocolVersion) {
		return nil, fmt.Errorf("server does not support protocol version %d", syncproto.ProtocolVersion)
	}

	c.caps.Store(&capResp)
	return &capResp, nil
}

// Push uploads a batch of outbox entries to the server. Returns the
// per-event results in the same order as the input events.
func (c *Client) Push(ctx context.Context, workspace string, events []syncproto.SyncEvent) (*syncproto.PushResponse, error) {
	if c.caps.Load() == nil {
		return nil, fmt.Errorf("capabilities handshake not performed; call Capabilities() first")
	}

	req := &syncproto.PushRequest{
		Workspace: workspace,
		OriginID:  c.originID,
		DeviceID:  c.deviceID,
		Events:    events,
	}

	httpReq, err := c.newRequest(ctx, "POST", "/sync/push", req)
	if err != nil {
		return nil, fmt.Errorf("failed to create push request: %w", err)
	}

	resp, err := c.httpClient.Do(httpReq)
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

	if pushResp.Error != "" {
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

	httpReq.Header.Set("Authorization", fmt.Sprintf("Bearer %s", c.token))
	httpReq.Header.Set("Content-Type", "application/octet-stream")

	resp, err := c.httpClient.Do(httpReq)
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

// Snapshot fetches the server's manifest snapshot for a workspace.
func (c *Client) Snapshot(ctx context.Context, workspace string) (*syncproto.SnapshotManifest, error) {
	url := fmt.Sprintf("%s/sync/snapshot?workspace=%s", c.serverURL, workspace)

	httpReq, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create snapshot request: %w", err)
	}

	httpReq.Header.Set("Authorization", fmt.Sprintf("Bearer %s", c.token))

	resp, err := c.httpClient.Do(httpReq)
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

// newRequest is a helper that constructs an authenticated HTTP request with
// a JSON body.
func (c *Client) newRequest(ctx context.Context, method, path string, body interface{}) (*http.Request, error) {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(body); err != nil {
		return nil, err
	}

	url := fmt.Sprintf("%s%s", c.serverURL, path)
	req, err := http.NewRequestWithContext(ctx, method, url, &buf)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", c.token))
	req.Header.Set("Content-Type", "application/json")
	return req, nil
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

// NewClientFromConfig constructs a Client from a SyncConfig, resolving the
// token and validating the server URL.
func NewClientFromConfig(ctx context.Context, cfg *config.SyncConfig, deviceID, originID, clientVersion string, logger *logging.UnifiedLogger) (*Client, error) {
	if cfg.Server == "" {
		return nil, fmt.Errorf("sync server URL not configured")
	}

	token, err := cfg.ResolveToken()
	if err != nil {
		return nil, fmt.Errorf("failed to resolve sync token: %w", err)
	}

	if token == "" {
		return nil, fmt.Errorf("sync token not configured")
	}

	client := NewClient(ClientConfig{
		ServerURL: cfg.Server,
		Token:     token,
		DeviceID:  deviceID,
		OriginID:  originID,
		Logger:    logger,
	})

	// Perform handshake
	if _, err := client.Capabilities(ctx, clientVersion); err != nil {
		return nil, fmt.Errorf("capabilities handshake failed: %w", err)
	}

	return client, nil
}

// PullEvents fetches a batch of events from the workspace event log, starting from the given cursor.
// It uses long-polling if wait is set to > 0 seconds.
func (c *Client) PullEvents(ctx context.Context, workspace string, cursor int64, limit int, wait time.Duration) (*syncproto.PullResponse, error) {
	req := &syncproto.PullRequest{
		Workspace: workspace,
		Cursor:    cursor,
		Limit:     limit,
	}
	if wait > 0 {
		req.Wait = wait.String()
	}

	waitStr := ""
	if wait > 0 {
		waitStr = wait.String()
	}
	httpReq, err := c.newRequest(ctx, "GET", fmt.Sprintf("/sync/events?workspace=%s&cursor=%d&limit=%d&wait=%s&origin_id=%s&exclude_origin=%s",
		neturl.QueryEscape(workspace), cursor, limit, waitStr, neturl.QueryEscape(c.originID), neturl.QueryEscape(c.originID)), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create pull request: %w", err)
	}

	resp, err := c.pollClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("pull request failed: %w", err)
	}
	defer resp.Body.Close()

	// 410 Gone is a protocol answer, not a transport failure: the cursor
	// predates the workspace's GC watermark and the body is a decodable
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

	resp, err := c.httpClient.Do(httpReq)
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
func (c *Client) History(ctx context.Context, workspace, path string) ([]HistoryEntry, error) {
	u := fmt.Sprintf("%s/sync/history?workspace=%s&path=%s",
		c.serverURL, neturl.QueryEscape(workspace), neturl.QueryEscape(path))

	httpReq, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create history request: %w", err)
	}
	httpReq.Header.Set("Authorization", fmt.Sprintf("Bearer %s", c.token))

	resp, err := c.httpClient.Do(httpReq)
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
func (c *Client) HistoryBlob(ctx context.Context, workspace, documentID string, version int64) ([]byte, error) {
	u := fmt.Sprintf("%s/sync/history/blob?workspace=%s&document_id=%s&version=%d",
		c.serverURL, neturl.QueryEscape(workspace), neturl.QueryEscape(documentID), version)

	httpReq, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create history blob request: %w", err)
	}
	httpReq.Header.Set("Authorization", fmt.Sprintf("Bearer %s", c.token))

	resp, err := c.httpClient.Do(httpReq)
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
