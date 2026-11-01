package main

// The monitor never uses the host network namespace. Pinned IP probes avoid
// confusing DNS failures with tunnel failures, including with SOCKS5.
import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type probeSpec struct {
	Family string `json:"family"`
	Target string `json:"target"`
	URL    string `json:"url"`
	IP     string `json:"ip,omitempty"`
	Proxy  string `json:"proxy,omitempty"`
	Source string `json:"source,omitempty"`
	Warp   bool   `json:"expect_warp"`
}
type probeResult struct {
	Family       string `json:"family"`
	Target       string `json:"target"`
	Path         string `json:"path"`
	Stage        string `json:"stage"`
	OK           bool   `json:"ok"`
	Milliseconds int64  `json:"elapsed_ms"`
	HTTP         int    `json:"http_status,omitempty"`
	Error        string `json:"error,omitempty"`
}

func targets(family string, warp bool) []probeSpec {
	ip, backup := "1.1.1.1", "8.8.8.8"
	if family == "ipv6" {
		ip, backup = "2606:4700:4700::1111", "2001:4860:4860::8888"
	}
	return []probeSpec{
		{Family: family, Target: "cloudflare-trace", URL: "https://" + net.JoinHostPort(ip, "443") + "/cdn-cgi/trace", IP: ip, Warp: warp},
		{Family: family, Target: "google-dns-https", URL: "https://dns.google/resolve?name=example.com&type=A", IP: backup},
	}
}

// A complete SOCKS CONNECT is required. Merely connecting to its TCP listener
// was insufficient to detect the connected-but-blackholed WARP session.
func socksConnect(conn net.Conn, host string, port uint16) error {
	if _, e := conn.Write([]byte{5, 1, 0}); e != nil {
		return e
	}
	header := make([]byte, 2)
	if _, e := io.ReadFull(conn, header); e != nil {
		return e
	}
	if header[0] != 5 || header[1] != 0 {
		return errors.New("SOCKS authentication rejected")
	}
	req := []byte{5, 1, 0}
	if ip, e := netip.ParseAddr(host); e == nil {
		ip = ip.Unmap()
		if ip.Is4() {
			req = append(req, 1)
		} else {
			req = append(req, 4)
		}
		req = append(req, ip.AsSlice()...)
	} else {
		if len(host) > 255 {
			return errors.New("SOCKS hostname too long")
		}
		req = append(req, 3, byte(len(host)))
		req = append(req, host...)
	}
	req = binary.BigEndian.AppendUint16(req, port)
	if _, e := conn.Write(req); e != nil {
		return e
	}
	header = make([]byte, 4)
	if _, e := io.ReadFull(conn, header); e != nil {
		return e
	}
	if header[0] != 5 || header[1] != 0 || header[2] != 0 {
		return fmt.Errorf("SOCKS CONNECT reply code %d", header[1])
	}
	n := 0
	switch header[3] {
	case 1:
		n = 4
	case 4:
		n = 16
	case 3:
		b := make([]byte, 1)
		if _, e := io.ReadFull(conn, b); e != nil {
			return e
		}
		n = int(b[0])
	default:
		return errors.New("invalid SOCKS reply address")
	}
	_, e := io.CopyN(io.Discard, conn, int64(n+2))
	return e
}

func requestProbe(s probeSpec) probeResult { return requestProbeTLS(s, nil) }

func requestProbeTLS(s probeSpec, roots *x509.CertPool) (r probeResult) {
	started := time.Now()
	r = probeResult{Family: s.Family, Target: s.Target, Path: "policy-route", Stage: "tcp"}
	if s.Proxy != "" {
		r.Path = "socks"
	}
	defer func() { r.Milliseconds = time.Since(started).Milliseconds() }()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	dialer := net.Dialer{Timeout: 5 * time.Second}
	if s.Source != "" {
		dialer.LocalAddr = &net.TCPAddr{IP: net.ParseIP(s.Source)}
	}
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true, TLSHandshakeTimeout: 5 * time.Second}
	defer transport.CloseIdleConnections()
	transport.DialTLSContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, e := net.SplitHostPort(address)
		if e != nil {
			return nil, e
		}
		portNumber, e := strconv.Atoi(port)
		if e != nil {
			return nil, e
		}
		destination := host
		if s.IP != "" {
			destination = s.IP
		}
		endpoint := net.JoinHostPort(destination, port)
		if s.Proxy != "" {
			endpoint = s.Proxy
		}
		conn, e := dialer.DialContext(ctx, "tcp", endpoint)
		if e != nil {
			return nil, e
		}
		deadline, _ := ctx.Deadline()
		_ = conn.SetDeadline(deadline)
		if s.Proxy != "" {
			r.Stage = "socks_connect"
			if e = socksConnect(conn, destination, uint16(portNumber)); e != nil {
				conn.Close()
				return nil, e
			}
		}
		r.Stage = "tls"
		t := tls.Client(conn, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12, RootCAs: roots})
		if e = t.HandshakeContext(ctx); e != nil {
			conn.Close()
			return nil, e
		}
		r.Stage = "http"
		return t, nil
	}
	client := http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, e := http.NewRequestWithContext(ctx, "GET", s.URL, nil)
	if e != nil {
		r.Error = e.Error()
		return
	}
	req.Header.Set("User-Agent", "WarpDeck-connectivity/"+monitorVersion)
	resp, e := client.Do(req)
	if e != nil {
		r.Error = cleanDiagnostic(e.Error())
		return
	}
	defer resp.Body.Close()
	r.HTTP = resp.StatusCode
	body, e := io.ReadAll(io.LimitReader(resp.Body, 16385))
	if e != nil {
		r.Error = cleanDiagnostic(e.Error())
		return
	}
	r.Stage = "response"
	if resp.StatusCode != 200 || len(body) > 16384 {
		r.Error = "unexpected HTTP response or oversized body"
		return
	}
	if s.Target == "google-dns-https" {
		var d struct{ Status *int }
		if json.Unmarshal(body, &d) != nil || d.Status == nil || *d.Status != 0 {
			r.Error = "unexpected DNS HTTPS response"
			return
		}
	} else {
		want := "warp=off"
		if s.Warp {
			want = "warp=on"
		}
		found := false
		for _, line := range strings.Split(string(body), "\n") {
			if strings.TrimSpace(line) == want {
				found = true
			}
		}
		if !found {
			r.Error = "unexpected WARP egress marker"
			return
		}
	}
	r.OK = true
	r.Stage = "complete"
	return
}

func childProbe(s probeSpec, unprivileged bool) probeResult {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	b, _ := json.Marshal(s)
	self, e := os.Executable()
	if e != nil {
		return probeResult{Family: s.Family, Target: s.Target, Error: e.Error()}
	}
	cmd := exec.CommandContext(ctx, self, "internal", "connectivity-probe", string(b))
	if unprivileged {
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 65534, Gid: 65534, Groups: []uint32{65534}}}
	}
	out, e := cmd.Output()
	var r probeResult
	if e != nil || json.Unmarshal(out, &r) != nil {
		return probeResult{Family: s.Family, Target: s.Target, Path: "probe-helper", Stage: "helper", Error: "probe process failed or timed out"}
	}
	return r
}

func localSOCKS(c Config) (endpoint, source string) {
	listen := netip.MustParseAddr(c.Socks.Listen)
	var candidates []netip.Addr
	for _, s := range []string{"127.0.0.1", "::1"} {
		candidates = append(candidates, netip.MustParseAddr(s))
	}
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if p, e := netip.ParsePrefix(a.String()); e == nil {
			candidates = append(candidates, p.Addr())
		}
	}
	for _, a := range candidates {
		if !listen.IsUnspecified() && a != listen {
			continue
		}
		if listen.Is4() && !a.Is4() {
			continue
		}
		for _, cidr := range c.Socks.Allowed {
			p, _ := netip.ParsePrefix(cidr)
			if p.Contains(a) {
				return net.JoinHostPort(a.String(), fmt.Sprint(c.Socks.Port)), a.String()
			}
		}
	}
	return "", ""
}

type familyResult struct {
	Family       string        `json:"family"`
	Policy       string        `json:"policy"`
	OK           bool          `json:"ok"`
	TunnelFailed bool          `json:"tunnel_failed"`
	ProxyFailed  bool          `json:"proxy_failed"`
	SOCKSCovered bool          `json:"socks_covered"`
	Probes       []probeResult `json:"probes"`
}

func checkFamily(c Config, family, policy string) familyResult {
	r := familyResult{Family: family, Policy: policy}
	if policy != "warp" {
		r.OK = true
		return r
	}
	endpoint, source := localSOCKS(c)
	useSOCKS := c.Mode != "gateway" && endpoint != ""
	r.SOCKSCovered = useSOCKS
	for _, s := range targets(family, policy == "warp") {
		if useSOCKS {
			s.Proxy = endpoint
			s.Source = source
		}
		p := childProbe(s, !useSOCKS)
		r.Probes = append(r.Probes, p)
		if p.OK {
			r.OK = true
			return r
		}
	}
	if useSOCKS {
		for _, s := range targets(family, policy == "warp") {
			p := childProbe(s, true)
			r.Probes = append(r.Probes, p)
			if p.OK {
				r.ProxyFailed = true
				return r
			}
		}
	}
	r.TunnelFailed = policy == "warp"
	return r
}

func checkFamilies(c Config) []familyResult {
	results := make([]familyResult, 2)
	var wg sync.WaitGroup
	for i, policy := range []string{c.Egress.IPv4, c.Egress.IPv6} {
		wg.Add(1)
		go func(i int, policy string) {
			defer wg.Done()
			results[i] = checkFamily(c, []string{"ipv4", "ipv6"}[i], policy)
		}(i, policy)
	}
	wg.Wait()
	return results
}
