package worker

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/miekg/dns"
	"golang.org/x/net/ipv4"
	"golang.org/x/sys/unix"
)

const (
	dacpFileLimit        = 128
	dacpServiceType      = "_dacp._tcp"
	dacpServiceDomain    = "local."
	dacpDiscoveryTimeout = 900 * time.Millisecond
	dacpRequestTimeout   = 1500 * time.Millisecond
	dacpResponseLimit    = 4 << 10
	dacpDNSPacketLimit   = 9 << 10
	dacpDNSMessageLimit  = 64
	dacpInterfaceLimit   = 8
	dacpDNSQueryLimit    = 3
)

var dacpMDNSGroupIPv4 = net.IPv4(224, 0, 0, 251)

var (
	errInvalidDACPFile = errors.New("invalid AirPlay DACP credentials")
	errDACPUnavailable = errors.New("AirPlay DACP target unavailable")
	errDACPCommand     = errors.New("unsupported AirPlay DACP command")
	errDACPRequest     = errors.New("AirPlay DACP request failed")
	errDACPResponse    = errors.New("invalid AirPlay DACP response")
	errDACPChanged     = errors.New("AirPlay DACP session changed")
)

// DACPCommand is deliberately limited to the controls that the client exposes.
type DACPCommand string

const (
	DACPPlayPause    DACPCommand = "playpause"
	DACPNextItem     DACPCommand = "nextitem"
	DACPPreviousItem DACPCommand = "previtem"
)

// DACPService is the small, provider-neutral subset of a resolved DNS-SD entry.
// HostName is retained only to make it explicit that HTTP must use an IP address.
type DACPService struct {
	Instance string
	Service  string
	Domain   string
	HostName string
	Port     int
	IPv4     []net.IP
}

// DACPResolver resolves exactly one DNS-SD instance name.
type DACPResolver interface {
	Lookup(ctx context.Context, instance string) ([]DACPService, error)
}

// DACPController sends a finite set of commands to the Apple sender that opened
// the active UxPlay session. The Active-Remote value never leaves this adapter.
type DACPController struct {
	credentialPath string
	resolver       DACPResolver
	client         *http.Client
}

type dacpCredentials struct {
	id           string
	activeRemote string
}

type dacpCredentialSnapshot struct {
	credentials dacpCredentials
	fingerprint [32]byte
}

// NewDACPController constructs an adapter with pure-Go mDNS/DNS-SD resolution
// and a direct, no-proxy HTTP transport.
func NewDACPController(credentialPath string) *DACPController {
	return newDACPControllerWithDependencies(credentialPath, mdnsDACPResolver{}, nil)
}

// newDACPControllerWithDependencies keeps discovery and HTTP deterministic in
// package tests. Production construction uses NewDACPController's direct
// no-proxy transport rather than accepting an arbitrary RoundTripper.
func newDACPControllerWithDependencies(credentialPath string, resolver DACPResolver, client *http.Client) *DACPController {
	if resolver == nil {
		resolver = mdnsDACPResolver{}
	}
	return &DACPController{
		credentialPath: credentialPath,
		resolver:       resolver,
		client:         safeDACPHTTPClient(client),
	}
}

// Send reads UxPlay's private transient credentials, resolves only the exact
// matching iTunes_Ctrl_<DACP-ID> service, and sends one allowlisted command.
func (c *DACPController) Send(ctx context.Context, command DACPCommand) error {
	if _, ok := dacpCommandPaths[command]; !ok {
		return errDACPCommand
	}
	if c == nil || c.resolver == nil || c.client == nil || c.credentialPath == "" {
		return errDACPUnavailable
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	initialSnapshot, err := readDACPCredentialSnapshot(c.credentialPath)
	if err != nil {
		return errInvalidDACPFile
	}

	instance := "iTunes_Ctrl_" + initialSnapshot.credentials.id
	discoveryCtx, cancelDiscovery := context.WithTimeout(ctx, dacpDiscoveryTimeout)
	services, err := c.resolver.Lookup(discoveryCtx, instance)
	cancelDiscovery()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return errDACPUnavailable
	}
	address, port, ok := matchingDACPAddress(services, instance)
	if !ok {
		return errDACPUnavailable
	}
	currentSnapshot, err := readDACPCredentialSnapshot(c.credentialPath)
	if err != nil || subtle.ConstantTimeCompare(initialSnapshot.fingerprint[:], currentSnapshot.fingerprint[:]) != 1 {
		return errDACPChanged
	}

	path, _ := dacpCommandPaths[command]
	requestCtx, cancelRequest := context.WithTimeout(ctx, dacpRequestTimeout)
	defer cancelRequest()
	endpoint := url.URL{
		Scheme: "http",
		Host:   net.JoinHostPort(address.String(), strconv.Itoa(port)),
		Path:   "/ctrl-int/1/" + path,
	}
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return errDACPRequest
	}
	request.Header.Set("Active-Remote", currentSnapshot.credentials.activeRemote)

	response, err := c.client.Do(request)
	if err != nil {
		if requestCtx.Err() != nil {
			return requestCtx.Err()
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return errDACPRequest
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, dacpResponseLimit+1))
	if err != nil || len(body) > dacpResponseLimit {
		return errDACPResponse
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return errDACPResponse
	}
	return nil
}

var dacpCommandPaths = map[DACPCommand]string{
	DACPPlayPause:    "playpause",
	DACPNextItem:     "nextitem",
	DACPPreviousItem: "previtem",
}

func safeDACPHTTPClient(injected *http.Client) *http.Client {
	client := &http.Client{
		Transport: &http.Transport{Proxy: nil},
		Timeout:   dacpRequestTimeout,
	}
	if injected != nil {
		*client = *injected
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	if client.Timeout == 0 || client.Timeout > dacpRequestTimeout {
		client.Timeout = dacpRequestTimeout
	}
	switch transport := client.Transport.(type) {
	case nil:
		client.Transport = &http.Transport{Proxy: nil}
	case *http.Transport:
		cloned := transport.Clone()
		cloned.Proxy = nil
		client.Transport = cloned
	}
	return client
}

func readDACPCredentials(path string) (dacpCredentials, error) {
	snapshot, err := readDACPCredentialSnapshot(path)
	if err != nil {
		return dacpCredentials{}, err
	}
	return snapshot.credentials, nil
}

func readDACPCredentialSnapshot(path string) (dacpCredentialSnapshot, error) {
	fileDescriptor, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return dacpCredentialSnapshot{}, errInvalidDACPFile
	}
	file := os.NewFile(uintptr(fileDescriptor), path)
	if file == nil {
		_ = unix.Close(fileDescriptor)
		return dacpCredentialSnapshot{}, errInvalidDACPFile
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > dacpFileLimit {
		return dacpCredentialSnapshot{}, errInvalidDACPFile
	}
	var before unix.Stat_t
	if err := unix.Fstat(fileDescriptor, &before); err != nil {
		return dacpCredentialSnapshot{}, errInvalidDACPFile
	}
	data, err := io.ReadAll(io.LimitReader(file, dacpFileLimit+1))
	if err != nil || len(data) > dacpFileLimit {
		return dacpCredentialSnapshot{}, errInvalidDACPFile
	}
	var after unix.Stat_t
	if err := unix.Fstat(fileDescriptor, &after); err != nil || !sameDACPFileVersion(before, after) {
		return dacpCredentialSnapshot{}, errInvalidDACPFile
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != 2 {
		return dacpCredentialSnapshot{}, errInvalidDACPFile
	}
	id := strings.TrimSuffix(lines[0], "\r")
	activeRemote := strings.TrimSuffix(lines[1], "\r")
	if !validDACPIdentifier(id) || !validActiveRemote(activeRemote) {
		return dacpCredentialSnapshot{}, errInvalidDACPFile
	}
	return dacpCredentialSnapshot{
		credentials: dacpCredentials{id: id, activeRemote: activeRemote},
		fingerprint: fingerprintDACPCredentials(data, after),
	}, nil
}

func sameDACPFileVersion(left, right unix.Stat_t) bool {
	return left.Dev == right.Dev && left.Ino == right.Ino && left.Size == right.Size &&
		left.Mtim.Sec == right.Mtim.Sec && left.Mtim.Nsec == right.Mtim.Nsec &&
		left.Ctim.Sec == right.Ctim.Sec && left.Ctim.Nsec == right.Ctim.Nsec
}

func fingerprintDACPCredentials(data []byte, stat unix.Stat_t) [32]byte {
	var identity [56]byte
	binary.LittleEndian.PutUint64(identity[0:8], uint64(stat.Dev))
	binary.LittleEndian.PutUint64(identity[8:16], uint64(stat.Ino))
	binary.LittleEndian.PutUint64(identity[16:24], uint64(stat.Size))
	binary.LittleEndian.PutUint64(identity[24:32], uint64(stat.Mtim.Sec))
	binary.LittleEndian.PutUint64(identity[32:40], uint64(stat.Mtim.Nsec))
	binary.LittleEndian.PutUint64(identity[40:48], uint64(stat.Ctim.Sec))
	binary.LittleEndian.PutUint64(identity[48:56], uint64(stat.Ctim.Nsec))
	hash := sha256.New()
	_, _ = hash.Write(identity[:])
	_, _ = hash.Write(data)
	var fingerprint [32]byte
	copy(fingerprint[:], hash.Sum(nil))
	return fingerprint
}

func validDACPIdentifier(value string) bool {
	if len(value) != 16 {
		return false
	}
	for _, character := range value {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f') || (character >= 'A' && character <= 'F')) {
			return false
		}
	}
	return true
}

func validActiveRemote(value string) bool {
	if value == "" || len(value) > 20 {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	number, err := strconv.ParseUint(value, 10, 64)
	return err == nil && number != 0
}

func matchingDACPAddress(services []DACPService, wantedInstance string) (netip.Addr, int, bool) {
	for _, service := range services {
		if !strings.EqualFold(strings.TrimSuffix(service.Instance, "."), wantedInstance) ||
			!strings.EqualFold(strings.TrimSuffix(service.Service, "."), dacpServiceType) ||
			!strings.EqualFold(strings.TrimSuffix(service.Domain, "."), strings.TrimSuffix(dacpServiceDomain, ".")) ||
			service.Port < 1 || service.Port > 65535 {
			continue
		}
		for _, candidate := range service.IPv4 {
			address, ok := netip.AddrFromSlice(candidate)
			if !ok {
				continue
			}
			address = address.Unmap()
			if !address.Is4() || !isDACPPrivateAddress(address) {
				continue
			}
			return address, service.Port, true
		}
	}
	return netip.Addr{}, 0, false
}

func isDACPPrivateAddress(address netip.Addr) bool {
	if !address.IsValid() || address.IsLoopback() || address.IsUnspecified() || address.IsMulticast() {
		return false
	}
	return address.IsPrivate() || address.IsLinkLocalUnicast()
}

type mdnsDACPResolver struct{}

type dacpDNSDiscoveryResult struct {
	service *DACPService
	err     error
}

type dacpDNSServiceState struct {
	serviceName string
	target      string
	port        int
	address     netip.Addr
}

func (mdnsDACPResolver) Lookup(ctx context.Context, instance string) ([]DACPService, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	identifier := strings.TrimPrefix(instance, "iTunes_Ctrl_")
	if !validDACPIdentifier(identifier) || !strings.EqualFold(instance, "iTunes_Ctrl_"+identifier) {
		return nil, errDACPUnavailable
	}
	interfaces := dacpMulticastInterfaces()
	if len(interfaces) == 0 {
		return nil, nil
	}
	serviceName := dns.Fqdn(instance + "." + dacpServiceType + "." + dacpServiceDomain)
	lookupCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan dacpDNSDiscoveryResult, len(interfaces))
	for _, networkInterface := range interfaces {
		go func(networkInterface net.Interface) {
			service, err := resolveDACPOnInterface(lookupCtx, networkInterface, instance, serviceName)
			results <- dacpDNSDiscoveryResult{service: service, err: err}
		}(networkInterface)
	}

	remaining := len(interfaces)
	for remaining > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case result := <-results:
			remaining--
			if result.service != nil {
				cancel()
				return []DACPService{*result.service}, nil
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
		}
	}
	return nil, nil
}

func dacpMulticastInterfaces() []net.Interface {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	selected := make([]net.Interface, 0, dacpInterfaceLimit)
	for _, networkInterface := range interfaces {
		if networkInterface.Flags&net.FlagUp == 0 || networkInterface.Flags&net.FlagMulticast == 0 {
			continue
		}
		addresses, err := networkInterface.Addrs()
		if err != nil {
			continue
		}
		for _, address := range addresses {
			prefix, err := netip.ParsePrefix(address.String())
			if err != nil {
				continue
			}
			ip := prefix.Addr().Unmap()
			if !ip.Is4() || !isDACPPrivateAddress(ip) {
				continue
			}
			selected = append(selected, networkInterface)
			break
		}
		if len(selected) == dacpInterfaceLimit {
			break
		}
	}
	return selected
}

func resolveDACPOnInterface(ctx context.Context, networkInterface net.Interface, instance, serviceName string) (*DACPService, error) {
	connection, err := net.ListenMulticastUDP("udp4", &networkInterface, &net.UDPAddr{IP: dacpMDNSGroupIPv4, Port: 5353})
	if err != nil {
		return nil, err
	}
	defer connection.Close()
	packetConn := ipv4.NewPacketConn(connection)
	if err := packetConn.SetMulticastInterface(&networkInterface); err != nil {
		return nil, err
	}
	if err := packetConn.SetMulticastTTL(255); err != nil {
		return nil, err
	}
	stopClose := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stopClose()
	if err := sendDACPQuestion(packetConn, networkInterface, serviceName, dns.TypeSRV); err != nil {
		return nil, err
	}

	state := dacpDNSServiceState{serviceName: serviceName}
	buffer := make([]byte, dacpDNSPacketLimit)
	messageCount := 0
	aQuerySent := false
	aQueryCount := 0
	srvQueryCount := 1
	lastSRVQuery := time.Now()
	lastAQuery := time.Time{}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := connection.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
			return nil, err
		}
		count, _, _, err := packetConn.ReadFrom(buffer)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if networkError, ok := err.(net.Error); ok && networkError.Timeout() {
				now := time.Now()
				if state.target == "" && srvQueryCount < dacpDNSQueryLimit && now.Sub(lastSRVQuery) >= 200*time.Millisecond {
					if err := sendDACPQuestion(packetConn, networkInterface, serviceName, dns.TypeSRV); err != nil {
						return nil, err
					}
					srvQueryCount++
					lastSRVQuery = now
				} else if state.target != "" && aQueryCount < dacpDNSQueryLimit && now.Sub(lastAQuery) >= 200*time.Millisecond {
					if err := sendDACPQuestion(packetConn, networkInterface, state.target, dns.TypeA); err != nil {
						return nil, err
					}
					aQueryCount++
					lastAQuery = now
				}
				continue
			}
			return nil, err
		}
		messageCount++
		if messageCount > dacpDNSMessageLimit {
			return nil, nil
		}
		var message dns.Msg
		if err := message.Unpack(buffer[:count]); err != nil || !message.Response || message.Opcode != dns.OpcodeQuery {
			continue
		}
		state.observe(&message)
		if state.address.IsValid() {
			address := net.IP(state.address.AsSlice())
			return &DACPService{
				Instance: instance,
				Service:  dacpServiceType,
				Domain:   dacpServiceDomain,
				HostName: state.target,
				Port:     state.port,
				IPv4:     []net.IP{address},
			}, nil
		}
		if state.target != "" && !aQuerySent {
			if err := sendDACPQuestion(packetConn, networkInterface, state.target, dns.TypeA); err != nil {
				return nil, err
			}
			aQuerySent = true
			aQueryCount = 1
			lastAQuery = time.Now()
		}
	}
}

func sendDACPQuestion(packetConn *ipv4.PacketConn, networkInterface net.Interface, name string, recordType uint16) error {
	packet, err := makeDACPQuestionPacket(name, recordType)
	if err != nil {
		return err
	}
	_, err = packetConn.WriteTo(packet, &ipv4.ControlMessage{IfIndex: networkInterface.Index, TTL: 255}, &net.UDPAddr{IP: dacpMDNSGroupIPv4, Port: 5353})
	return err
}

func makeDACPQuestionPacket(name string, recordType uint16) ([]byte, error) {
	question := &dns.Msg{
		MsgHdr: dns.MsgHdr{Id: 0, Opcode: dns.OpcodeQuery},
		Question: []dns.Question{{
			Name:   dns.Fqdn(name),
			Qtype:  recordType,
			Qclass: dns.ClassINET,
		}},
	}
	return question.Pack()
}

func (s *dacpDNSServiceState) observe(message *dns.Msg) {
	records := [][]dns.RR{message.Answer, message.Ns, message.Extra}
	if s.target == "" {
		for _, section := range records {
			for _, record := range section {
				srv, ok := record.(*dns.SRV)
				if !ok || !dnsNameEqual(srv.Hdr.Name, s.serviceName) || srv.Port == 0 || srv.Target == "." {
					continue
				}
				target := dns.Fqdn(srv.Target)
				if len(target) > 255 {
					continue
				}
				if _, valid := dns.IsDomainName(target); !valid {
					continue
				}
				s.target = target
				s.port = int(srv.Port)
				break
			}
			if s.target != "" {
				break
			}
		}
	}
	if s.target == "" {
		return
	}
	for _, section := range records {
		for _, record := range section {
			a, ok := record.(*dns.A)
			if !ok || !dnsNameEqual(a.Hdr.Name, s.target) {
				continue
			}
			address, valid := netip.AddrFromSlice(a.A)
			if !valid {
				continue
			}
			address = address.Unmap()
			if isDACPPrivateAddress(address) {
				s.address = address
				return
			}
		}
	}
}

func dnsNameEqual(left, right string) bool {
	return strings.EqualFold(strings.TrimSuffix(left, "."), strings.TrimSuffix(right, "."))
}
