package control

import (
	"bufio"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"unicode"
)

const maxLogScanBytes = 2 << 20 // 2 MiB

type logTab struct {
	Type   string
	Label  string
	Href   string
	Active bool
}

type logsPage struct {
	baseData
	Type     string
	Lines    int
	Filter   string
	Tabs     []logTab
	FileNote string
	Message  string
	Content  []string
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	typ := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("type")))
	if typ != "system" {
		typ = "access"
	}
	lines := clampLines(r.URL.Query().Get("lines"))
	filter := sanitizeFilter(r.URL.Query().Get("filter"))

	p := logsPage{
		baseData: s.newBase(r, "Logs", "logs"),
		Type:     typ,
		Lines:    lines,
		Filter:   filter,
	}
	p.Tabs = []logTab{
		{Type: "access", Label: "Access log", Href: logHref("access", lines, filter)},
		{Type: "system", Label: "System log", Href: logHref("system", lines, filter)},
	}
	for i := range p.Tabs {
		p.Tabs[i].Active = p.Tabs[i].Type == typ
	}

	label := "access"
	path := s.opts.AccessLog
	if typ == "system" {
		label = "system"
		path = s.opts.SystemLog
	}
	if strings.TrimSpace(path) == "" {
		p.Message = "No " + label + " log file is configured for this panel."
		s.renderPage(w, r, pages.logs, p)
		return
	}

	p.FileNote = "File: " + sanitizePath(path) +
		" · last " + strconv.Itoa(lines) + " matching lines · at most the final 2 MiB is scanned"

	content, err := tailMatchingLines(path, lines, filter)
	switch {
	case err != nil:
		if errors.Is(err, fs.ErrNotExist) {
			p.Message = "The " + label + " log file does not exist yet."
		} else {
			p.Message = "The " + label + " log file could not be read."
			s.opts.Logger.Warn("control log read failed", "type", typ, "err", sanitizeMessage(err.Error()))
		}
	case len(content) == 0:
		p.Message = "No matching log lines."
	default:
		p.Content = content
	}
	s.renderPage(w, r, pages.logs, p)
}

func logHref(typ string, lines int, filter string) string {
	q := url.Values{}
	q.Set("type", typ)
	q.Set("lines", strconv.Itoa(lines))
	if filter != "" {
		q.Set("filter", filter)
	}
	return "/logs?" + q.Encode()
}

func sanitizeLogLine(line string) string {
	line = redactText(line)
	line = strings.Map(func(r rune) rune {
		switch {
		case r == '\t':
			return r
		case r == '\r':
			return -1
		case unicode.IsControl(r):
			return ' '
		default:
			return r
		}
	}, line)
	return truncateRunes(line, maxLogLine)
}

// tailMatchingLines reads at most the final 2 MiB of path and returns the last
// maxLines lines containing filter (case-insensitive). It never invokes a
// shell and only reads regular files.
func tailMatchingLines(path string, maxLines int, filter string) ([]string, error) {
	if maxLines < 1 {
		maxLines = 1
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	size := info.Size()
	start := int64(0)
	if size > maxLogScanBytes {
		start = size - maxLogScanBytes
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}

	reader := bufio.NewReader(io.LimitReader(f, maxLogScanBytes))
	if start > 0 {
		// The first line is probably partial; discard it.
		if _, err := reader.ReadString('\n'); err != nil {
			if errors.Is(err, io.EOF) {
				return nil, nil
			}
			return nil, err
		}
	}

	needle := strings.ToLower(filter)
	ring := make([]string, maxLines)
	count := 0
	for {
		line, readErr := reader.ReadString('\n')
		if len(line) > 0 {
			line = strings.TrimSuffix(line, "\n")
			line = strings.TrimSuffix(line, "\r")
			line = sanitizeLogLine(line)
			if needle == "" || strings.Contains(strings.ToLower(line), needle) {
				ring[count%maxLines] = line
				count++
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return nil, readErr
		}
	}

	n := count
	if n > maxLines {
		n = maxLines
	}
	out := make([]string, 0, n)
	startIdx := 0
	if count > maxLines {
		startIdx = count % maxLines
	}
	for i := 0; i < n; i++ {
		out = append(out, ring[(startIdx+i)%maxLines])
	}
	return out, nil
}
