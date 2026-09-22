// Package diagnostics measures protocol reachability without granting media capabilities.
package diagnostics

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Dialer interface {
	DialContext(context.Context, string, string) (net.Conn, error)
}

type Target struct {
	Address                           string
	HTTPPort, DiscoveryPort, RTSPPort int
}

type Result struct {
	State     string `json:"state"`
	ElapsedMS int64  `json:"elapsedMs"`
}

type Report struct {
	Version          int      `json:"reportVersion"`
	HTTP             Result   `json:"httpHealth"`
	Discovery        Result   `json:"discoveryUnicast"`
	RTSP             Result   `json:"rtspOptions"`
	MediaValidated   bool     `json:"mediaValidated"`
	AccountValidated bool     `json:"accountValidated"`
	Limitations      []string `json:"limitations"`
}

// Probe uses three bounded connections to one explicitly selected local IPv4 address.
// It never reads credentials/state, follows redirects, publishes media or pairs a device.
func Probe(ctx context.Context, dialer Dialer, target Target) (Report, error) {
	ip := net.ParseIP(target.Address)
	if ip == nil || ip.To4() == nil || strings.Contains(target.Address, ":") || (!ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast()) {
		return Report{}, errors.New("select a literal private, link-local or loopback IPv4 address")
	}
	for _, port := range []int{target.HTTPPort, target.DiscoveryPort, target.RTSPPort} {
		if port < 1 || port > 65535 {
			return Report{}, errors.New("ports must be between 1 and 65535")
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	report := Report{Version: 1, Limitations: []string{
		"Reachability is not media, account, decoder or hardware-acceleration validation.",
		"Discovery is unicast; multicast/broadcast across Wi-Fi isolation is not tested.",
		"RTSP OPTIONS does not validate authentication, publishing, RTP or playback.",
		"Loopback results do not establish reachability from another LAN device.",
	}}
	var jobs sync.WaitGroup
	run := func(result *Result, network string, port int, check func(net.Conn) string) {
		jobs.Add(1)
		go func() {
			defer jobs.Done()
			start := time.Now()
			result.State = "unavailable"
			defer func() { result.ElapsedMS = time.Since(start).Milliseconds() }()
			conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(target.Address, strconv.Itoa(port)))
			if err != nil {
				return
			}
			defer conn.Close()
			stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
			defer stop()
			deadline, _ := ctx.Deadline()
			if conn.SetDeadline(deadline) != nil {
				return
			}
			result.State = check(conn)
		}()
	}
	run(&report.HTTP, "tcp4", target.HTTPPort, func(conn net.Conn) string { return health(conn, target) })
	run(&report.Discovery, "udp4", target.DiscoveryPort, func(conn net.Conn) string { return discovery(conn, target.HTTPPort) })
	run(&report.RTSP, "tcp4", target.RTSPPort, func(conn net.Conn) string { return rtsp(conn, target) })
	jobs.Wait()
	return report, nil
}

func health(conn net.Conn, target Target) string {
	host := net.JoinHostPort(target.Address, strconv.Itoa(target.HTTPPort))
	request, err := http.NewRequest("GET", "http://"+host+"/health", nil)
	if err != nil || request.Write(conn) != nil {
		return "unavailable"
	}
	response, err := http.ReadResponse(bufio.NewReader(io.LimitReader(conn, 16<<10)), request)
	if err != nil {
		return readFailure(err)
	}
	defer response.Body.Close()
	if response.StatusCode == 401 || response.StatusCode == 403 {
		return "authentication_required"
	}
	if response.StatusCode != 200 {
		return "unavailable"
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 513))
	var value struct {
		Status  string `json:"status"`
		Version int    `json:"apiVersion"`
	}
	if err != nil {
		return readFailure(err)
	}
	if len(data) > 512 || json.Unmarshal(data, &value) != nil || value.Status != "ok" || value.Version != 1 {
		return "invalid_response"
	}
	return "ok"
}

func discovery(conn net.Conn, port int) string {
	var nonceBytes [16]byte
	if _, err := rand.Read(nonceBytes[:]); err != nil {
		return "unavailable"
	}
	nonce := hex.EncodeToString(nonceBytes[:])
	if _, err := io.WriteString(conn, "ZOMBIE_DISCOVER_V1 "+nonce+"\n"); err != nil {
		return "unavailable"
	}
	buffer := make([]byte, 129)
	n, err := conn.Read(buffer)
	if err != nil {
		return "unavailable"
	}
	if n > 128 || !strings.HasSuffix(string(buffer[:n]), "\n") {
		return "invalid_response"
	}
	fields := strings.Fields(string(buffer[:n]))
	if len(fields) != 3 || fields[0] != "ZOMBIE_GATEWAY_V1" || fields[1] != nonce {
		return "invalid_response"
	}
	advertised, err := strconv.Atoi(fields[2])
	if err != nil || advertised < 1 || advertised > 65535 {
		return "invalid_response"
	}
	if advertised != port {
		return "port_mismatch"
	}
	return "ok"
}

func rtsp(conn net.Conn, target Target) string {
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "unavailable"
	}
	sequence := strconv.FormatUint(uint64(nonce[0])<<8|uint64(nonce[1]), 10)
	host := net.JoinHostPort(target.Address, strconv.Itoa(target.RTSPPort))
	if _, err := fmt.Fprintf(conn, "OPTIONS rtsp://%s/ RTSP/1.0\r\nCSeq: %s\r\nUser-Agent: ZombieBox-Diagnostics\r\n\r\n", host, sequence); err != nil {
		return "unavailable"
	}
	reader := textproto.NewReader(bufio.NewReader(io.LimitReader(conn, 8192)))
	line, err := reader.ReadLine()
	if err != nil {
		return readFailure(err)
	}
	status := strings.Fields(line)
	if len(status) < 2 || status[0] != "RTSP/1.0" {
		return "invalid_response"
	}
	headers, err := reader.ReadMIMEHeader()
	if err != nil {
		return readFailure(err)
	}
	if len(headers.Values("Cseq")) != 1 || headers.Get("Cseq") != sequence {
		return "invalid_response"
	}
	if status[1] == "401" || status[1] == "403" {
		return "authentication_required"
	}
	if status[1] != "200" {
		return "unavailable"
	}
	methods := map[string]bool{}
	for _, method := range strings.Split(headers.Get("Public"), ",") {
		methods[strings.TrimSpace(method)] = true
	}
	if !methods["DESCRIBE"] || !methods["SETUP"] || !methods["PLAY"] {
		return "invalid_response"
	}
	return "ok"
}

func readFailure(err error) string {
	var timeout net.Error
	if errors.Is(err, net.ErrClosed) || errors.Is(err, io.ErrClosedPipe) || (errors.As(err, &timeout) && timeout.Timeout()) {
		return "unavailable"
	}
	return "invalid_response"
}
