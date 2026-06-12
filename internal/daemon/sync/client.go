// Package sync provides the client-side state machine and HTTP client
// for the notebook sync protocol's Phase 1 push pipeline.
package sync

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"time"

	"github.com/grovetools/core/config"
	"github.com/grovetools/core/logging"
	"github.com/grovetools/core/pkg/syncproto"
)

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
	caps       *syncproto.CapabilitiesResponse // Cached from handshake
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

	c.caps = &capResp
	return &capResp, nil
}

// Push uploads a batch of outbox entries to the server. Returns the
// per-event results in the same order as the input events.
func (c *Client) Push(ctx context.Context, workspace string, events []syncproto.SyncEvent) (*syncproto.PushResponse, error) {
	if c.caps == nil {
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

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("push request failed: unauthorized (invalid token)")
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

	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("blob upload failed: unauthorized (invalid token)")
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

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("snapshot request failed: unauthorized (invalid token)")
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
	if c.caps != nil && c.caps.Capabilities.MaxInlineSize > 0 {
		return c.caps.Capabilities.MaxInlineSize
	}
	return 256 * 1024 // 256KB default
}

// SupportsBlobs reports whether the server supports the blob tier.
func (c *Client) SupportsBlobs() bool {
	if c.caps != nil {
		return c.caps.Capabilities.Blobs
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

	if resp.StatusCode != http.StatusOK {
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

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("blob fetch failed with status %d: %s", resp.StatusCode, string(body))
	}

	content, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read blob content: %w", err)
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

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("version %d not found for document %s", version, documentID)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("history blob request failed with status %d: %s", resp.StatusCode, string(body))
	}

	return io.ReadAll(resp.Body)
}
