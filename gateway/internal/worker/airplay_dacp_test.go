package worker

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
)

const (
	testDACPIdentifier = "0123456789ABCDEF"
	testActiveRemote   = "1234567890"
)

type fakeDACPResolver struct {
	services []DACPService
	err      error
	instance string
	calls    int
	lookup   func(context.Context) ([]DACPService, error)
}

func (r *fakeDACPResolver) Lookup(ctx context.Context, instance string) ([]DACPService, error) {
	r.instance = instance
	r.calls++
	if r.lookup != nil {
		return r.lookup(ctx)
	}
	return r.services, r.err
}

type dacpRoundTripper func(*http.Request) (*http.Response, error)

func (f dacpRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func writeDACPCredentials(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "receiver.dacp")
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func matchingDACPService(ipv4 string) DACPService {
	return DACPService{
		Instance: "iTunes_Ctrl_" + testDACPIdentifier,
		Service:  dacpServiceType,
		Domain:   dacpServiceDomain,
		HostName: "untrusted-advertisement.example",
		Port:     50200,
		IPv4:     []net.IP{net.ParseIP(ipv4)},
	}
}

func response(status int, body string, request *http.Request) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    request,
	}
}

func TestReadDACPCredentialsRejectsUnsafeOrMalformedFiles(t *testing.T) {
	tests := []struct {
		name     string
		contents string
	}{
		{name: "missing second line", contents: testDACPIdentifier + "\n"},
		{name: "extra line", contents: testDACPIdentifier + "\n" + testActiveRemote + "\nextra\n"},
		{name: "short identifier", contents: "0123\n" + testActiveRemote + "\n"},
		{name: "non-hex identifier", contents: "0123456789ABCDEG\n" + testActiveRemote + "\n"},
		{name: "non-numeric remote", contents: testDACPIdentifier + "\nremote-key\n"},
		{name: "zero remote", contents: testDACPIdentifier + "\n0\n"},
		{name: "overflow remote", contents: testDACPIdentifier + "\n18446744073709551616\n"},
		{name: "oversized", contents: testDACPIdentifier + "\n" + strings.Repeat("1", dacpFileLimit) + "\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := writeDACPCredentials(t, test.contents)
			if _, err := readDACPCredentials(path); !errors.Is(err, errInvalidDACPFile) {
				t.Fatalf("readDACPCredentials() error = %v, want invalid file", err)
			}
		})
	}

	t.Run("symlink", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "credentials")
		if err := os.WriteFile(target, []byte(testDACPIdentifier+"\n"+testActiveRemote+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "receiver.dacp")
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
		if _, err := readDACPCredentials(path); !errors.Is(err, errInvalidDACPFile) {
			t.Fatalf("readDACPCredentials() error = %v, want symlink rejection", err)
		}
	})

	t.Run("directory", func(t *testing.T) {
		if _, err := readDACPCredentials(t.TempDir()); !errors.Is(err, errInvalidDACPFile) {
			t.Fatalf("readDACPCredentials() error = %v, want non-regular file rejection", err)
		}
	})
}

func TestReadDACPCredentialsAcceptsUxPlayTwoLineFormat(t *testing.T) {
	path := writeDACPCredentials(t, testDACPIdentifier+"\r\n"+testActiveRemote+"\r\n")
	credentials, err := readDACPCredentials(path)
	if err != nil {
		t.Fatalf("readDACPCredentials() error = %v", err)
	}
	if credentials.id != testDACPIdentifier || credentials.activeRemote != testActiveRemote {
		t.Fatalf("parsed credentials = (%q, %q)", credentials.id, credentials.activeRemote)
	}
}

func TestDACPControllerMatchesExactServiceAndUsesValidatedIPv4(t *testing.T) {
	wrongID := matchingDACPService("192.168.1.20")
	wrongID.Instance = "iTunes_Ctrl_0000000000000000"
	wrongType := matchingDACPService("192.168.1.21")
	wrongType.Service = "_http._tcp"
	wrongDomain := matchingDACPService("192.168.1.22")
	wrongDomain.Domain = "example.org"
	resolver := &fakeDACPResolver{services: []DACPService{
		wrongID,
		wrongType,
		wrongDomain,
		matchingDACPService("192.168.1.23"),
	}}
	var requestHost, requestPath, remoteHeader string
	client := &http.Client{Transport: dacpRoundTripper(func(request *http.Request) (*http.Response, error) {
		requestHost = request.URL.Host
		requestPath = request.URL.Path
		remoteHeader = request.Header.Get("Active-Remote")
		return response(http.StatusNoContent, "", request), nil
	})}
	controller := newDACPControllerWithDependencies(
		writeDACPCredentials(t, testDACPIdentifier+"\n"+testActiveRemote+"\n"),
		resolver,
		client,
	)
	if err := controller.Send(context.Background(), DACPNextItem); err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	if resolver.instance != "iTunes_Ctrl_"+testDACPIdentifier {
		t.Fatalf("Lookup instance = %q", resolver.instance)
	}
	if resolver.calls != 1 {
		t.Fatalf("Lookup calls = %d, want 1", resolver.calls)
	}
	if requestHost != "192.168.1.23:50200" {
		t.Fatalf("request host = %q, want validated IP literal", requestHost)
	}
	if requestPath != "/ctrl-int/1/nextitem" {
		t.Fatalf("request path = %q", requestPath)
	}
	if remoteHeader != testActiveRemote {
		t.Fatal("Active-Remote was not sent to the validated service")
	}
}

func TestDACPControllerAbortsWhenCredentialsChangeDuringDiscovery(t *testing.T) {
	path := writeDACPCredentials(t, testDACPIdentifier+"\n"+testActiveRemote+"\n")
	resolver := &fakeDACPResolver{lookup: func(context.Context) ([]DACPService, error) {
		if err := os.WriteFile(path, []byte(testDACPIdentifier+"\n9876543210\n"), 0600); err != nil {
			t.Fatal(err)
		}
		return []DACPService{matchingDACPService("192.168.1.30")}, nil
	}}
	requests := 0
	client := &http.Client{Transport: dacpRoundTripper(func(request *http.Request) (*http.Response, error) {
		requests++
		return response(http.StatusNoContent, "", request), nil
	})}
	controller := newDACPControllerWithDependencies(path, resolver, client)
	if err := controller.Send(context.Background(), DACPNextItem); !errors.Is(err, errDACPChanged) {
		t.Fatalf("Send() error = %v, want changed-session rejection", err)
	}
	if requests != 0 {
		t.Fatalf("HTTP requests = %d, want 0 for changed credentials", requests)
	}
}

func TestDACPResolverCombinesIPv6FirstSRVAndLaterPrivateIPv4Record(t *testing.T) {
	instance := "iTunes_Ctrl_" + testDACPIdentifier
	serviceName := dns.Fqdn(instance + "." + dacpServiceType + "." + dacpServiceDomain)
	state := &dacpDNSServiceState{serviceName: serviceName}

	firstAdvertisement := &dns.Msg{Answer: []dns.RR{
		&dns.SRV{
			Hdr:  dns.RR_Header{Name: serviceName, Rrtype: dns.TypeSRV, Class: dns.ClassINET, Ttl: 120},
			Port: 50200, Target: "ipad.local.",
		},
		&dns.AAAA{
			Hdr:  dns.RR_Header{Name: "ipad.local.", Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: 120},
			AAAA: net.ParseIP("fd00::40"),
		},
	}}
	state.observe(firstAdvertisement)
	if state.target != "ipad.local." || state.port != 50200 || state.address.IsValid() {
		t.Fatalf("IPv6-first advertisement state = %+v, want service identity without usable IPv4", state)
	}

	secondAdvertisement := &dns.Msg{Answer: []dns.RR{
		&dns.A{
			Hdr: dns.RR_Header{Name: "ipad.local.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 120},
			A:   net.IPv4(192, 168, 1, 40),
		},
	}}
	state.observe(secondAdvertisement)
	if !state.address.IsValid() || state.address != netip.MustParseAddr("192.168.1.40") {
		t.Fatalf("later IPv4 address = %v, want private 192.168.1.40", state.address)
	}
}

func TestDACPResolverQueriesOnlyExactServiceName(t *testing.T) {
	instance := "iTunes_Ctrl_" + testDACPIdentifier
	wantName := dns.Fqdn(instance + "." + dacpServiceType + "." + dacpServiceDomain)
	packet, err := makeDACPQuestionPacket(wantName, dns.TypeSRV)
	if err != nil {
		t.Fatalf("makeDACPQuestionPacket() error = %v", err)
	}
	var question dns.Msg
	if err := question.Unpack(packet); err != nil {
		t.Fatalf("decode mDNS question: %v", err)
	}
	if question.Id != 0 || question.Response || question.RecursionDesired || len(question.Question) != 1 {
		t.Fatalf("unexpected mDNS question header: %+v", question.MsgHdr)
	}
	if question.Question[0].Name != wantName || question.Question[0].Qtype != dns.TypeSRV || question.Question[0].Qclass != dns.ClassINET {
		t.Fatalf("mDNS question = %+v, want exact service SRV query for %s", question.Question[0], wantName)
	}
}

func TestDACPResolverRejectsMismatchedServiceAndNonPrivateIPv4(t *testing.T) {
	instance := "iTunes_Ctrl_" + testDACPIdentifier
	serviceName := dns.Fqdn(instance + "." + dacpServiceType + "." + dacpServiceDomain)
	state := &dacpDNSServiceState{serviceName: serviceName}
	wrongService := &dns.Msg{Answer: []dns.RR{
		&dns.SRV{
			Hdr:  dns.RR_Header{Name: dns.Fqdn("iTunes_Ctrl_0000000000000000." + dacpServiceType + "." + dacpServiceDomain), Rrtype: dns.TypeSRV, Class: dns.ClassINET},
			Port: 50200, Target: "ipad.local.",
		},
		&dns.A{
			Hdr: dns.RR_Header{Name: "ipad.local.", Rrtype: dns.TypeA, Class: dns.ClassINET},
			A:   net.IPv4(192, 168, 1, 40),
		},
	}}
	state.observe(wrongService)
	if state.target != "" || state.address.IsValid() {
		t.Fatalf("mismatched DACP ID was accepted: %+v", state)
	}

	state.target = "ipad.local."
	state.port = 50200
	publicAddress := &dns.Msg{Answer: []dns.RR{
		&dns.A{
			Hdr: dns.RR_Header{Name: "ipad.local.", Rrtype: dns.TypeA, Class: dns.ClassINET},
			A:   net.ParseIP("8.8.8.8"),
		},
	}}
	state.observe(publicAddress)
	if state.address.IsValid() {
		t.Fatalf("public IPv4 address was accepted: %v", state.address)
	}
}

func TestMDNSDACPResolverHonorsPreCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (mdnsDACPResolver{}).Lookup(ctx, "iTunes_Ctrl_"+testDACPIdentifier); !errors.Is(err, context.Canceled) {
		t.Fatalf("Lookup() error = %v, want context cancellation", err)
	}
}

func TestDACPControllerAcceptsEachFiniteCommandPath(t *testing.T) {
	for command, wantPath := range dacpCommandPaths {
		t.Run(string(command), func(t *testing.T) {
			resolver := &fakeDACPResolver{services: []DACPService{matchingDACPService("192.168.1.28")}}
			var gotPath string
			client := &http.Client{Transport: dacpRoundTripper(func(request *http.Request) (*http.Response, error) {
				gotPath = request.URL.Path
				return response(http.StatusNoContent, "", request), nil
			})}
			controller := newDACPControllerWithDependencies(
				writeDACPCredentials(t, testDACPIdentifier+"\n"+testActiveRemote+"\n"), resolver, client,
			)
			if err := controller.Send(context.Background(), command); err != nil {
				t.Fatalf("Send() error = %v", err)
			}
			if gotPath != "/ctrl-int/1/"+wantPath {
				t.Fatalf("request path = %q, want %q", gotPath, "/ctrl-int/1/"+wantPath)
			}
		})
	}
}

func TestDACPControllerRejectsUntrustedServiceAddressesBeforeSendingToken(t *testing.T) {
	tests := []struct {
		name    string
		service DACPService
	}{
		{name: "public", service: matchingDACPService("8.8.8.8")},
		{name: "loopback", service: matchingDACPService("127.0.0.1")},
		{name: "unspecified", service: matchingDACPService("0.0.0.0")},
		{name: "multicast", service: matchingDACPService("224.0.0.1")},
		{name: "carrier-grade NAT", service: matchingDACPService("100.64.0.1")},
		{name: "invalid port", service: func() DACPService {
			service := matchingDACPService("192.168.1.24")
			service.Port = 65536
			return service
		}()},
		{name: "IPv6 only", service: func() DACPService {
			service := matchingDACPService("192.168.1.25")
			service.IPv4 = nil
			return service
		}()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resolver := &fakeDACPResolver{services: []DACPService{test.service}}
			requests := 0
			client := &http.Client{Transport: dacpRoundTripper(func(request *http.Request) (*http.Response, error) {
				requests++
				return response(http.StatusNoContent, "", request), nil
			})}
			controller := newDACPControllerWithDependencies(
				writeDACPCredentials(t, testDACPIdentifier+"\n"+testActiveRemote+"\n"), resolver, client,
			)
			if err := controller.Send(context.Background(), DACPPlayPause); !errors.Is(err, errDACPUnavailable) {
				t.Fatalf("Send() error = %v, want unavailable", err)
			}
			if requests != 0 {
				t.Fatalf("HTTP requests = %d, want 0", requests)
			}
		})
	}

	if isDACPPrivateAddress(netip.MustParseAddr("169.254.3.4")) != true {
		t.Fatal("link-local IPv4 should be accepted as LAN scope")
	}
}

func TestDACPControllerAllowsOnlyFiniteCommands(t *testing.T) {
	resolver := &fakeDACPResolver{}
	controller := newDACPControllerWithDependencies(writeDACPCredentials(t, testDACPIdentifier+"\n"+testActiveRemote+"\n"), resolver, nil)
	for _, command := range []DACPCommand{"", "volumeup", "../nextitem", "nextitem?host=attacker"} {
		if err := controller.Send(context.Background(), command); !errors.Is(err, errDACPCommand) {
			t.Errorf("Send(%q) error = %v, want command rejection", command, err)
		}
	}
	if resolver.calls != 0 {
		t.Fatalf("Lookup calls = %d, want 0 for rejected commands", resolver.calls)
	}
}

func TestDACPControllerRefusesRedirects(t *testing.T) {
	resolver := &fakeDACPResolver{services: []DACPService{matchingDACPService("192.168.1.26")}}
	requests := 0
	client := &http.Client{Transport: dacpRoundTripper(func(request *http.Request) (*http.Response, error) {
		requests++
		if request.Header.Get("Active-Remote") != testActiveRemote {
			t.Fatal("initial validated request did not include Active-Remote")
		}
		redirect := response(http.StatusFound, "", request)
		redirect.Header.Set("Location", "http://203.0.113.80:80/collect")
		return redirect, nil
	})}
	controller := newDACPControllerWithDependencies(
		writeDACPCredentials(t, testDACPIdentifier+"\n"+testActiveRemote+"\n"), resolver, client,
	)
	if err := controller.Send(context.Background(), DACPNextItem); !errors.Is(err, errDACPResponse) {
		t.Fatalf("Send() error = %v, want redirect response rejection", err)
	}
	if requests != 1 {
		t.Fatalf("RoundTrip calls = %d, want 1 without following redirect", requests)
	}
}

func TestDACPControllerBoundsResponseBody(t *testing.T) {
	resolver := &fakeDACPResolver{services: []DACPService{matchingDACPService("192.168.1.29")}}
	client := &http.Client{Transport: dacpRoundTripper(func(request *http.Request) (*http.Response, error) {
		return response(http.StatusOK, strings.Repeat("x", dacpResponseLimit+1), request), nil
	})}
	controller := newDACPControllerWithDependencies(
		writeDACPCredentials(t, testDACPIdentifier+"\n"+testActiveRemote+"\n"), resolver, client,
	)
	if err := controller.Send(context.Background(), DACPPlayPause); !errors.Is(err, errDACPResponse) {
		t.Fatalf("Send() error = %v, want bounded response rejection", err)
	}
}

func TestDACPControllerHonorsContextCancellationDuringLookup(t *testing.T) {
	entered := make(chan struct{})
	resolver := &fakeDACPResolver{lookup: func(ctx context.Context) ([]DACPService, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	controller := newDACPControllerWithDependencies(
		writeDACPCredentials(t, testDACPIdentifier+"\n"+testActiveRemote+"\n"), resolver, nil,
	)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- controller.Send(ctx, DACPNextItem) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("resolver lookup did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Send() error = %v, want context cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Send() did not stop after context cancellation")
	}
}

func TestDACPControllerHonorsContextCancellationDuringRequest(t *testing.T) {
	resolver := &fakeDACPResolver{services: []DACPService{matchingDACPService("192.168.1.27")}}
	started := make(chan struct{})
	client := &http.Client{Transport: dacpRoundTripper(func(request *http.Request) (*http.Response, error) {
		close(started)
		<-request.Context().Done()
		return nil, request.Context().Err()
	})}
	controller := newDACPControllerWithDependencies(
		writeDACPCredentials(t, testDACPIdentifier+"\n"+testActiveRemote+"\n"), resolver, client,
	)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- controller.Send(ctx, DACPPlayPause) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("request did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Send() error = %v, want context cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Send() did not stop after request cancellation")
	}
}
