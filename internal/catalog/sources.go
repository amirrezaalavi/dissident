package catalog

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"dissident/internal/contract"
	"dissident/internal/uri"
)

// maxSourceBytes caps one fetched subscription body at 8 MiB.
const maxSourceBytes = 8 << 20

// maxSourceLine bounds one line of the sources file. It matches the fetch
// limit so a single-line base64 subscription is not truncated.
const maxSourceLine = maxSourceBytes + 1024

// defaultSourceTimeout bounds one subscription fetch when the caller passes a
// non-positive timeout.
const defaultSourceTimeout = 30 * time.Second

// ParseSources parses a subscription payload or newline-separated link list.
// It is uri.ParseMany: good links are returned, and every bad line is
// reported separately with its line number.
func ParseSources(data []byte) ([]contract.Link, []error) {
	return uri.ParseMany(data)
}

// LoadSources reads path, where each non-empty, non-comment line is either an
// http(s) subscription URL or a share link. Subscriptions are fetched with
// timeout (redirects followed, at most 8 MiB), parsed, and deduplicated
// globally by link hash. Bad sources and bad lines are appended to errs and
// do not discard good links.
//
// A read failure, or a file with at least one source attempt but no usable
// link, returns an error.
func LoadSources(ctx context.Context, path string, timeout time.Duration) ([]contract.Link, []error, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("catalog: read sources %s: %w", path, err)
	}

	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64*1024), maxSourceLine)

	var (
		links    []contract.Link
		problems []error
		attempts int
		seen     = make(map[string]bool)
	)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(strings.TrimPrefix(scanner.Text(), "\ufeff"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		attempts++

		if isHTTPSource(line) {
			body, err := fetchSource(ctx, line, timeout)
			if err != nil {
				problems = append(problems, fmt.Errorf("line %d: source %s: %w", lineNumber, line, err))
				continue
			}
			found, parseProblems := ParseSources(body)
			for _, problem := range parseProblems {
				problems = append(problems, fmt.Errorf("line %d: source %s: %w", lineNumber, line, problem))
			}
			for _, link := range found {
				link = withHash(link)
				if link.Hash == "" || seen[link.Hash] {
					continue
				}
				seen[link.Hash] = true
				links = append(links, link)
			}
			continue
		}

		link, err := uri.Parse(line)
		if err != nil {
			problems = append(problems, fmt.Errorf("line %d: %w", lineNumber, err))
			continue
		}
		if seen[link.Hash] {
			continue
		}
		seen[link.Hash] = true
		links = append(links, link)
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, fmt.Errorf("catalog: read sources %s: %w", path, err)
	}
	if len(links) == 0 && attempts > 0 {
		return nil, problems, fmt.Errorf("catalog: %s produced no usable links", path)
	}
	return links, problems, nil
}

// isHTTPSource reports whether a line should be fetched as a subscription.
func isHTTPSource(line string) bool {
	lower := strings.ToLower(line)
	return strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://")
}

// fetchSource downloads one subscription, enforcing the scheme, timeout, and
// body-size limits.
func fetchSource(ctx context.Context, rawURL string, timeout time.Duration) ([]byte, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %w", err)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
	default:
		return nil, fmt.Errorf("unsupported URL scheme %q", parsed.Scheme)
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("URL has no host")
	}
	if timeout <= 0 {
		timeout = defaultSourceTimeout
	}
	client := &http.Client{Timeout: timeout}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", "dissident/1.0")
	request.Header.Set("Accept", "*/*")
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return nil, fmt.Errorf("HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxSourceBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if len(body) > maxSourceBytes {
		return nil, fmt.Errorf("body exceeds %d bytes", maxSourceBytes)
	}
	return body, nil
}
