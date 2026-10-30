// Package provider classifies a link's server into a coarse failure domain
// using only published IP ranges and netip. It deliberately performs no DNS
// resolution and uses no ASN database, so its answer is a cheap lower bound on
// provider diversity rather than an authoritative operator identity.
package provider

import (
	"net"
	"net/netip"
	"strconv"
	"strings"

	"dissident/internal/contract"
)

// Kind is the provider/failure-domain class of one link endpoint.
type Kind string

const (
	// Cloudflare means the server is a literal IPv4 address inside one of the
	// published Cloudflare ranges below.
	Cloudflare Kind = "cloudflare"
	// NonCloudflare means the server is a literal public IPv4 address outside
	// the Cloudflare ranges.
	NonCloudflare Kind = "non-cloudflare"
	// Unknown means the classifier cannot make a safe statement: hostnames are
	// not resolved, IPv6 is not covered by the published IPv4 list, and
	// private/reserved addresses are not useful WAN targets.
	Unknown Kind = "unknown"
)

// cloudflareIPv4 is Cloudflare's published IPv4 list, retrieved from
// https://www.cloudflare.com/ips-v4 on 2026-10-31. Cloudflare can add or
// retire ranges; re-check the published list when refreshing this list.
//
// Keeping the list in source (instead of fetching it) keeps classification
// deterministic, offline, and free of WAN traffic. The tradeoff is that a new
// Cloudflare range is classified as non-Cloudflare until this list is updated.
var cloudflareIPv4 = []netip.Prefix{
	netip.MustParsePrefix("173.245.48.0/20"),
	netip.MustParsePrefix("103.21.244.0/22"),
	netip.MustParsePrefix("103.22.200.0/22"),
	netip.MustParsePrefix("103.31.4.0/22"),
	netip.MustParsePrefix("141.101.64.0/18"),
	netip.MustParsePrefix("108.162.192.0/18"),
	netip.MustParsePrefix("190.93.240.0/20"),
	netip.MustParsePrefix("188.114.96.0/20"),
	netip.MustParsePrefix("197.234.240.0/22"),
	netip.MustParsePrefix("198.41.128.0/17"),
	netip.MustParsePrefix("162.158.0.0/15"),
	netip.MustParsePrefix("104.16.0.0/13"),
	netip.MustParsePrefix("104.24.0.0/14"),
	netip.MustParsePrefix("172.64.0.0/13"),
	netip.MustParsePrefix("131.0.72.0/22"),
}

// reservedIPv4 lists non-global or non-routable space that is not a meaningful
// public WAN endpoint. netip.Addr already covers RFC1918, loopback, link-local,
// multicast, and ULA; these prefixes cover the remaining special-purpose
// blocks (including documentation ranges).
var reservedIPv4 = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("255.255.255.255/32"),
}

// ClassifyServer classifies a server host. A hostname is Unknown because this
// package never resolves names; treating a hostname as non-Cloudflare would
// silently claim diversity that cannot be verified without DNS. The caller can
// still use hostname endpoints for admission, but they are not reserve
// candidates and are counted as unknown in the panel/metrics.
func ClassifyServer(server string) Kind {
	host := normalizeHost(server)
	if host == "" {
		return Unknown
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		// Hostname: no resolver by design.
		return Unknown
	}
	addr = addr.Unmap()
	if !addr.Is4() {
		// The documented Cloudflare list used here is IPv4-only. IPv6 is not
		// guessed from an incomplete range set.
		return Unknown
	}
	if isSpecial(addr) {
		return Unknown
	}
	for _, p := range cloudflareIPv4 {
		if p.Contains(addr) {
			return Cloudflare
		}
	}
	return NonCloudflare
}

// ClassifyLink classifies one link by its server address.
func ClassifyLink(link contract.Link) Kind {
	return ClassifyServer(link.Server)
}

// EndpointKey returns a canonical exact endpoint key (server:port). Hostnames
// are lowercased and trailing dots are removed. An empty host or invalid port
// yields "".
func EndpointKey(link contract.Link) string {
	return EndpointKeyHost(link.Server, link.Port)
}

// EndpointKeyHost is EndpointKey for separate host and port values.
func EndpointKeyHost(server string, port int) string {
	host := normalizeHost(server)
	if host == "" || port < 1 || port > 65535 {
		return ""
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		host = addr.Unmap().String()
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// PrefixKey returns the /24 key for a literal IPv4 server, "host:<name>" for a
// hostname, and "" for an IPv6 literal or empty host. A hostname is its own
// failure domain here because no address is available without DNS.
func PrefixKey(link contract.Link) string {
	return PrefixKeyHost(link.Server)
}

// PrefixKeyHost is PrefixKey for a server host.
func PrefixKeyHost(server string) string {
	host := normalizeHost(server)
	if host == "" {
		return ""
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		addr = addr.Unmap()
		if !addr.Is4() {
			return ""
		}
		b := addr.As4()
		return netip.PrefixFrom(netip.AddrFrom4(b), 24).Masked().String()
	}
	return "host:" + host
}

// Counts summarizes the provider mix of a ready pool.
type Counts struct {
	Cloudflare    int
	NonCloudflare int
	Unknown       int
	// Domains is the number of distinct /24 (or hostname) keys.
	Domains int
	// SingleProvider is true when every ready entry is a known provider class
	// and all of them are the same. Unknown entries make it false because they
	// cannot be attributed safely.
	SingleProvider bool
}

// Summarize counts provider classes and failure domains in ready.
func Summarize(ready []contract.Ready) Counts {
	var c Counts
	domains := make(map[string]struct{}, len(ready))
	for _, r := range ready {
		switch ClassifyLink(r.Link) {
		case Cloudflare:
			c.Cloudflare++
		case NonCloudflare:
			c.NonCloudflare++
		default:
			c.Unknown++
		}
		if key := PrefixKey(r.Link); key != "" {
			domains[key] = struct{}{}
		}
	}
	c.Domains = len(domains)
	known := 0
	if c.Cloudflare > 0 {
		known++
	}
	if c.NonCloudflare > 0 {
		known++
	}
	// Unknown entries cannot be attributed safely; require every standby to be
	// known before calling the pool single-provider.
	c.SingleProvider = known == 1 && c.Unknown == 0
	return c
}

func normalizeHost(server string) string {
	host := strings.TrimSpace(server)
	host = strings.Trim(host, "[]")
	host = strings.TrimSuffix(host, ".")
	return strings.ToLower(host)
}

func isSpecial(addr netip.Addr) bool {
	if !addr.IsValid() {
		return true
	}
	if addr.IsUnspecified() || addr.IsLoopback() || addr.IsPrivate() ||
		addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() || addr.IsMulticast() {
		return true
	}
	for _, p := range reservedIPv4 {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}
