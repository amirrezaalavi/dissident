// Package qualify runs dissident's strict staged qualification pipeline
// against a contract.Link. A candidate must pass static support, TCP, TLS
// (when applicable), credential validation, render, xray preflight, a real
// HTTP request through its own xray, content quorum, speed/TTFB, and
// stability before it can be promoted.
package qualify

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"time"

	"dissident/internal/config"
	"dissident/internal/contract"
	"dissident/internal/platform"
	"dissident/internal/xraycfg"
)

const (
	defaultTCPTimeout      = 3 * time.Second
	defaultTLSTimeout      = 3 * time.Second
	defaultProtocolTimeout = 3 * time.Second
	defaultXrayTimeout     = 10 * time.Second
	defaultHTTPTimeout     = 5 * time.Second
	defaultSpeedSize       = int64(4_000_000)
	defaultContentMinBytes = 4096
	defaultContentQuorum   = 3
	stopXrayTimeout        = 6 * time.Second
)

// Options configures a Tester. Zero timeouts and limits fall back to the
// documented demo defaults; MinSpeedMbps and MaxTTFBMs alone use zero to
// disable their check.
type Options struct {
	Test        config.TestConfig
	URLs        config.URLsConfig
	XrayBin     string
	EgressProxy string
	Logger      *slog.Logger
	TempDir     string
}

// Tester runs qualification stages. It holds only immutable configuration
// after New, so concurrent Test calls are safe. Real xray work remains
// serialized by the caller's real_workers budget.
type Tester struct {
	test        config.TestConfig
	urls        config.URLsConfig
	xrayBin     string
	egressProxy string
	log         *slog.Logger
	tempDir     string
	egressDial  func(context.Context, string, string) (net.Conn, error)
	egressErr   error
}

// New builds a Tester, applying defaults for unset bounds and constructing
// the optional egress dialer once.
func New(opts Options) *Tester {
	test := withTestDefaults(opts.Test)
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	tester := &Tester{
		test:        test,
		urls:        cloneURLs(opts.URLs),
		xrayBin:     opts.XrayBin,
		egressProxy: strings.TrimSpace(opts.EgressProxy),
		log:         logger,
		tempDir:     opts.TempDir,
	}
	if tester.egressProxy != "" {
		dial, err := platform.NewEgressDialer(tester.egressProxy, test.TCPTimeout)
		tester.egressDial = dial
		tester.egressErr = err
		if err != nil {
			logger.Warn("egress proxy is unusable", "err", err)
		}
	}
	return tester
}

func withTestDefaults(test config.TestConfig) config.TestConfig {
	if test.TCPTimeout <= 0 {
		test.TCPTimeout = defaultTCPTimeout
	}
	if test.TLSTimeout <= 0 {
		test.TLSTimeout = defaultTLSTimeout
	}
	if test.ProtocolTimeout <= 0 {
		test.ProtocolTimeout = defaultProtocolTimeout
	}
	if test.XrayTimeout <= 0 {
		test.XrayTimeout = defaultXrayTimeout
	}
	if test.HTTPTimeout <= 0 {
		test.HTTPTimeout = defaultHTTPTimeout
	}
	if test.SpeedSize <= 0 {
		test.SpeedSize = defaultSpeedSize
	}
	if test.ContentMinBytes <= 0 {
		test.ContentMinBytes = defaultContentMinBytes
	}
	if test.ContentQuorum <= 0 {
		test.ContentQuorum = defaultContentQuorum
	}
	if test.StabilityPasses <= 0 {
		test.StabilityPasses = 1
	}
	if test.MinSpeedMbps < 0 {
		test.MinSpeedMbps = 0
	}
	if test.MaxTTFBMs < 0 {
		test.MaxTTFBMs = 0
	}
	return test
}

func cloneURLs(urls config.URLsConfig) config.URLsConfig {
	clone := urls
	clone.Probe = append([]string(nil), urls.Probe...)
	clone.Verify = append([]string(nil), urls.Verify...)
	clone.Content = append([]string(nil), urls.Content...)
	return clone
}

// Test runs the strict qualification pipeline for one link. It never starts
// xray before support, TCP, TLS, protocol, render, and preflight pass, starts
// the real xray at most once, and stops it on every return path.
func (t *Tester) Test(ctx context.Context, link contract.Link) contract.Result {
	if ctx == nil {
		ctx = context.Background()
	}
	result := contract.Result{Hash: link.Hash, At: time.Now().UTC()}
	fail := func(stage contract.Stage, format string, args ...any) contract.Result {
		result.Stage = stage
		result.OK = false
		result.Reason = fmt.Sprintf(format, args...)
		result.At = time.Now().UTC()
		t.log.Debug("qualification failed", "hash", link.Hash, "stage", stage, "reason", result.Reason)
		return result
	}

	// 1. Static support gate.
	if err := xraycfg.Supports(link); err != nil {
		return fail(contract.StageParse, "parse: %s", detailOf(err))
	}

	// 2. TCP reachability.
	latency, err := t.tcpCheck(ctx, link)
	if err != nil {
		return fail(contract.StageTCP, "tcp: %s", detailOf(err))
	}
	result.TCPLatency = latency

	// 3. TLS handshake only for TLS transports.
	if needsTLS(link) {
		if err := t.tlsCheck(ctx, link); err != nil {
			return fail(contract.StageTLS, "tls: %s", detailOf(err))
		}
	}

	// 4. Static credential validation; no wire handshake.
	if err := validateProtocol(link); err != nil {
		return fail(contract.StageProtocol, "protocol: %s", detailOf(err))
	}

	// 5. Render + xray schema preflight.
	port, err := platform.FreePort()
	if err != nil {
		return fail(contract.StageRender, "render: %s", detailOf(err))
	}
	cfg, err := platform.RenderWithEgress(link, port, t.egressProxy)
	if err != nil {
		return fail(contract.StageRender, "render: %s", detailOf(err))
	}
	preflightCtx, preflightCancel := context.WithTimeout(ctx, t.test.XrayTimeout)
	err = xraycfg.Preflight(preflightCtx, t.xrayBin, cfg)
	preflightCancel()
	if err != nil {
		return fail(contract.StageRender, "render: %s", detailOf(err))
	}

	// 6. Start the real xray once and reuse it for the remaining stages.
	startCtx, startCancel := context.WithTimeout(ctx, t.test.XrayTimeout)
	xray, err := platform.StartXray(startCtx, t.xrayBin, cfg, t.tempDir)
	startCancel()
	if err != nil {
		return fail(contract.StageReal, "real: %s", detailOf(err))
	}
	defer t.stopXray(xray)

	socksAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(xray.Port))
	client := t.httpClient(socksAddr)
	defer client.CloseIdleConnections()

	real, err := t.verifyAny(ctx, client, maxRealBody)
	if err != nil {
		return fail(contract.StageReal, "real: %s", detailOf(err))
	}
	result.TTFB = real.TTFB
	result.HTTPStatus = real.Status
	result.Bytes = real.Bytes

	// 7. Content quorum.
	pass, total, contentBytes, err := t.contentCheck(ctx, client)
	if err != nil {
		return fail(contract.StageContent, "content: %s", detailOf(err))
	}
	if contentBytes > result.Bytes {
		result.Bytes = contentBytes
	}
	if pass < t.test.ContentQuorum {
		return fail(contract.StageContent, "content: %d/%d", pass, total)
	}

	// 8. Speed and TTFB.
	speed, ttfb, err := t.Speed(ctx, socksAddr)
	if err != nil {
		return fail(contract.StageSpeed, "speed: %s", detailOf(err))
	}
	result.SpeedMbps = speed
	if ttfb > 0 {
		result.TTFB = ttfb
	}
	if reason := t.speedReason(speed, result.TTFB); reason != "" {
		return fail(contract.StageSpeed, "%s", reason)
	}

	// 9. Stability: StabilityPasses-1 additional verify successes.
	extra := t.test.StabilityPasses - 1
	for pass := 0; pass < extra; pass++ {
		if _, err := t.verifyAny(ctx, client, 0); err != nil {
			return fail(contract.StageStability, "stability: pass %d/%d: %s", pass+2, t.test.StabilityPasses, detailOf(err))
		}
	}

	result.Stage = contract.StageStability
	result.OK = true
	result.Reason = "ok"
	result.At = time.Now().UTC()
	t.log.Debug("qualification passed", "hash", link.Hash, "tcp", result.TCPLatency, "ttfb", result.TTFB, "mbps", result.SpeedMbps)
	return result
}

// Promote renders link, starts its xray, verifies a 2xx through it, and
// optionally measures speed/TTFB. On success the caller owns the returned
// process; on any failure the process is stopped and an error returned.
func (t *Tester) Promote(ctx context.Context, link contract.Link) (contract.Result, *platform.Xray, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	result := contract.Result{Hash: link.Hash, At: time.Now().UTC()}
	fail := func(stage contract.Stage, format string, args ...any) (contract.Result, *platform.Xray, error) {
		result.Stage = stage
		result.OK = false
		result.Reason = fmt.Sprintf(format, args...)
		result.At = time.Now().UTC()
		t.log.Debug("promotion failed", "hash", link.Hash, "stage", stage, "reason", result.Reason)
		return result, nil, errors.New(result.Reason)
	}

	if err := xraycfg.Supports(link); err != nil {
		return fail(contract.StageParse, "parse: %s", detailOf(err))
	}
	if err := validateProtocol(link); err != nil {
		return fail(contract.StageProtocol, "protocol: %s", detailOf(err))
	}
	port, err := platform.FreePort()
	if err != nil {
		return fail(contract.StageRender, "render: %s", detailOf(err))
	}
	cfg, err := platform.RenderWithEgress(link, port, t.egressProxy)
	if err != nil {
		return fail(contract.StageRender, "render: %s", detailOf(err))
	}
	startCtx, startCancel := context.WithTimeout(ctx, t.test.XrayTimeout)
	xray, err := platform.StartXray(startCtx, t.xrayBin, cfg, t.tempDir)
	startCancel()
	if err != nil {
		return fail(contract.StageReal, "real: %s", detailOf(err))
	}
	success := false
	defer func() {
		if !success {
			t.stopXray(xray)
		}
	}()

	socksAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(xray.Port))
	client := t.httpClient(socksAddr)
	defer client.CloseIdleConnections()

	verified, err := t.verifyAny(ctx, client, maxRealBody)
	if err != nil {
		return fail(contract.StageReal, "real: %s", detailOf(err))
	}
	result.TTFB = verified.TTFB
	result.HTTPStatus = verified.Status
	result.Bytes = verified.Bytes

	measuredStage := contract.StageReal
	if t.test.MinSpeedMbps > 0 || t.test.MaxTTFBMs > 0 {
		speed, ttfb, err := t.Speed(ctx, socksAddr)
		if err != nil {
			return fail(contract.StageSpeed, "speed: %s", detailOf(err))
		}
		result.SpeedMbps = speed
		if ttfb > 0 {
			result.TTFB = ttfb
		}
		if reason := t.speedReason(speed, result.TTFB); reason != "" {
			return fail(contract.StageSpeed, "%s", reason)
		}
		measuredStage = contract.StageSpeed
	}

	result.Stage = measuredStage
	result.OK = true
	result.Reason = "ok"
	result.At = time.Now().UTC()
	success = true
	t.log.Debug("promotion passed", "hash", link.Hash, "port", xray.Port)
	return result, xray, nil
}

// tcpCheck dials the link server and records the TCP round-trip time.
func (t *Tester) tcpCheck(ctx context.Context, link contract.Link) (time.Duration, error) {
	address := net.JoinHostPort(link.Server, strconv.Itoa(link.Port))
	dialCtx, cancel := context.WithTimeout(ctx, t.test.TCPTimeout)
	defer cancel()
	start := time.Now()
	conn, err := t.dial(dialCtx, "tcp", address)
	elapsed := time.Since(start)
	if err != nil {
		return 0, err
	}
	conn.Close()
	return elapsed, nil
}

// tlsCheck performs a TLS handshake with the link's SNI. Verification is
// deliberately disabled: this stage measures reachability, not identity.
func (t *Tester) tlsCheck(ctx context.Context, link contract.Link) error {
	address := net.JoinHostPort(link.Server, strconv.Itoa(link.Port))
	dialCtx, cancel := context.WithTimeout(ctx, t.test.TLSTimeout)
	defer cancel()
	conn, err := t.dial(dialCtx, "tcp", address)
	if err != nil {
		return err
	}
	defer conn.Close()
	if deadline, ok := dialCtx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	tlsConn := tls.Client(conn, &tls.Config{
		ServerName:         sniOf(link),
		InsecureSkipVerify: true, // reachability probe only
		MinVersion:         tls.VersionTLS12,
	})
	return tlsConn.HandshakeContext(dialCtx)
}

// dial uses the egress dialer when configured, otherwise direct TCP.
func (t *Tester) dial(ctx context.Context, network, address string) (net.Conn, error) {
	if t.egressErr != nil {
		return nil, t.egressErr
	}
	if t.egressDial != nil {
		return t.egressDial(ctx, network, address)
	}
	dialer := &net.Dialer{Timeout: t.test.TCPTimeout}
	return dialer.DialContext(ctx, network, address)
}

// speedReason returns a stable failure reason when a speed result misses the
// configured thresholds, or the empty string when it passes.
func (t *Tester) speedReason(mbps float64, ttfb time.Duration) string {
	if minimum := t.test.MinSpeedMbps; minimum > 0 && mbps < minimum {
		return fmt.Sprintf("speed: %.1f Mbps < %g", mbps, minimum)
	}
	if maximum := t.test.MaxTTFBMs; maximum > 0 && ttfb > time.Duration(maximum)*time.Millisecond {
		return fmt.Sprintf("speed: TTFB %dms > %dms", ttfb.Milliseconds(), maximum)
	}
	return ""
}

// stopXray stops a qualification child with a bounded background context.
func (t *Tester) stopXray(xray *platform.Xray) {
	if xray == nil {
		return
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), stopXrayTimeout)
	defer cancel()
	if err := xray.Stop(stopCtx); err != nil {
		t.log.Warn("stop xray failed", "err", err)
	}
}

// detailOf trims the package prefix shared by every xraycfg error.
func detailOf(err error) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	for strings.HasPrefix(message, "xraycfg: ") {
		message = strings.TrimPrefix(message, "xraycfg: ")
	}
	return message
}
