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
func TestDefaultsAndStrictConfiguration(t *testing.T) {
	base := fixture(t)
	c, e := decode(base)
	if e != nil {
		t.Fatal(e)
	}
	if c.Mode != "proxy" || c.Egress != (Egress{"warp", "warp"}) || c.Socks.Listen != "::" || len(c.Socks.Allowed) != 2 {
		t.Fatal("unexpected defaults")
	}
	cases := []string{
		strings.Replace(string(base), `"version": 2`, `"version": 2, "version": 2`, 1),
		strings.Replace(string(base), `"version": 2`, `"version": 2, "typo": true`, 1),
		strings.Replace(string(base), `"mode": "proxy"`, `"mode": "invalid"`, 1),
		strings.Replace(string(base), `"ipv4": "warp"`, `"ipv4": "auto"`, 1),
		strings.Replace(string(base), `"ipv6": "warp"`, `"ipv6": null`, 1),
		strings.Replace(string(base), `"port": 1080`, `"port": 0`, 1),
		strings.Replace(string(base), `"interface": "eth0"`, `"interface": "eth0;touch /tmp/bad"`, 1),
		strings.Replace(string(base), `"udp": true`, `"udp": "true"`, 1),
		strings.Replace(string(base), `"ipv4": true`, `"ipv4": null`, 1),
		string(base) + ` {}`,
	}
	for _, s := range cases {
		if _, e := decode([]byte(s)); e == nil {
			t.Fatalf("accepted invalid config: %s", s)
		}
	}
}
func TestAllSourceRangesAndLowPort(t *testing.T) {
	c, _ := decode(fixture(t))
	c.Socks.Port = 53
	c.Socks.Allowed = []string{"0.0.0.0/0", "::/0", "0.0.0.0", "192.0.2.37/24", "2001:db8::1", "192.0.2.0/24", "::ffff:192.0.2.1/128"}
	c.Gateway.Allowed = append([]string{}, c.Socks.Allowed...)
	c, e := decode(canonical(c))
	if e != nil {
		t.Fatal(e)
	}
	joined := strings.Join(c.Socks.Allowed, ",")
	for _, v := range []string{"0.0.0.0/0", "::/0", "0.0.0.0/32", "192.0.2.0/24", "2001:db8::1/128", "192.0.2.1/32"} {
		if !strings.Contains(joined, v) {
			t.Fatal(v, joined)
		}
	}
	c.Gateway.Allowed = []string{"broken/0"}
	if _, e = decode(canonical(c)); e == nil {
		t.Fatal("invalid source accepted")
	}
}
func TestEgressMatrixRendering(t *testing.T) {
	for _, v4 := range []string{"native", "warp"} {
		for _, v6 := range []string{"native", "warp"} {
			c, _ := decode(fixture(t))
			c.Mode = "both"
			c.Egress = Egress{v4, v6}
			c.Gateway.Interface = "lan1"
			for _, nat := range []bool{false, true} {
				c.Gateway.NAT = Masquerade{nat, nat}
				out := t.TempDir()
				if e := render(c, out); e != nil {
					t.Fatal(e)
				}
				n, _ := os.ReadFile(filepath.Join(out, "gateway.nft"))
				text := string(n)
				for _, family := range []string{"ip saddr { 0.0.0.0/0 }", "ip6 saddr { ::/0 }"} {
					if !strings.Contains(text, family) {
						t.Fatal(text)
					}
				}
				if strings.Contains(text, "masquerade") != nat {
					t.Fatal(text)
				}
				if !strings.Contains(text, `table inet warpdeck_gateway`) || !strings.Contains(text, `iifname "lan1"`) {
					t.Fatal(text)
				}
				env, _ := os.ReadFile(filepath.Join(out, "settings.env"))
				if !strings.Contains(string(env), "EGRESS4="+v4) || !strings.Contains(string(env), "EGRESS6="+v6) {
					t.Fatal(string(env))
				}
			}
		}
	}
}
func TestModesEmptySourcesAndUDP(t *testing.T) {
	c, _ := decode(fixture(t))
	out := t.TempDir()
	c.Socks.Allowed = []string{}
	c.Gateway.Allowed = []string{}
	c.Socks.UDP = false
	if e := render(c, out); e != nil {
		t.Fatal(e)
	}
	p, _ := os.ReadFile(filepath.Join(out, "3proxy.cfg"))
	n, _ := os.ReadFile(filepath.Join(out, "gateway.nft"))
	if strings.Contains(string(p), "allow *") || strings.Contains(string(n), " accept\n") || strings.Contains(string(p), "UDPASSOC") {
		t.Fatal("empty sources or proxy mode opens service")
	}
	c, _ = decode(fixture(t))
	c.Mode = "proxy"
	_ = render(c, out)
	n, _ = os.ReadFile(filepath.Join(out, "gateway.nft"))
	if strings.Contains(string(n), " accept\n") {
		t.Fatal("proxy mode forwards traffic")
	}
}
func TestV1Compatibility(t *testing.T) {
	old := []byte(`{"version":1,"mode":"both","ipv6":"native","socks":{"listen":"192.0.2.2","port":1080,"allowed_sources":["192.0.2.1/32"],"max_connections":32,"resolution":"prefer_ipv6","udp":true},"gateway":{"interface":"eth0","allowed_sources":["192.0.2.1/32"],"tcp_mss":1240}}`)
	c, e := decode(old)
	if e != nil {
		t.Fatal(e)
	}
	if c.Version != 2 || c.Mode != "both" || c.Egress != (Egress{"warp", "native"}) || c.Gateway.NAT != (Masquerade{true, false}) || strings.Join(c.Gateway.Allowed, ",") != "192.0.2.1/32" {
		t.Fatal(c)
	}
	again, e := decode(canonical(c))
	if e != nil || digest(c) != digest(again) {
		t.Fatal("migration not idempotent", e)
	}
	bad := strings.Replace(string(old), `"ipv6":"native"`, `"ipv6":"warp"`, 1)
	if _, e = decode([]byte(bad)); e == nil {
		t.Fatal("accepted unsupported v1 behavior")
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
	for i := 0; i < 10; i++ {
		if e := atomic(p, canonical(c)); e != nil {
			t.Fatal(e)
		}
		if _, e := load(p); e != nil {
			t.Fatal(e)
		}
	}
}
