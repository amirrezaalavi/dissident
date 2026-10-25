package collector

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client is a small HTTP helper with a fixed timeout, used for polling the
// daemons. No external deps.
type Client struct {
	hc      *http.Client
	timeout time.Duration
}

func NewClient(timeout time.Duration) *Client {
	return &Client{
		hc:      &http.Client{},
		timeout: timeout,
	}
}

// Get fetches a URL and returns the body. A non-2xx status is an error.
func (c *Client) Get(ctx context.Context, url string) ([]byte, error) {
	reqCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", url, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MiB cap
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", url, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("get %s: status %d", url, resp.StatusCode)
	}
	return body, nil
}

// GetRaw fetches a URL and returns the raw response (body, status code, error).
// Unlike Get, it does not treat non-2xx as an error so callers can proxy the
// upstream status through.
func (c *Client) GetRaw(ctx context.Context, url string) ([]byte, int, error) {
	reqCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("build request: %w", err)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("get %s: %w", url, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MiB cap
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read %s: %w", url, err)
	}
	return body, resp.StatusCode, nil
}

// Post sends an empty POST and returns the raw response (body, status code, error).
func (c *Client) Post(ctx context.Context, url string) ([]byte, int, error) {
	reqCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("build request: %w", err)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("post %s: %w", url, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MiB cap
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read %s: %w", url, err)
	}
	return body, resp.StatusCode, nil
}
