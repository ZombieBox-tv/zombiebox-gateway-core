// Package discovery provides credential-free local IPv4 gateway announcements.
package discovery

import (
	"context"
	"encoding/hex"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

const Port = 8098

// Response returns a small, nonce-bound locator, never pairing or account data.
func Response(request []byte, httpPort int) []byte {
	if httpPort < 1 || httpPort > 65535 || len(request) != len("ZOMBIE_DISCOVER_V1 ")+32+1 {
		return nil
	}
	value := string(request)
	if !strings.HasPrefix(value, "ZOMBIE_DISCOVER_V1 ") || !strings.HasSuffix(value, "\n") {
		return nil
	}
	nonce := strings.TrimSuffix(strings.TrimPrefix(value, "ZOMBIE_DISCOVER_V1 "), "\n")
	if _, err := hex.DecodeString(nonce); err != nil || nonce != strings.ToLower(nonce) {
		return nil
	}
	return []byte("ZOMBIE_GATEWAY_V1 " + nonce + " " + strconv.Itoa(httpPort) + "\n")
}

// Listen is opt-in. Ownership of the socket transfers to Serve.
func Listen(address string) (*net.UDPConn, error) {
	endpoint, err := net.ResolveUDPAddr("udp4", address)
	if err != nil {
		return nil, err
	}
	return net.ListenUDP("udp4", endpoint)
}

func local(ip net.IP) bool {
	return ip.To4() != nil && (ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLoopback())
}

// Serve uses one goroutine and a fixed packet buffer. A global bound avoids an
// attacker-controlled per-source map. Read deadlines bound cancellation latency.
func Serve(ctx context.Context, socket *net.UDPConn, httpPort int) error {
	defer socket.Close()
	if httpPort < 1 || httpPort > 65535 {
		return fmt.Errorf("invalid discovery HTTP port")
	}
	buffer := make([]byte, 128)
	window := time.Now()
	replies := 0
	for ctx.Err() == nil {
		if err := socket.SetReadDeadline(time.Now().Add(250 * time.Millisecond)); err != nil {
			return err
		}
		n, peer, err := socket.ReadFromUDP(buffer)
		if err != nil {
			if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
				continue
			}
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if !local(peer.IP) {
			continue
		}
		if time.Since(window) >= time.Second {
			window, replies = time.Now(), 0
		}
		if replies >= 20 {
			continue
		}
		response := Response(buffer[:n], httpPort)
		if response == nil {
			continue
		}
		replies++
		if err := socket.SetWriteDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
			return err
		}
		// A sender disappearing must not stop discovery for other devices.
		_, _ = socket.WriteToUDP(response, peer)
	}
	return nil
}
