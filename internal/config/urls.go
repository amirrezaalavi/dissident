package config

import "strings"

// sanitizeURL trims the artifacts that commonly sneak into a copied URL:
// surrounding whitespace, one layer of matching quotes, and trailing
// backslashes. A legitimate trailing slash is preserved: the sanitizer never
// rewrites the path.
//
// A trailing backslash is almost always an artifact of pasting a URL from a
// shell or an editor where the URL continued on the next line. Leaving it in
// place makes net/url encode it as %5C, so an HTTP/2 or exit-IP check hits
// https://host/%5C instead of the intended page and fails for no real reason.
func sanitizeURL(raw string) string {
	s := strings.TrimSpace(raw)
	s = unquote(s)
	s = strings.TrimSpace(s)
	for len(s) > 0 && s[len(s)-1] == '\\' {
		s = strings.TrimSpace(s[:len(s)-1])
	}
	return s
}

// normalizeURLs sanitizes every URL-bearing field in place. It is applied both
// when a config file is loaded and when a Config is published to a Manager, so
// programmatic snapshots get the same treatment as file-based ones.
func (c *Config) normalizeURLs() {
	for i, raw := range c.URLs.Probe {
		c.URLs.Probe[i] = sanitizeURL(raw)
	}
	for i, raw := range c.URLs.Verify {
		c.URLs.Verify[i] = sanitizeURL(raw)
	}
	for i, raw := range c.URLs.Content {
		c.URLs.Content[i] = sanitizeURL(raw)
	}
	c.URLs.Speed = sanitizeURL(c.URLs.Speed)
	c.URLs.SpeedFallback = sanitizeURL(c.URLs.SpeedFallback)
	c.URLs.Upload = sanitizeURL(c.URLs.Upload)
	c.URLs.HTTP2 = sanitizeURL(c.URLs.HTTP2)
	c.URLs.ExitIP = sanitizeURL(c.URLs.ExitIP)
}
