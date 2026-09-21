package discovery

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

func TestResponseRejectsMalformedAndOversizedPackets(t *testing.T) {
	valid := "ZOMBIE_DISCOVER_V1 " + strings.Repeat("a", 32) + "\n"
	if got := string(Response([]byte(valid), 8090)); got != "ZOMBIE_GATEWAY_V1 "+strings.Repeat("a", 32)+" 8090\n" {
		t.Fatal(got)
	}
	for _, request := range []string{"", valid + "extra", strings.Replace(valid, "V1", "V2", 1), strings.Replace(valid, "a", "z", 1), strings.TrimSpace(valid)} {
		if Response([]byte(request), 8090) != nil {
			t.Fatalf("accepted %q", request)
		}
	}
	if Response([]byte(valid), 0) != nil || Response([]byte(valid), 65536) != nil {
		t.Fatal("invalid port accepted")
	}
}

func TestLocalAddressesOnly(t *testing.T) {
	for _, value := range []string{"192.168.1.4", "10.0.0.2", "172.16.0.1", "169.254.1.4", "127.0.0.1"} {
		if !local(net.ParseIP(value)) {
			t.Fatal(value)
		}
	}
	for _, value := range []string{"8.8.8.8", "224.0.0.1", "::1", "0.0.0.0"} {
		if local(net.ParseIP(value)) {
			t.Fatal(value)
		}
	}
}

func TestLoopbackResponseRateBoundAndCancellation(t *testing.T) {
	server, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, server, 8090) }()
	client, err := net.DialUDP("udp4", nil, server.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	request := []byte("ZOMBIE_DISCOVER_V1 " + strings.Repeat("b", 32) + "\n")
	for i := 0; i < 25; i++ {
		if _, err := client.Write(request); err != nil {
			t.Fatal(err)
		}
	}
	_ = client.SetReadDeadline(time.Now().Add(400 * time.Millisecond))
	count := 0
	buffer := make([]byte, 128)
	for {
		n, err := client.Read(buffer)
		if err != nil {
			break
		}
		if string(buffer[:n]) != string(Response(request, 8090)) {
			t.Fatal("unexpected response")
		}
		count++
	}
	if count != 20 {
		t.Fatalf("got %d replies, expected rate bound 20", count)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("discovery did not stop")
	}
}
