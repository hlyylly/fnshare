package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

// Client is a thin HTTP wrapper used by the CLI to drive a running daemon.
type Client struct {
	base string
	hc   *http.Client
}

func NewClient(addr string) *Client {
	if addr == "" {
		addr = "127.0.0.1:4101"
	}
	return &Client{
		base: "http://" + addr,
		// No request timeout — file uploads/downloads are unbounded.
		hc: &http.Client{},
	}
}

// Reachable returns true if the daemon's API socket is accepting connections.
// Used by CLI commands to decide between API path and direct DB fallback.
func (c *Client) Reachable(ctx context.Context) bool {
	addr := c.base[len("http://"):]
	dialer := net.Dialer{Timeout: 500 * time.Millisecond}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func (c *Client) Status(ctx context.Context) (*StatusResponse, error) {
	var out StatusResponse
	if err := c.do(ctx, http.MethodGet, "/v1/status", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) Invite(ctx context.Context, req InviteRequest) (*InviteResponse, error) {
	var out InviteResponse
	if err := c.do(ctx, http.MethodPost, "/v1/invite", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) ListFiles(ctx context.Context) (*FilesResponse, error) {
	var out FilesResponse
	if err := c.do(ctx, http.MethodGet, "/v1/files", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Upload streams body to /v1/files. mode is "shared" (default) or "private";
// groupID is optional if the node is in exactly one group.
func (c *Client) Upload(ctx context.Context, filename string, body io.Reader, size int64, mode, groupID string) ([]byte, error) {
	u := c.base + "/v1/files?name=" + url.QueryEscape(filename)
	if mode != "" {
		u += "&mode=" + url.QueryEscape(mode)
	}
	if groupID != "" {
		u += "&group=" + url.QueryEscape(groupID)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, body)
	if err != nil {
		return nil, err
	}
	if size > 0 {
		req.ContentLength = size
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("upload: %s — %s", resp.Status, string(raw))
	}
	return raw, nil
}

// Download streams /v1/files/{id}/content into w.
func (c *Client) Download(ctx context.Context, fileID string, w io.Writer) error {
	u := c.base + "/v1/files/" + url.PathEscape(fileID) + "/content"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("download: %s — %s", resp.Status, string(raw))
	}
	_, err = io.Copy(w, resp.Body)
	return err
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		var er ErrorResponse
		_ = json.NewDecoder(resp.Body).Decode(&er)
		if er.Error != "" {
			return fmt.Errorf("%s — %s", resp.Status, er.Error)
		}
		return fmt.Errorf("api error: %s", resp.Status)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}
