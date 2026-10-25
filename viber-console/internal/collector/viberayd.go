package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ViberaydClient polls the Viberayd management API (JSON endpoints).
type ViberaydClient struct {
	client  *Client
	apiURL  string
	subURL  string
}

func NewViberaydClient(apiURL, subURL string, timeout time.Duration) *ViberaydClient {
	return &ViberaydClient{
		client: NewClient(timeout),
		apiURL: apiURL,
		subURL: subURL,
	}
}

// ViberaydStats mirrors GET /api/stats.
type ViberaydStats struct {
	Total       int    `json:"total"`
	Working     int    `json:"working"`
	Failed      int    `json:"failed"`
	Unreachable int    `json:"unreachable"`
	UpdatedAt   string `json:"updated_at"`
}

// ViberaydConfig mirrors one ConfigEntry from GET /api/configs.
type ViberaydConfig struct {
	Raw          string `json:"raw"`
	Host         string `json:"host"`
	Port         int    `json:"port"`
	Protocol     string `json:"protocol"`
	SourceURL    string `json:"source_url"`
	FirstSeen    string `json:"first_seen"`
	LastTested   string `json:"last_tested"`
	LastSuccess  string `json:"last_success"`
	SuccessCount int    `json:"success_count"`
	FailCount    int    `json:"fail_count"`
	State        string `json:"state"`
	LatencyMs    int    `json:"latency_ms"`
}

// ViberaydConfigPage mirrors GET /api/configs.
type ViberaydConfigPage struct {
	Page    int              `json:"page"`
	PerPage int              `json:"per_page"`
	Total   int              `json:"total"`
	Configs []ViberaydConfig `json:"configs"`
}

// FetchStats returns the current stats snapshot. On error it returns the
// error; the caller marks the daemon unreachable.
func (v *ViberaydClient) FetchStats(ctx context.Context) (ViberaydStats, error) {
	body, err := v.client.Get(ctx, v.apiURL+"/api/stats")
	if err != nil {
		return ViberaydStats{}, err
	}
	var s ViberaydStats
	if err := json.Unmarshal(body, &s); err != nil {
		return ViberaydStats{}, err
	}
	return s, nil
}

// FetchConfigs fetches the config table. stateFilter (optional) is applied
// backend-side; empty string returns all.
func (v *ViberaydClient) FetchConfigs(ctx context.Context, page, perPage int, stateFilter string) (ViberaydConfigPage, error) {
	url := v.apiURL + "/api/configs?page=" + strconv.Itoa(page) + "&per_page=" + strconv.Itoa(perPage)
	body, err := v.client.Get(ctx, url)
	if err != nil {
		return ViberaydConfigPage{}, err
	}
	var p ViberaydConfigPage
	if err := json.Unmarshal(body, &p); err != nil {
		return ViberaydConfigPage{}, err
	}
	if stateFilter != "" && len(p.Configs) > 0 {
		filtered := p.Configs[:0]
		for _, c := range p.Configs {
			if c.State == stateFilter {
				filtered = append(filtered, c)
			}
		}
		p.Configs = filtered
	}
	return p, nil
}

// FetchURLs returns the subscription URL list.
func (v *ViberaydClient) FetchURLs(ctx context.Context) ([]string, error) {
	body, err := v.client.Get(ctx, v.apiURL+"/api/urls")
	if err != nil {
		return nil, err
	}
	var urls []string
	if err := json.Unmarshal(body, &urls); err != nil {
		return nil, err
	}
	return urls, nil
}

// ReplaceURLs sets the subscription URL list to exactly the given lines by
// diffing against the current list: DELETE lines that are gone, POST lines
// that are new. This works against the stock viberayd API (no replace
// endpoint needed upstream). Returns the final list as seen after the diff.
func (v *ViberaydClient) ReplaceURLs(ctx context.Context, want []string) ([]string, error) {
	current, err := v.FetchURLs(ctx)
	if err != nil {
		return nil, err
	}

	wantSet := map[string]bool{}
	for _, u := range want {
		wantSet[u] = true
	}
	curSet := map[string]bool{}
	for _, u := range current {
		curSet[u] = true
	}

	// DELETE removed lines (by 1-indexed line number, per viberayd API).
	for i, u := range current {
		if !wantSet[u] {
			_, err := v.client.Get(ctx, v.apiURL+"/api/urls/"+strconv.Itoa(i+1))
			if err != nil {
				return nil, err
			}
		}
	}

	// POST added lines (append one at a time).
	for _, u := range want {
		if !curSet[u] {
			body, err := json.Marshal(map[string]string{"url": u})
			if err != nil {
				return nil, err
			}
			if err := v.postJSON(ctx, v.apiURL+"/api/urls", body); err != nil {
				return nil, err
			}
		}
	}

	return v.FetchURLs(ctx)
}

// postJSON issues a POST with a JSON body; non-2xx is an error.
func (v *ViberaydClient) postJSON(ctx context.Context, url string, body []byte) error {
	return v.doJSON(ctx, "POST", url, body)
}

func (v *ViberaydClient) doJSON(ctx context.Context, method, url string, body []byte) error {
	reqCtx, cancel := context.WithTimeout(ctx, v.client.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, method, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := v.client.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s %s: status %d: %s", method, url, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

// SubURL returns the subscription endpoint for clients (informational).
func (v *ViberaydClient) SubURL() string { return v.subURL }
