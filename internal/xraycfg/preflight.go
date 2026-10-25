package xraycfg

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// preflightTimeout bounds a schema check when the caller supplied no deadline.
const preflightTimeout = 15 * time.Second

// Preflight validates cfg with a real xray binary by writing it to a 0600
// temporary file and running "xray -test -c <file>". The temp file is always
// removed. No shell is involved. On failure the combined xray output is
// included in the returned error.
func Preflight(ctx context.Context, xrayBin string, cfg []byte) error {
	if strings.TrimSpace(xrayBin) == "" {
		return errors.New("xraycfg: preflight: empty xray binary path")
	}
	if len(cfg) == 0 {
		return errors.New("xraycfg: preflight: empty config")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	file, err := os.CreateTemp("", "dissident-xray-*.json")
	if err != nil {
		return fmt.Errorf("xraycfg: preflight: create temp config: %w", err)
	}
	name := file.Name()
	defer os.Remove(name)

	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return fmt.Errorf("xraycfg: preflight: chmod temp config: %w", err)
	}
	if _, err := file.Write(cfg); err != nil {
		file.Close()
		return fmt.Errorf("xraycfg: preflight: write temp config: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("xraycfg: preflight: close temp config: %w", err)
	}

	runCtx := ctx
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, preflightTimeout)
		defer cancel()
	}

	cmd := exec.CommandContext(runCtx, xrayBin, "-test", "-c", name)
	output, runErr := cmd.CombinedOutput()
	if runErr != nil {
		out := strings.TrimSpace(string(output))
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("xraycfg: preflight: %s -test timed out: %w: %s", xrayBin, runCtx.Err(), out)
		}
		if runCtx.Err() != nil {
			return fmt.Errorf("xraycfg: preflight: %s -test canceled: %w: %s", xrayBin, runCtx.Err(), out)
		}
		return fmt.Errorf("xraycfg: preflight: %s -test failed: %w: %s", xrayBin, runErr, out)
	}
	return nil
}
