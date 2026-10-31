package web

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ValidatePublicURL rejects non-http(s) schemes and hosts that resolve to
// loopback, link-local, private, or metadata addresses (basic SSRF guard).
func ValidatePublicURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fmt.Errorf("empty url")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid url: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("scheme %q not allowed (http/https only)", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("url missing host")
	}
	// Block obvious names without DNS.
	lower := strings.ToLower(host)
	switch lower {
	case "localhost", "metadata.google.internal", "metadata":
		return fmt.Errorf("host %q is not allowed", host)
	}
	if strings.HasSuffix(lower, ".localhost") || strings.HasSuffix(lower, ".local") {
		return fmt.Errorf("host %q is not allowed", host)
	}

	// Literal IP
	if ip := net.ParseIP(host); ip != nil {
		if isBlockedIP(ip) {
			return fmt.Errorf("address %s is not allowed", ip)
		}
		return nil
	}

	// Resolve hostname — block if any A/AAAA is private/link-local/etc.
	ips, err := net.LookupIP(host)
	if err != nil {
		// Fail closed on DNS errors for agent-initiated fetches.
		return fmt.Errorf("dns lookup failed for %q: %w", host, err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("no addresses for host %q", host)
	}
	for _, ip := range ips {
		if isBlockedIP(ip) {
			return fmt.Errorf("host %q resolves to blocked address %s", host, ip)
		}
	}
	return nil
}

func isBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}
	if ip.IsUnspecified() || ip.IsMulticast() {
		return true
	}
	// Cloud metadata ranges commonly used in SSRF.
	if ip4 := ip.To4(); ip4 != nil {
		// 169.254.0.0/16 already covered by link-local; be explicit for 169.254.169.254
		if ip4[0] == 169 && ip4[1] == 254 {
			return true
		}
		// 100.64.0.0/10 shared address space (CGNAT / some cloud internals)
		if ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127 {
			return true
		}
		// 0.0.0.0/8
		if ip4[0] == 0 {
			return true
		}
	}
	// IPv6 unique local fc00::/7
	if ip16 := ip.To16(); ip16 != nil && ip.To4() == nil {
		if (ip16[0] & 0xfe) == 0xfc {
			return true
		}
	}
	return false
}

// safeHTTPClient returns a client that re-checks redirect targets.
func safeHTTPClient(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("too many redirects")
			}
			if err := ValidatePublicURL(req.URL.String()); err != nil {
				return fmt.Errorf("redirect blocked: %w", err)
			}
			return nil
		},
	}
}
