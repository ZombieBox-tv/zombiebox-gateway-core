package httpclient

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"
)

// PublicDownloads is isolated from provider clients, which may intentionally
// address configured LAN servers. Resolve and pin each dial, including redirects;
// a shared URL cannot reach gateway/admin/loopback services through DNS rebinding.
func PublicDownloads() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                  nil,
			DialContext:            publicDial,
			TLSHandshakeTimeout:    5 * time.Second,
			ResponseHeaderTimeout:  8 * time.Second,
			MaxResponseHeaderBytes: 32 << 10,
			MaxConnsPerHost:        2,
			IdleConnTimeout:        20 * time.Second,
		},
		CheckRedirect: func(r *http.Request, via []*http.Request) error {
			if len(via) >= 5 || r.URL.User != nil || (r.URL.Scheme != "http" && r.URL.Scheme != "https") {
				return errors.New("invalid media redirect")
			}
			return nil
		},
	}
}

func publicIP(ip net.IP) bool {
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	for _, cidr := range []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "2001::/23", "2001:db8::/32", "64:ff9b::/96", "64:ff9b:1::/48", "2002::/16"} {
		_, block, _ := net.ParseCIDR(cidr)
		if block.Contains(ip) {
			return false
		}
	}
	return true
}

func publicDial(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil || len(ips) == 0 {
		return nil, errors.New("media host unavailable")
	}
	for _, item := range ips {
		if !publicIP(item.IP) {
			return nil, errors.New("media host is not public")
		}
	}
	var last error
	for _, item := range ips {
		conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, net.JoinHostPort(item.IP.String(), port))
		if err == nil {
			return mediaConn{conn}, nil
		}
		last = err
	}
	return nil, last
}
