// Package httpclient defines outbound network policy at the infrastructure boundary.
package httpclient

import (
	"context"
	"net"
	"net/http"
	"time"

	"zombiebox.local/gateway/internal/providers"
)

func Metadata() *http.Client {
	return &http.Client{Timeout: 5 * time.Second, CheckRedirect: providers.SafeRedirect}
}
func Private() *http.Client {
	return &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

type mediaConn struct{ net.Conn }

func (c mediaConn) Read(b []byte) (int, error) {
	_ = c.SetReadDeadline(time.Now().Add(30 * time.Second))
	return c.Conn.Read(b)
}

func Streaming() *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			c, e := (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext(ctx, network, address)
			if e != nil {
				return nil, e
			}
			return mediaConn{c}, nil
		},
		TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 8 * time.Second, IdleConnTimeout: 30 * time.Second, MaxIdleConns: 8, MaxConnsPerHost: 4,
	}, CheckRedirect: providers.SafeRedirect}
}
