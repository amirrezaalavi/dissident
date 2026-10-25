package qualify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync"
	"time"

	"dissident/internal/platform"
)

// maxRealBody bounds the body bytes read during the real HTTP stage.
const maxRealBody = 64 << 10

// fetchResult is the observable outcome of one HTTP GET.
type fetchResult struct {
	Status int
	TTFB   time.Duration
	Bytes  int64
}

// httpClient builds a client that dials every request through socksAddr.
func (t *Tester) httpClient(socksAddr string) *http.Client {
	dialer := &platform.SOCKS5Dialer{Addr: socksAddr, Timeout: t.test.HTTPTimeout}
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          8,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   t.test.TLSTimeout,
		ExpectContinueTimeout: time.Second,
	}
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			return nil
		},
	}
}

// get performs one GET through the client, measures TTFB with httptrace, and
// optionally reads up to readLimit body bytes. Redirects are followed.
func (t *Tester) get(ctx context.Context, client *http.Client, rawURL string, readLimit int64) (fetchResult, error) {
	requestCtx, cancel := context.WithTimeout(ctx, t.test.HTTPTimeout)
	defer cancel()

	start := time.Now()
	var once sync.Once
	var firstByte time.Time
	trace := &httptrace.ClientTrace{
		GotFirstResponseByte: func() {
			once.Do(func() { firstByte = time.Now() })
		},
	}
	request, err := http.NewRequestWithContext(httptrace.WithClientTrace(requestCtx, trace), http.MethodGet, rawURL, nil)
	if err != nil {
		return fetchResult{}, fmt.Errorf("request %s: %w", rawURL, err)
	}
	request.Header.Set("User-Agent", "dissident/1.0")
	request.Header.Set("Accept", "*/*")

	response, err := client.Do(request)
	if err != nil {
		return fetchResult{}, err
	}
	defer response.Body.Close()

	result := fetchResult{Status: response.StatusCode}
	if firstByte.IsZero() {
		result.TTFB = time.Since(start)
	} else {
		result.TTFB = firstByte.Sub(start)
	}
	if readLimit > 0 {
		written, err := io.Copy(io.Discard, io.LimitReader(response.Body, readLimit))
		result.Bytes = written
		if err != nil {
			return result, fmt.Errorf("read %s: %w", rawURL, err)
		}
	}
	return result, nil
}

func is2xx(status int) bool {
	return status >= 200 && status <= 299
}

// verifyOne performs a 2xx verify GET for one URL, optionally reading up to
// readLimit body bytes into the result.
func (t *Tester) verifyOne(ctx context.Context, client *http.Client, rawURL string, readLimit int64) (fetchResult, error) {
	result, err := t.get(ctx, client, rawURL, readLimit)
	if err != nil {
		return result, err
	}
	if !is2xx(result.Status) {
		return result, fmt.Errorf("%s: HTTP %d", rawURL, result.Status)
	}
	return result, nil
}

// verifyAny tries verify URLs in order and returns the first 2xx response.
func (t *Tester) verifyAny(ctx context.Context, client *http.Client, readLimit int64) (fetchResult, error) {
	if len(t.urls.Verify) == 0 {
		return fetchResult{}, errors.New("no verify URLs configured")
	}
	var failures []string
	for _, rawURL := range t.urls.Verify {
		result, err := t.verifyOne(ctx, client, rawURL, readLimit)
		if err == nil {
			return result, nil
		}
		failures = append(failures, err.Error())
	}
	return fetchResult{}, fmt.Errorf("all verify URLs failed (last: %s)", failures[len(failures)-1])
}

// Verify requires the first verify URL to return 2xx through the SOCKS proxy,
// following redirects. The body is closed.
func (t *Tester) Verify(ctx context.Context, socksAddr string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(socksAddr) == "" {
		return errors.New("empty SOCKS address")
	}
	if len(t.urls.Verify) == 0 {
		return errors.New("no verify URLs configured")
	}
	client := t.httpClient(socksAddr)
	defer client.CloseIdleConnections()
	_, err := t.verifyOne(ctx, client, t.urls.Verify[0], 0)
	return err
}

// Probe requires at least one probe URL to return 2xx through the SOCKS proxy,
// following redirects. Bodies are closed.
func (t *Tester) Probe(ctx context.Context, socksAddr string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(socksAddr) == "" {
		return errors.New("empty SOCKS address")
	}
	if len(t.urls.Probe) == 0 {
		return errors.New("no probe URLs configured")
	}
	client := t.httpClient(socksAddr)
	defer client.CloseIdleConnections()

	var failures []string
	for _, rawURL := range t.urls.Probe {
		result, err := t.get(ctx, client, rawURL, 0)
		if err == nil && is2xx(result.Status) {
			return nil
		}
		if err != nil {
			failures = append(failures, err.Error())
		} else {
			failures = append(failures, fmt.Sprintf("%s: HTTP %d", rawURL, result.Status))
		}
	}
	return fmt.Errorf("all probe URLs failed (last: %s)", failures[len(failures)-1])
}

// Speed downloads up to test.speed_size bytes through the SOCKS proxy,
// measuring Mbps and TTFB. It tries urls.speed, then urls.speed_fallback on a
// transport error, non-2xx status, or empty body.
func (t *Tester) Speed(ctx context.Context, socksAddr string) (mbps float64, ttfb time.Duration, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(socksAddr) == "" {
		return 0, 0, errors.New("empty SOCKS address")
	}
	urls := make([]string, 0, 2)
	if speed := strings.TrimSpace(t.urls.Speed); speed != "" {
		urls = append(urls, speed)
	}
	if fallback := strings.TrimSpace(t.urls.SpeedFallback); fallback != "" && fallback != t.urls.Speed {
		urls = append(urls, fallback)
	}
	if len(urls) == 0 {
		return 0, 0, errors.New("no speed URLs configured")
	}

	var failures []string
	for _, rawURL := range urls {
		value, firstByte, err := t.speedOne(ctx, socksAddr, rawURL)
		if err == nil {
			return value, firstByte, nil
		}
		failures = append(failures, err.Error())
	}
	return 0, 0, fmt.Errorf("all speed URLs failed (last: %s)", failures[len(failures)-1])
}

// speedOne measures one URL. The per-request timeout doubles for a 4 MB
// transfer so slow links can finish.
func (t *Tester) speedOne(ctx context.Context, socksAddr, rawURL string) (float64, time.Duration, error) {
	client := t.httpClient(socksAddr)
	defer client.CloseIdleConnections()

	size := t.test.SpeedSize
	timeout := t.test.HTTPTimeout
	if size >= 4_000_000 {
		timeout *= 2
	}
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	var once sync.Once
	var firstByte time.Time
	trace := &httptrace.ClientTrace{
		GotFirstResponseByte: func() {
			once.Do(func() { firstByte = time.Now() })
		},
	}
	request, err := http.NewRequestWithContext(httptrace.WithClientTrace(requestCtx, trace), http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("request %s: %w", rawURL, err)
	}
	request.Header.Set("User-Agent", "dissident/1.0")
	request.Header.Set("Accept", "*/*")
	request.Header.Set("Accept-Encoding", "identity")

	response, err := client.Do(request)
	if err != nil {
		return 0, 0, err
	}
	defer response.Body.Close()

	measuredTTFB := time.Since(start)
	if !firstByte.IsZero() {
		measuredTTFB = firstByte.Sub(start)
	}
	if !is2xx(response.StatusCode) {
		return 0, measuredTTFB, fmt.Errorf("%s: HTTP %d", rawURL, response.StatusCode)
	}
	read, err := io.Copy(io.Discard, io.LimitReader(response.Body, size))
	if err != nil {
		return 0, measuredTTFB, fmt.Errorf("%s: read body: %w", rawURL, err)
	}
	if read <= 0 {
		return 0, measuredTTFB, fmt.Errorf("%s: empty body", rawURL)
	}
	elapsed := time.Since(start)
	if elapsed <= 0 {
		return 0, measuredTTFB, errors.New("zero elapsed time")
	}
	return float64(read) * 8 / elapsed.Seconds() / 1e6, measuredTTFB, nil
}

// contentCheck counts content URLs returning 2xx with at least
// test.content_min_bytes body bytes. It returns the number of successes, the
// number of URLs, and the largest body read.
func (t *Tester) contentCheck(ctx context.Context, client *http.Client) (pass, total int, bytes int64, err error) {
	total = len(t.urls.Content)
	if total == 0 {
		return 0, 0, 0, errors.New("no content URLs configured")
	}
	minimum := int64(t.test.ContentMinBytes)
	for _, rawURL := range t.urls.Content {
		result, err := t.get(ctx, client, rawURL, minimum)
		if err != nil {
			t.log.Debug("content URL failed", "url", rawURL, "err", err)
			continue
		}
		if is2xx(result.Status) && result.Bytes >= minimum {
			pass++
			if result.Bytes > bytes {
				bytes = result.Bytes
			}
		}
	}
	return pass, total, bytes, nil
}
