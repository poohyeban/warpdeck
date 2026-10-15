package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixture(t *testing.T) []byte {
	t.Helper()
	b, e := os.ReadFile("../../config.example.json")
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func TestStrictConfiguration(t *testing.T) {
	base := fixture(t)
	if _, e := decode(base); e != nil {
		t.Fatal(e)
	}
	cases := []string{
		strings.Replace(string(base), `"version": 1`, `"version": 1, "version": 1`, 1),
		strings.Replace(string(base), `"version": 1`, `"version": 1, "typo": true`, 1),
		strings.Replace(string(base), `"mode": "both"`, `"mode": "something"`, 1),
		strings.Replace(string(base), `"ipv6": "native"`, `"ipv6": "disabled"`, 1),
		strings.Replace(string(base), `"port": 1080`, `"port": 0`, 1),
		strings.Replace(string(base), `"allowed_sources": []`, `"allowed_sources": ["1.2.3.999/32"]`, 1),
		strings.Replace(string(base), `"interface": "eth0"`, `"interface": "eth0;touch /tmp/bad"`, 1),
		strings.Replace(string(base), `"udp": true`, `"udp": "true"`, 1),
		strings.Replace(string(base), `"allowed_sources": []`, `"allowed_sources": null`, 1),
		string(base) + ` {}`,
	}
	for _, s := range cases {
		if _, e := decode([]byte(s)); e == nil {
			t.Fatalf("accepted invalid JSON: %s", s)
		}
	}
}
func TestSourcesAndRendering(t *testing.T) {
	c, e := decode(fixture(t))
	if e != nil {
		t.Fatal(e)
	}
	out := t.TempDir()
	if e = render(c, out); e != nil {
		t.Fatal(e)
	}
	p, _ := os.ReadFile(filepath.Join(out, "3proxy.cfg"))
	n, _ := os.ReadFile(filepath.Join(out, "gateway.nft"))
	if strings.Contains(string(p), "allow *") || strings.Contains(string(n), "ip saddr") {
		t.Fatal("empty whitelist must deny all")
	}
	c.Socks.Allowed = []string{"192.0.2.37/24", "2001:db8::1", "192.0.2.0/24"}
	c.Gateway.Allowed = []string{"198.51.100.19"}
	c.Gateway.Interface = "lan1"
	c.Socks.Listen = "::"
	c.Socks.UDP = false
	c, e = decode(canonical(c))
	if e != nil {
		t.Fatal(e)
	}
	if len(c.Socks.Allowed) != 2 || c.Gateway.Allowed[0] != "198.51.100.19/32" {
		t.Fatal("CIDR normalization failed")
	}
	if e = render(c, out); e != nil {
		t.Fatal(e)
	}
	p, _ = os.ReadFile(filepath.Join(out, "3proxy.cfg"))
	n, _ = os.ReadFile(filepath.Join(out, "gateway.nft"))
	if strings.Contains(string(p), "UDPASSOC") || !strings.Contains(string(p), "192.0.2.0/24,2001:db8::1/128") {
		t.Fatal(string(p))
	}
	if !strings.Contains(string(n), `iifname "lan1"`) || !strings.Contains(string(n), "198.51.100.19/32") {
		t.Fatal(string(n))
	}
	c.Mode = "proxy"
	_ = render(c, out)
	n, _ = os.ReadFile(filepath.Join(out, "gateway.nft"))
	if strings.Contains(string(n), "accept") {
		t.Fatal("proxy mode permits forwarding")
	}
	c.Gateway.Allowed = []string{"2001:db8::/32"}
	if _, e = decode(canonical(c)); e == nil {
		t.Fatal("IPv6 gateway sources accepted by IPv4 gateway")
	}
}
func TestCanonicalAndAtomic(t *testing.T) {
	c, _ := decode(fixture(t))
	raw, _ := json.Marshal(c)
	other, e := decode(raw)
	if e != nil || digest(c) != digest(other) {
		t.Fatal("formatting changed revision")
	}
	p := filepath.Join(t.TempDir(), "config.json")
	for i := 0; i < 100; i++ {
		if e := atomic(p, canonical(c)); e != nil {
			t.Fatal(e)
		}
		if _, e := load(p); e != nil {
			t.Fatal(e)
		}
	}
}
