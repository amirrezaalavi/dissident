package logging

import (
	"io"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"

	"dissident/internal/contract"
)

// AccessTimeFormat is the fixed, millisecond-resolution UTC timestamp used in
// the access log. Keeping it fixed makes the TSV columns easy to parse.
const AccessTimeFormat = "2006-01-02T15:04:05.000Z07:00"

// FormatAccessRecord renders one access record as a tab-separated line ending
// in a newline. The base columns are:
//
//	time  src  state  dst  domain:port
//
// When any extended detail is present, six more columns are appended:
//
//	proto  wan  up_bytes  down_bytes  duration_ms  reason
//
// Missing string values are rendered as "-". Up and down byte counts are
// always decimal; duration is in whole milliseconds. Tabs, newlines, and
// carriage returns in values are replaced with '_', and URL userinfo is
// stripped so credentials can never reach the access log.
func FormatAccessRecord(rec contract.AccessRecord) string {
	fields := make([]string, 0, 11)
	fields = append(fields,
		rec.Time.UTC().Format(AccessTimeFormat),
		orDash(sanitizeAddr(rec.Src)),
		orDash(sanitizeField(rec.State)),
		orDash(sanitizeAddr(rec.Dst)),
		domainPort(rec.Domain, rec.Port),
	)
	if hasAccessDetails(rec) {
		fields = append(fields,
			orDash(sanitizeField(rec.Proto)),
			orDash(sanitizeField(rec.WAN)),
			strconv.FormatInt(rec.Up, 10),
			strconv.FormatInt(rec.Down, 10),
			strconv.FormatInt(rec.Duration.Milliseconds(), 10),
			orDash(sanitizeField(rec.Reason)),
		)
	}
	return strings.Join(fields, "\t") + "\n"
}

func hasAccessDetails(rec contract.AccessRecord) bool {
	return rec.Proto != "" || rec.WAN != "" || rec.Up != 0 || rec.Down != 0 ||
		rec.Duration != 0 || rec.Reason != ""
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func domainPort(domain string, port int) string {
	d := sanitizeAddr(domain)
	switch {
	case d != "" && port > 0:
		return net.JoinHostPort(d, strconv.Itoa(port))
	case d != "":
		return d
	case port > 0:
		return ":" + strconv.Itoa(port)
	default:
		return "-"
	}
}

func sanitizeField(s string) string {
	s = strings.TrimSpace(s)
	return strings.Map(func(r rune) rune {
		switch r {
		case '\t', '\n', '\r':
			return '_'
		case 0:
			return -1
		default:
			return r
		}
	}, s)
}

func sanitizeAddr(s string) string {
	s = sanitizeField(s)
	if i := strings.Index(s, "://"); i > 0 {
		if u, err := url.Parse(s); err == nil && u.User != nil {
			u.User = nil
			s = u.String()
		}
	}
	// Handle raw user:pass@host:port forms that are not full URLs.
	if at := strings.LastIndexByte(s, '@'); at >= 0 && !strings.ContainsAny(s[:at], " /") {
		s = s[at+1:]
	}
	return s
}

// AccessLogger serializes TSV access records to a writer. It is safe for
// concurrent use and preserves whole-line writes.
type AccessLogger struct {
	mu      sync.Mutex
	w       io.Writer
	closeFn func() error
	closed  bool
}

func newAccessLogger(w io.Writer, closeFn func() error) *AccessLogger {
	return &AccessLogger{w: w, closeFn: closeFn}
}

// Log writes one access record. It returns os.ErrClosed after Close.
func (a *AccessLogger) Log(rec contract.AccessRecord) error {
	if a == nil || a.w == nil {
		return nil
	}
	line := FormatAccessRecord(rec)
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return os.ErrClosed
	}
	_, err := io.WriteString(a.w, line)
	return err
}

// Close closes the underlying writer if it has one. It is idempotent.
func (a *AccessLogger) Close() error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return nil
	}
	a.closed = true
	if a.closeFn != nil {
		return a.closeFn()
	}
	return nil
}
