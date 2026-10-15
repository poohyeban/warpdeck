package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const stateDir = "/var/lib/warpdeck"
const runDir = "/run/warpdeck"
const configPath = stateDir + "/config.json"

type Socks struct {
	Listen     string   `json:"listen"`
	Port       int      `json:"port"`
	Allowed    []string `json:"allowed_sources"`
	Max        int      `json:"max_connections"`
	Resolution string   `json:"resolution"`
	UDP        bool     `json:"udp"`
}
type Egress struct {
	IPv4 string `json:"ipv4"`
	IPv6 string `json:"ipv6"`
}
type Masquerade struct {
	IPv4 bool `json:"ipv4"`
	IPv6 bool `json:"ipv6"`
}
type Gateway struct {
	Interface string     `json:"interface"`
	Allowed   []string   `json:"allowed_sources"`
	MSS       int        `json:"tcp_mss"`
	NAT       Masquerade `json:"masquerade"`
}
type Config struct {
	Version int     `json:"version"`
	Mode    string  `json:"mode"`
	Egress  Egress  `json:"egress"`
	Socks   Socks   `json:"socks"`
	Gateway Gateway `json:"gateway"`
}

func noDuplicates(d *json.Decoder) error {
	t, e := d.Token()
	if e != nil {
		return e
	}
	if delim, ok := t.(json.Delim); ok {
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				k, e := d.Token()
				if e != nil {
					return e
				}
				s := k.(string)
				if seen[s] {
					return fmt.Errorf("duplicate JSON key %q", s)
				}
				seen[s] = true
				if e = noDuplicates(d); e != nil {
					return e
				}
			}
		case '[':
			for d.More() {
				if e = noDuplicates(d); e != nil {
					return e
				}
			}
		default:
			return errors.New("unexpected JSON delimiter")
		}
		_, e = d.Token()
		return e
	}
	return nil
}
func exactKeys(m map[string]json.RawMessage, keys ...string) error {
	if len(m) != len(keys) {
		return errors.New("missing or unknown configuration fields")
	}
	for _, k := range keys {
		v, ok := m[k]
		if !ok || bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return fmt.Errorf("missing/null field %s", k)
		}
	}
	return nil
}
func sources(in []string) ([]string, error) {
	if in == nil {
		return nil, errors.New("allowed_sources must be an array; [] denies all")
	}
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, s := range in {
		p, e := netip.ParsePrefix(s)
		if e != nil {
			a, e2 := netip.ParseAddr(s)
			if e2 != nil {
				return nil, fmt.Errorf("invalid source IP/CIDR %q", s)
			}
			p = netip.PrefixFrom(a, a.BitLen())
		}
		if p.Addr().Zone() != "" {
			return nil, fmt.Errorf("unsupported source address %q", s)
		}
		if p.Addr().Is4In6() && p.Bits() >= 96 {
			p = netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-96)
		}
		s = p.Masked().String()
		if !seen[s] {
			out = append(out, s)
			seen[s] = true
		}
	}
	sort.Strings(out)
	return out, nil
}
func decode(b []byte) (Config, error) {
	var c Config
	if len(b) > 65536 {
		return c, errors.New("configuration exceeds 64 KiB")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	if e := noDuplicates(d); e != nil {
		return c, e
	}
	if _, e := d.Token(); e != io.EOF {
		return c, errors.New("configuration must contain exactly one JSON object")
	}
	var top, sock, gw map[string]json.RawMessage
	if e := json.Unmarshal(b, &top); e != nil {
		return c, e
	}
	// v1 is interpreted without changing its file or broadening its allowlists.
	var version int
	if e := json.Unmarshal(top["version"], &version); e != nil {
		return c, e
	}
	if version == 1 {
		if e := exactKeys(top, "version", "mode", "ipv6", "socks", "gateway"); e != nil {
			return c, e
		}
		var oldIPv6 string
		if e := json.Unmarshal(top["ipv6"], &oldIPv6); e != nil || oldIPv6 != "native" {
			return c, errors.New("v1 requires ipv6=native")
		}
	} else if version == 2 {
		if e := exactKeys(top, "version", "mode", "egress", "socks", "gateway"); e != nil {
			return c, e
		}
		var egress map[string]json.RawMessage
		if e := json.Unmarshal(top["egress"], &egress); e != nil {
			return c, e
		}
		if e := exactKeys(egress, "ipv4", "ipv6"); e != nil {
			return c, e
		}
	} else {
		return c, errors.New("version must be 1 or 2")
	}
	if e := json.Unmarshal(top["socks"], &sock); e != nil {
		return c, e
	}
	if e := exactKeys(sock, "listen", "port", "allowed_sources", "max_connections", "resolution", "udp"); e != nil {
		return c, e
	}
	if e := json.Unmarshal(top["gateway"], &gw); e != nil {
		return c, e
	}
	if version == 1 {
		if e := exactKeys(gw, "interface", "allowed_sources", "tcp_mss"); e != nil {
			return c, e
		}
		// v1 only forwarded IPv4: keep its original IPv4-only allowlist semantics.
		var oldAllowed []string
		if e := json.Unmarshal(gw["allowed_sources"], &oldAllowed); e != nil {
			return c, e
		}
		for _, value := range oldAllowed {
			addr := strings.Split(value, "/")[0]
			a, e := netip.ParseAddr(addr)
			if e != nil || !a.Is4() {
				return c, errors.New("v1 gateway sources must be IPv4; use v2 for IPv6")
			}
		}
		gw["masquerade"] = json.RawMessage(`{"ipv4":true,"ipv6":false}`)
		top["gateway"] = canonical(gw)
		top["version"] = json.RawMessage(`2`)
		top["egress"] = json.RawMessage(`{"ipv4":"warp","ipv6":"native"}`)
		delete(top, "ipv6")
	} else {
		if e := exactKeys(gw, "interface", "allowed_sources", "tcp_mss", "masquerade"); e != nil {
			return c, e
		}
		var nat map[string]json.RawMessage
		if e := json.Unmarshal(gw["masquerade"], &nat); e != nil {
			return c, e
		}
		if e := exactKeys(nat, "ipv4", "ipv6"); e != nil {
			return c, e
		}
	}
	d = json.NewDecoder(bytes.NewReader(canonical(top)))
	d.DisallowUnknownFields()
	if e := d.Decode(&c); e != nil {
		return c, e
	}
	if c.Mode != "proxy" && c.Mode != "gateway" && c.Mode != "both" {
		return c, errors.New("mode must be proxy, gateway or both")
	}
	for _, policy := range []string{c.Egress.IPv4, c.Egress.IPv6} {
		if policy != "native" && policy != "warp" {
			return c, errors.New("egress must be native or warp for each address family")
		}
	}
	a, e := netip.ParseAddr(c.Socks.Listen)
	if e != nil || a.Is4In6() || a.Zone() != "" {
		return c, errors.New("socks.listen must be an IPv4/IPv6 address")
	}
	c.Socks.Listen = a.String()
	if c.Socks.Port < 1 || c.Socks.Port > 65535 {
		return c, errors.New("socks.port must be 1..65535")
	}
	if c.Socks.Max < 1 || c.Socks.Max > 2147483647 {
		return c, errors.New("socks.max_connections must be a positive 32-bit integer")
	}
	if c.Socks.Resolution != "prefer_ipv6" && c.Socks.Resolution != "prefer_ipv4" {
		return c, errors.New("socks.resolution must be prefer_ipv6 or prefer_ipv4")
	}
	if !regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,14}$`).MatchString(c.Gateway.Interface) {
		return c, errors.New("invalid gateway.interface")
	}
	if c.Gateway.MSS < 0 || c.Gateway.MSS > 65535 {
		return c, errors.New("gateway.tcp_mss must be 0..65535 (0 disables adjustment)")
	}
	c.Socks.Allowed, e = sources(c.Socks.Allowed)
	if e != nil {
		return c, e
	}
	c.Gateway.Allowed, e = sources(c.Gateway.Allowed)
	return c, e
}
func load(path string) (Config, error) {
	b, e := os.ReadFile(path)
	if e != nil {
		return Config{}, e
	}
	return decode(b)
}
func canonical(v any) []byte { b, _ := json.MarshalIndent(v, "", "  "); return append(b, '\n') }
func digest(v any) string    { b := sha256.Sum256(canonical(v)); return hex.EncodeToString(b[:]) }
func atomic(path string, b []byte) error {
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".warpdeck-")
	if e != nil {
		return e
	}
	name := f.Name()
	defer os.Remove(name)
	if _, e = f.Write(b); e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e != nil {
		return e
	}
	if ce != nil {
		return ce
	}
	if e = os.Rename(name, path); e != nil {
		return e
	}
	dir, e := os.Open(filepath.Dir(path))
	if e == nil {
		defer dir.Close()
		_ = dir.Sync()
	}
	return nil
}
func needsWARP(c Config) bool { return c.Egress.IPv4 == "warp" || c.Egress.IPv6 == "warp" }
func networkCheck(c Config) error {
	if _, e := net.InterfaceByName(c.Gateway.Interface); e != nil {
		return fmt.Errorf("gateway interface: %w", e)
	}
	if c.Mode == "gateway" {
		return nil
	}
	a := netip.MustParseAddr(c.Socks.Listen)
	local := a.IsUnspecified()
	list, e := net.InterfaceAddrs()
	if e != nil {
		return e
	}
	for _, v := range list {
		p, e := netip.ParsePrefix(v.String())
		if e == nil && p.Addr() == a {
			local = true
		}
	}
	if !local {
		return fmt.Errorf("socks.listen %s is not assigned in this container", a)
	}
	// An unchanged port may be occupied by our own proxy until reconfiguration.
	active, e := load(runDir + "/active/config.json")
	if e == nil && alive(runDir+"/proxy.pid") && active.Socks.Port == c.Socks.Port {
		return nil
	}
	listener, e := net.Listen("tcp", net.JoinHostPort(c.Socks.Listen, strconv.Itoa(c.Socks.Port)))
	if e != nil {
		return fmt.Errorf("SOCKS listener unavailable: %w", e)
	}
	return listener.Close()
}
func render(c Config, out string) error {
	if e := os.MkdirAll(out, 0700); e != nil {
		return e
	}
	ops := "CONNECT"
	if c.Socks.UDP {
		ops += ",UDPASSOC"
	}
	family := "64"
	if c.Socks.Resolution == "prefer_ipv4" {
		family = "46"
	}
	proxy := fmt.Sprintf("maxconn %d\ntimeouts 1 5 30 60 180 1800 15 60 15 5\nauth iponly\n", c.Socks.Max)
	// Dual-stack sockets expose IPv4 clients as IPv4-mapped IPv6 addresses.
	// Match those against IPv4 ACLs first, then prevent ::/0 from implicitly
	// authorizing IPv4 clients which the IPv4 ACL did not allow.
	var allowed4, allowed6 []string
	for _, source := range c.Socks.Allowed {
		prefix, _ := netip.ParsePrefix(source)
		if prefix.Addr().Is4() {
			mapped := netip.AddrFrom16(prefix.Addr().As16())
			allowed4 = append(allowed4, source, netip.PrefixFrom(mapped, prefix.Bits()+96).String())
		} else {
			allowed6 = append(allowed6, source)
		}
	}
	if len(allowed4) > 0 {
		proxy += fmt.Sprintf("allow * %s * * %s\n", strings.Join(allowed4, ","), ops)
	}
	proxy += "deny * 0.0.0.0/0,::ffff:0.0.0.0/96\n"
	if len(allowed6) > 0 {
		proxy += fmt.Sprintf("allow * %s * * %s\n", strings.Join(allowed6, ","), ops)
	}
	proxy += fmt.Sprintf("deny *\nsetgid 65534\nsetuid 65534\nsocks -%s -i%s -p%d\n", family, c.Socks.Listen, c.Socks.Port)
	// Do not teach clients a route around this gateway when native egress
	// forwards through the same interface. Later policy changes must still apply.
	nft := "delete table inet warpdeck_gateway\ntable inet warpdeck_gateway {\n chain proxy_route { type route hook output priority -150; policy accept; meta skuid 65534 meta mark set 0x10910000; meta skuid 65534 ct direction reply meta mark set 0x10920000; }\n chain output { type filter hook output priority -10; policy accept; ip protocol 1 icmp type 5 drop; meta l4proto 58 icmpv6 type 137 drop; }\n chain forward { type filter hook forward priority -10; policy drop;\n"
	natRules := ""
	if c.Mode != "proxy" {
		for _, family := range []int{4, 6} {
			values := []string{}
			for _, value := range c.Gateway.Allowed {
				prefix, _ := netip.ParsePrefix(value)
				if prefix.Addr().Is4() == (family == 4) {
					values = append(values, value)
				}
			}
			if len(values) == 0 {
				continue
			}
			af, policy, nat := "ip", c.Egress.IPv4, c.Gateway.NAT.IPv4
			if family == 6 {
				af, policy, nat = "ip6", c.Egress.IPv6, c.Gateway.NAT.IPv6
			}
			outIf, inIf := `oifname "CloudflareWARP"`, `iifname "CloudflareWARP"`
			if policy == "native" {
				outIf, inIf = `oifname != "CloudflareWARP"`, `iifname != "CloudflareWARP"`
			}
			allowed := "{ " + strings.Join(values, ", ") + " }"
			nft += fmt.Sprintf("%s oifname %q %s daddr %s ct state established,related accept\n", inIf, c.Gateway.Interface, af, allowed)
			match := fmt.Sprintf("iifname %q %s %s saddr %s", c.Gateway.Interface, outIf, af, allowed)
			if c.Gateway.MSS > 0 {
				nft += fmt.Sprintf("%s tcp flags syn tcp option maxseg size > %d tcp option maxseg size set %d\n", match, c.Gateway.MSS, c.Gateway.MSS)
			}
			nft += match + " accept\n"
			if nat {
				natRules += match + " masquerade\n"
			}
		}
	}
	nft += " }\n chain postrouting { type nat hook postrouting priority srcnat; policy accept;\n" + natRules + " }\n}\n"
	env := fmt.Sprintf("MODE=%s\nINTERFACE=%s\nSOCKS_LISTEN=%s\nSOCKS_PORT=%d\nEGRESS4=%s\nEGRESS6=%s\nREVISION=%s\nPROXY_REVISION=%s\nEGRESS_REVISION=%s\n", c.Mode, c.Gateway.Interface, c.Socks.Listen, c.Socks.Port, c.Egress.IPv4, c.Egress.IPv6, digest(c), digest(c.Socks), digest(c.Egress))
	for name, b := range map[string][]byte{"config.json": canonical(c), "3proxy.cfg": []byte(proxy), "gateway.nft": []byte(nft), "settings.env": []byte(env)} {
		if e := atomic(filepath.Join(out, name), b); e != nil {
			return e
		}
	}
	return nil
}
func read(path string) string { b, _ := os.ReadFile(path); return strings.TrimSpace(string(b)) }
func alive(path string) bool {
	pid, e := strconv.Atoi(read(path))
	return e == nil && pid > 0 && syscall.Kill(pid, 0) == nil
}
func wake(event string) error {
	fd, e := syscall.Open(runDir+"/events", syscall.O_RDWR|syscall.O_NONBLOCK, 0)
	if e != nil {
		return e
	}
	defer syscall.Close(fd)
	_, e = syscall.Write(fd, []byte(event+"\n"))
	return e
}
func waitApplied(rev string) error {
	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		if read(runDir+"/applied-revision") == rev {
			return nil
		}
		if read(runDir+"/failed-revision") == rev {
			return fmt.Errorf("configuration saved but not applied: %s", read(runDir+"/config-error"))
		}
		if !alive(runDir + "/supervisor.pid") {
			return errors.New("configuration saved; supervisor unavailable, check status")
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("configuration saved but application timed out; check status")
}
func lock() (func(), error) {
	f, e := os.OpenFile(stateDir+"/control.lock", os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	until := time.Now().Add(125 * time.Second)
	for {
		e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if e == nil {
			break
		}
		if time.Now().After(until) {
			f.Close()
			return nil, errors.New("another configuration command is running")
		}
		time.Sleep(100 * time.Millisecond)
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
}
func change(kind, path, value string) error {
	if os.Geteuid() != 0 {
		return errors.New("configuration changes require container root")
	}
	unlock, e := lock()
	if e != nil {
		return e
	}
	defer unlock()
	if !alive(runDir + "/supervisor.pid") {
		return errors.New("supervisor not ready")
	}
	var c Config
	if kind == "apply" {
		c, e = load(path)
	} else {
		c, e = load(configPath)
		if e == nil && kind == "set" {
			var m map[string]any
			_ = json.Unmarshal(canonical(c), &m)
			var v any
			if e = json.Unmarshal([]byte(value), &v); e != nil {
				return e
			}
			parts := strings.Split(path, ".")
			obj := m
			for _, p := range parts[:len(parts)-1] {
				next, ok := obj[p].(map[string]any)
				if !ok {
					return errors.New("unknown configuration path")
				}
				obj = next
			}
			last := parts[len(parts)-1]
			if _, ok := obj[last]; !ok {
				return errors.New("unknown configuration field")
			}
			obj[last] = v
			c, e = decode(canonical(m))
		}
	}
	if e != nil {
		return e
	}
	if kind != "set" {
		if needsWARP(c) && read(runDir+"/registration-state") != "registered" {
			return errors.New("WARP is not registered; run warpdeck register --accept-tos first")
		}
		if e = networkCheck(c); e != nil {
			return e
		}
	}
	if e = atomic(configPath, canonical(c)); e != nil {
		return e
	}
	if kind == "set" {
		fmt.Printf("WarpDeck configuration saved (not applied): mode=%s. Run warpdeck reload to apply.\n", c.Mode)
		return nil
	}
	// Freeze exactly the configuration confirmed by this command. An unrelated
	// direct edit after confirmation must not enter the running configuration.
	if e = atomic(runDir+"/request.json", canonical(c)); e != nil {
		return e
	}
	_ = os.Remove(runDir + "/failed-revision")
	_ = os.Remove(runDir + "/config-error")
	if e = wake("reload"); e != nil {
		return e
	}
	if e = waitApplied(digest(c)); e != nil {
		return e
	}
	fmt.Printf("WarpDeck configuration applied: mode=%s revision=%s\n", c.Mode, digest(c)[:12])
	return nil
}
func register(accept bool) error {
	if !accept {
		return errors.New("registration requires explicit acceptance: warpdeck register --accept-tos")
	}
	if os.Geteuid() != 0 {
		return errors.New("registration requires container root")
	}
	unlock, e := lock()
	if e != nil {
		return e
	}
	defer unlock()
	if !alive(runDir + "/supervisor.pid") {
		return errors.New("supervisor not ready")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	// compose up can return before IPC is ready. Bound the wait rather than
	// treating daemon startup as a failed registration/network request.
	for read(runDir+"/registration-state") == "starting" {
		if ctx.Err() != nil || !alive(runDir+"/supervisor.pid") {
			return errors.New("WARP service did not become ready; check container status")
		}
		time.Sleep(200 * time.Millisecond)
	}

	registered := func() bool {
		return exec.CommandContext(ctx, "warp-cli", "--accept-tos", "registration", "show").Run() == nil
	}
	if registered() {
		fmt.Println("WARP registration already exists; kept unchanged.")
		return wake("registered")
	}
	// Identity stays in the WARP volume. Never print account identifiers/keys,
	// never delete an existing identity, and do not recreate it on transient errors.
	if e = exec.CommandContext(ctx, "warp-cli", "--accept-tos", "registration", "new").Run(); e != nil {
		return fmt.Errorf("registration failed (%v); check connectivity and WARP service logs; existing identity was not deleted", e)
	}
	if !registered() {
		return errors.New("registration could not be verified; existing identity was not deleted")
	}
	if e = wake("registered"); e != nil {
		return e
	}
	fmt.Println("WARP registration completed. The supervisor will connect; check warpdeck status.")
	return nil
}
func status(asJSON bool) error {
	current, e := load(configPath)
	requested := "invalid"
	configError := ""
	if e == nil {
		requested = current.Mode
	} else {
		configError = e.Error()
	}
	active, ae := load(runDir + "/active/config.json")
	mode := "starting"
	if ae == nil {
		mode = active.Mode
	}
	if !alive(runDir + "/supervisor.pid") {
		mode = "stopped"
	}
	if e == nil {
		configError = read(runDir + "/config-error")
	}
	sync := e == nil && read(runDir+"/applied-revision") == digest(current)
	socks := "stopped"
	if alive(runDir + "/proxy.pid") {
		socks = "running"
	}
	gateway := "disabled"
	if mode == "both" || mode == "gateway" {
		gateway = "enabled"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	raw, _ := exec.CommandContext(ctx, "warp-cli", "--accept-tos", "status").Output()
	tunnel := "disconnected"
	if strings.Contains(string(raw), "Status update: Connected") {
		tunnel = "connected"
	}
	egress := Egress{}
	if ae == nil {
		egress = active.Egress
	}
	routeState := map[string]string{}
	for _, family := range []string{"4", "6"} {
		target := "1.1.1.1"
		if family == "6" {
			target = "2606:4700:4700::1111"
		}
		routeState["ipv"+family] = "unavailable"
		if exec.CommandContext(ctx, "ip", "-"+family, "route", "get", target, "mark", "0x10910000").Run() == nil {
			routeState["ipv"+family] = "available"
		}
	}
	result := map[string]any{"name": "WarpDeck", "registration": read(runDir + "/registration-state"), "mode": mode, "requested_mode": requested, "config_path": configPath, "config_in_sync": sync, "config_error": configError, "socks": socks, "ipv4_gateway": gateway, "tunnel": tunnel, "egress": egress, "routes": routeState, "ipv6_gateway": gateway}
	if asJSON {
		fmt.Print(string(canonical(result)))
	} else {
		fmt.Printf("WarpDeck\nRegistration: %s\nMode: %s\nConfigured mode: %s\nConfiguration: %s (in sync: %t)\nSOCKS5: %s\nIPv4 gateway: %s\nWARP tunnel: %s\nIPv6 gateway: %s\nIPv4 egress: %s (route: %s)\nIPv6 egress: %s (route: %s)\n", read(runDir+"/registration-state"), mode, requested, configPath, sync, socks, gateway, tunnel, gateway, egress.IPv4, routeState["ipv4"], egress.IPv6, routeState["ipv6"])
		if configError != "" {
			fmt.Println("Configuration error:", configError)
		}
	}
	return nil
}
func probe(path string) error {
	c, e := load(path)
	if e != nil {
		return e
	}
	a := netip.MustParseAddr(c.Socks.Listen)
	if a.IsUnspecified() {
		if a.Is4() {
			a = netip.MustParseAddr("127.0.0.1")
		} else {
			a = netip.IPv6Loopback()
		}
	}
	conn, e := net.DialTimeout("tcp", net.JoinHostPort(a.String(), strconv.Itoa(c.Socks.Port)), time.Second)
	if e == nil {
		conn.Close()
	}
	return e
}
func usage() {
	fmt.Println("WarpDeck\n  warpdeck register --accept-tos\n  warpdeck status [--json]\n  warpdeck mode proxy|gateway|both  (save only; run reload to apply)\n  warpdeck config show\n  warpdeck config validate [FILE]\n  warpdeck config set FIELD JSON_VALUE  (save only)\n  warpdeck config apply FILE\n  warpdeck reload  (explicitly apply saved configuration)")
}
func dispatch(a []string) error {
	if len(a) == 0 || len(a) == 1 && (a[0] == "--help" || a[0] == "help") {
		usage()
		return nil
	}
	if a[0] == "register" && (len(a) == 1 || len(a) == 2 && a[1] == "--accept-tos") {
		return register(len(a) == 2)
	}
	if a[0] == "status" && (len(a) == 1 || len(a) == 2 && a[1] == "--json") {
		return status(len(a) == 2)
	}
	if a[0] == "mode" && len(a) == 2 {
		v, _ := json.Marshal(a[1])
		return change("set", "mode", string(v))
	}
	if a[0] == "reload" && len(a) == 1 {
		return change("reload", "", "")
	}
	if a[0] == "config" && len(a) >= 2 {
		switch a[1] {
		case "show":
			if len(a) == 2 {
				c, e := load(configPath)
				if e == nil {
					fmt.Print(string(canonical(c)))
				}
				return e
			}
		case "validate":
			if len(a) == 2 || len(a) == 3 {
				p := configPath
				if len(a) == 3 {
					p = a[2]
				}
				_, e := load(p)
				if e == nil {
					fmt.Println("Configuration valid")
				}
				return e
			}
		case "set":
			if len(a) == 4 {
				return change("set", a[2], a[3])
			}
		case "apply":
			if len(a) == 3 {
				return change("apply", a[2], "")
			}
		}
	}
	if a[0] == "internal" && len(a) == 4 && a[1] == "prepare" {
		c, e := load(a[2])
		if e != nil {
			return e
		}
		if e = networkCheck(c); e != nil {
			return e
		}
		return render(c, a[3])
	}
	if a[0] == "internal" && len(a) == 3 && a[1] == "probe" {
		return probe(a[2])
	}
	return errors.New("invalid arguments; use warpdeck --help")
}
func main() {
	if e := dispatch(os.Args[1:]); e != nil {
		fmt.Fprintln(os.Stderr, "warpdeck:", e)
		os.Exit(1)
	}
}
