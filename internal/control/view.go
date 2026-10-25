package control

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"dissident/internal/contract"
)

const (
	maxDisplayName = 120
	maxReason      = 300
	maxFlash       = 300
	maxFilter      = 120
	maxLogLine     = 4000
	hashPrefixLen  = 12
	subKeyLen      = 16
)

var (
	// Share links are never displayed, partly or wholly. Matching the whole
	// token also removes UUIDs and passwords embedded in the URI.
	shareLinkPattern = regexp.MustCompile(`(?i)\b(?:vless|vmess|trojan|ss|socks5)://[^\s"'<>]+`)
	// HTTP(S) URLs may be displayed after their userinfo is stripped.
	httpURLPattern = regexp.MustCompile(`(?i)\bhttps?://[^\s"'<>]+`)
	// UUIDs are credentials in several protocols and are always redacted.
	uuidPattern = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	// Raw user:pass@host forms (no scheme) are also redacted.
	userinfoPattern = regexp.MustCompile(`[A-Za-z0-9._~%+\-]+:[^\s@/<>"']+@`)
)

// redactText removes credentials and share-link URIs from any data that might
// reach HTML, a flash message, or a log line. It is deliberately conservative:
// hiding a little too much is acceptable, leaking a password is not.
func redactText(s string) string {
	if s == "" {
		return ""
	}
	s = shareLinkPattern.ReplaceAllString(s, "[redacted link]")
	s = httpURLPattern.ReplaceAllStringFunc(s, sanitizeHTTPURL)
	s = userinfoPattern.ReplaceAllString(s, "[redacted]@")
	s = uuidPattern.ReplaceAllString(s, "[redacted-uuid]")
	return s
}

// sanitizeHTTPURL strips userinfo and redacts sensitive query parameters while
// preserving enough of the URL (host, path) to be useful on the settings and
// subscriptions pages.
func sanitizeHTTPURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "[redacted url]"
	}
	u.User = nil
	q := u.Query()
	for _, key := range []string{
		"password", "passwd", "pass", "pwd", "token", "secret", "key",
		"uuid", "auth", "authorization", "apikey", "api_key",
	} {
		if q.Has(key) {
			q.Set(key, "redacted")
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func sanitizeMessage(s string) string {
	s = redactText(strings.TrimSpace(s))
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return ""
	}
	return truncateRunes(s, maxFlash)
}

func sanitizeLabel(s, fallback string) string {
	s = redactText(strings.TrimSpace(s))
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\t' || r == '\n' || r == '\r':
			return ' '
		case unicode.IsControl(r):
			return -1
		default:
			return r
		}
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return fallback
	}
	return truncateRunes(s, maxDisplayName)
}

func sanitizeReason(s string) string {
	s = sanitizeMessage(s)
	if s == "" {
		return ""
	}
	return truncateRunes(s, maxReason)
}

func sanitizeFilter(s string) string {
	s = redactText(strings.TrimSpace(s))
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return ""
	}
	return truncateRunes(s, maxFilter)
}

func sanitizePath(s string) string {
	s = redactText(strings.TrimSpace(s))
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	if s == "" {
		return "(not set)"
	}
	return truncateRunes(s, 300)
}

func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

func sanitizeHost(s string) string {
	s = redactText(strings.TrimSpace(s))
	if strings.Contains(s, "://") {
		if u, err := url.Parse(s); err == nil && u.Hostname() != "" {
			s = u.Hostname()
		}
	}
	if at := strings.LastIndexByte(s, '@'); at >= 0 {
		s = s[at+1:]
	}
	s = strings.Trim(s, "[]")
	s = strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\r', '\n', '/', '@':
			return -1
		default:
			return r
		}
	}, s)
	return s
}

func hostPort(l contract.Link) string {
	host := sanitizeHost(l.Server)
	if host == "" {
		return "-"
	}
	if l.Port > 0 {
		return net.JoinHostPort(host, strconv.Itoa(l.Port))
	}
	return host
}

func protocolLabel(p contract.Protocol) string {
	return sanitizeLabel(string(p), "unknown")
}

func displayName(l contract.Link) string {
	if name := sanitizeLabel(l.Name, ""); name != "" {
		return name
	}
	if hp := hostPort(l); hp != "-" {
		return hp
	}
	return "unnamed"
}

func displayOptional(s string) string {
	return sanitizeLabel(s, "unknown")
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.UTC().Format(time.RFC3339)
}

func fmtAgo(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Second:
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func formatScore(score float64) string {
	if math.IsNaN(score) || math.IsInf(score, 0) {
		return "-"
	}
	return strconv.FormatFloat(score, 'f', 2, 64)
}

func durationText(d time.Duration) string {
	if d <= 0 {
		return "-"
	}
	if d < time.Millisecond {
		return d.Round(time.Microsecond).String()
	}
	return d.Round(time.Millisecond).String()
}

func speedText(mbps float64) string {
	if mbps <= 0 || math.IsNaN(mbps) || math.IsInf(mbps, 0) {
		return "-"
	}
	return fmt.Sprintf("%.2f Mbps", mbps)
}

func hashPrefix(h string) string {
	h = strings.TrimSpace(h)
	if len(h) > hashPrefixLen {
		h = h[:hashPrefixLen]
	}
	return sanitizeLabel(h, "")
}

func subKey(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])[:subKeyLen]
}

func isHTTPSubscription(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Scheme, "http") || strings.EqualFold(u.Scheme, "https")
}

func validateSubscriptionURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fmt.Errorf("subscription URL is required")
	}
	if strings.ContainsAny(raw, "\r\n\t") {
		return fmt.Errorf("subscription URL must be a single line")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid subscription URL: %v", err)
	}
	if !strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https") {
		return fmt.Errorf("subscription URL must start with http:// or https://")
	}
	if u.Host == "" {
		return fmt.Errorf("subscription URL is missing a host")
	}
	return nil
}

func subscriptionDisplay(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "[invalid URL]"
	}
	return sanitizeHTTPURL(raw)
}

type activeView struct {
	Name       string
	ServerPort string
	Protocol   string
	Since      string
	SinceAgo   string
	ExitIP     string
}

func activeViewFrom(a *contract.Active) *activeView {
	if a == nil {
		return nil
	}
	name := sanitizeLabel(a.Link.Name, "")
	if name == "" {
		name = hostPort(a.Link)
	}
	if name == "-" {
		name = "active WAN"
	}
	return &activeView{
		Name:       name,
		ServerPort: hostPort(a.Link),
		Protocol:   protocolLabel(a.Link.Protocol),
		Since:      fmtTime(a.Since),
		SinceAgo:   fmtAgo(a.Since),
		ExitIP:     displayOptional(a.ExitIP),
	}
}

type readyRow struct {
	Name       string
	ServerPort string
	Protocol   string
	Score      string
	LastCheck  string
	ExitIP     string
	HashPrefix string
	CanRemove  bool
}

func sortedReady(ready []contract.Ready) []contract.Ready {
	out := append([]contract.Ready(nil), ready...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].LastCheck.After(out[j].LastCheck)
	})
	return out
}

func readyRows(ready []contract.Ready, limit int, canRemove bool) []readyRow {
	sorted := sortedReady(ready)
	if limit > 0 && len(sorted) > limit {
		sorted = sorted[:limit]
	}
	rows := make([]readyRow, 0, len(sorted))
	for _, r := range sorted {
		prefix := hashPrefix(r.Link.Hash)
		rows = append(rows, readyRow{
			Name:       displayName(r.Link),
			ServerPort: hostPort(r.Link),
			Protocol:   protocolLabel(r.Link.Protocol),
			Score:      formatScore(r.Score),
			LastCheck:  fmtTime(r.LastCheck),
			ExitIP:     displayOptional(r.ExitIP),
			HashPrefix: prefix,
			CanRemove:  canRemove && prefix != "",
		})
	}
	return rows
}

type testRow struct {
	Name       string
	Protocol   string
	ServerPort string
	State      string
	LastStage  string
	Result     string
	Reason     string
	TCP        string
	Speed      string
	Updated    string
}

func entryRank(s contract.EntryState) int {
	switch s {
	case contract.StateTesting:
		return 0
	case contract.StateActive:
		return 1
	case contract.StateReady:
		return 2
	case contract.StateCatalog:
		return 3
	case contract.StateFailed:
		return 4
	case contract.StateUnsupported:
		return 5
	default:
		return 6
	}
}

func entryTime(e contract.Entry) time.Time {
	if !e.Updated.IsZero() {
		return e.Updated
	}
	return e.Last.At
}

func sortedEntries(entries []contract.Entry) []contract.Entry {
	out := append([]contract.Entry(nil), entries...)
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := entryRank(out[i].State), entryRank(out[j].State)
		if ri != rj {
			return ri < rj
		}
		ti, tj := entryTime(out[i]), entryTime(out[j])
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return displayName(out[i].Link) < displayName(out[j].Link)
	})
	return out
}

func testRowFrom(e contract.Entry) testRow {
	state := sanitizeLabel(string(e.State), "unknown")
	stage := sanitizeLabel(string(e.Last.Stage), "-")
	result := "-"
	if !e.Last.At.IsZero() {
		if e.Last.OK {
			result = "ok"
		} else {
			result = "fail"
		}
	}
	updated := e.Updated
	if updated.IsZero() {
		updated = e.Last.At
	}
	return testRow{
		Name:       displayName(e.Link),
		Protocol:   protocolLabel(e.Link.Protocol),
		ServerPort: hostPort(e.Link),
		State:      state,
		LastStage:  stage,
		Result:     result,
		Reason:     sanitizeReason(e.Last.Reason),
		TCP:        durationText(e.Last.TCPLatency),
		Speed:      speedText(e.Last.SpeedMbps),
		Updated:    fmtTime(updated),
	}
}

func testRows(entries []contract.Entry, limit int) []testRow {
	sorted := sortedEntries(entries)
	if limit > 0 && len(sorted) > limit {
		sorted = sorted[:limit]
	}
	rows := make([]testRow, 0, len(sorted))
	for _, e := range sorted {
		rows = append(rows, testRowFrom(e))
	}
	return rows
}

func recentRows(entries []contract.Entry, limit int) []testRow {
	filtered := make([]contract.Entry, 0, len(entries))
	for _, e := range entries {
		if !e.Last.At.IsZero() {
			filtered = append(filtered, e)
		}
	}
	sort.SliceStable(filtered, func(i, j int) bool {
		return filtered[i].Last.At.After(filtered[j].Last.At)
	})
	if limit > 0 && len(filtered) > limit {
		filtered = filtered[:limit]
	}
	rows := make([]testRow, 0, len(filtered))
	for _, e := range filtered {
		rows = append(rows, testRowFrom(e))
	}
	return rows
}
