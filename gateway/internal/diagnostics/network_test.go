package diagnostics

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strconv"
	"strings"
	"testing"
	"time"
)

func fixtureTarget(t *testing.T, healthBody, rtspStatus string, wrongNonce bool) Target {
	t.Helper()
	health := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" || r.Header.Get("Authorization") != "" {
			t.Error("unexpected request")
		}
		w.Write([]byte(healthBody))
	}))
	t.Cleanup(health.Close)
	udp, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { udp.Close() })
	go func() {
		buffer := make([]byte, 128)
		n, peer, err := udp.ReadFrom(buffer)
		if err != nil {
			return
		}
		fields := strings.Fields(string(buffer[:n]))
		if len(fields) != 2 {
			return
		}
		nonce := fields[1]
		if wrongNonce {
			nonce = strings.Repeat("0", 32)
		}
		_, port, _ := net.SplitHostPort(health.Listener.Addr().String())
		udp.WriteTo([]byte("ZOMBIE_GATEWAY_V1 "+nonce+" "+port+"\n"), peer)
	}()
	rtsp, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rtsp.Close() })
	go func() {
		conn, err := rtsp.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(3 * time.Second))
		reader := textproto.NewReader(bufio.NewReader(conn))
		line, _ := reader.ReadLine()
		if !strings.HasPrefix(line, "OPTIONS rtsp://") {
			t.Error("unexpected RTSP request")
		}
		headers, _ := reader.ReadMIMEHeader()
		fmt.Fprintf(conn, "RTSP/1.0 %s\r\nCSeq: %s\r\nPublic: OPTIONS, DESCRIBE, SETUP, PLAY\r\n\r\n", rtspStatus, headers.Get("Cseq"))
	}()
	port := func(address string) int {
		_, value, _ := net.SplitHostPort(address)
		n, _ := strconv.Atoi(value)
		return n
	}
	return Target{"127.0.0.1", port(health.Listener.Addr().String()), port(udp.LocalAddr().String()), port(rtsp.Addr().String())}
}

func TestSelectedEndpointRoundTripsNeverGrantMediaCapabilities(t *testing.T) {
	target := fixtureTarget(t, `{"status":"ok","apiVersion":1}`, "200 OK", false)
	result, err := Probe(context.Background(), &net.Dialer{}, target)
	if err != nil || result.HTTP.State != "ok" || result.Discovery.State != "ok" || result.RTSP.State != "ok" {
		t.Fatal(result, err)
	}
	if result.MediaValidated || result.AccountValidated {
		t.Fatal("reachability promoted into media evidence")
	}
	data, _ := json.Marshal(result)
	if strings.Contains(string(data), "127.0.0.1") {
		t.Fatal("endpoint leaked into shareable report")
	}
}

func TestBadHealthNonceAndAuthenticationStayDistinct(t *testing.T) {
	target := fixtureTarget(t, `{"status":"ok","apiVersion":99}`, "401 Unauthorized", true)
	result, err := Probe(context.Background(), &net.Dialer{}, target)
	if err != nil || result.HTTP.State != "invalid_response" || result.Discovery.State != "invalid_response" || result.RTSP.State != "authentication_required" {
		t.Fatal(result, err)
	}
}

type noDial struct{ t *testing.T }

func (d noDial) DialContext(context.Context, string, string) (net.Conn, error) {
	d.t.Fatal("invalid target was dialed")
	return nil, io.EOF
}

func TestTargetsMustBeExplicitLocalIPv4AndPortsBounded(t *testing.T) {
	for _, address := range []string{"example.com", "8.8.8.8", "::1", "0.0.0.0", "224.0.0.1", "255.255.255.255"} {
		if _, err := Probe(context.Background(), noDial{t}, Target{address, 8090, 8098, 8554}); err == nil {
			t.Fatal(address)
		}
	}
	if _, err := Probe(context.Background(), noDial{t}, Target{"127.0.0.1", -1, 8098, 8554}); err == nil {
		t.Fatal("invalid port")
	}
}

type silentDial struct{}

func (silentDial) DialContext(ctx context.Context, _, _ string) (net.Conn, error) {
	client, server := net.Pipe()
	go func() { defer server.Close(); io.Copy(io.Discard, server) }()
	return client, nil
}

func TestCancellationBoundsAllThreeSilentPeers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	result, err := Probe(ctx, silentDial{}, Target{"127.0.0.1", 8090, 8098, 8554})
	if err != nil || time.Since(start) > time.Second {
		t.Fatal(result, err)
	}
	if result.HTTP.State != "unavailable" || result.RTSP.State != "unavailable" || result.Discovery.State != "unavailable" {
		t.Fatal(result)
	}
}
