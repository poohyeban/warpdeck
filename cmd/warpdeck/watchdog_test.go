package main

import (
	"bytes"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSOCKSConnectUsesExactDestinationFamily(t *testing.T) {
	for _, tc := range []struct {
		host string
		kind byte
		n    int
	}{{"1.1.1.1", 1, 4}, {"2606:4700:4700::1111", 4, 16}, {"www.cloudflare.com", 3, 19}} {
		t.Run(tc.host, func(t *testing.T) {
			client, server := net.Pipe()
			defer client.Close()
			done := make(chan error, 1)
			go func() {
				defer server.Close()
				hello := make([]byte, 3)
				_, e := io.ReadFull(server, hello)
				if e != nil {
					done <- e
					return
				}
				_, _ = server.Write([]byte{5, 0})
				head := make([]byte, 4)
				_, e = io.ReadFull(server, head)
				if e != nil {
					done <- e
					return
				}
				if head[3] != tc.kind {
					done <- io.ErrUnexpectedEOF
					return
				}
				n := tc.n
				if tc.kind == 3 {
					b := make([]byte, 1)
					_, _ = io.ReadFull(server, b)
					n = int(b[0])
				}
				b := make([]byte, n+2)
				_, e = io.ReadFull(server, b)
				_, _ = server.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
				done <- e
			}()
			if e := socksConnect(client, tc.host, 443); e != nil {
				t.Fatal(e)
			}
			if e := <-done; e != nil {
				t.Fatal(e)
			}
		})
	}
}

func TestSOCKSListenerAloneIsNotSuccess(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	_ = client.SetDeadline(time.Now().Add(40 * time.Millisecond))
	go func() {
		hello := make([]byte, 3)
		_, _ = io.ReadFull(server, hello)
		_, _ = server.Write([]byte{5, 0})
		_, _ = io.Copy(io.Discard, server)
	}()
	if e := socksConnect(client, "1.1.1.1", 443); e == nil {
		t.Fatal("accepted a proxy that never completed CONNECT")
	}
}

func TestHTTPSProbeRequiresTLSStatusBodyAndEgress(t *testing.T) {
	for _, tc := range []struct {
		body   string
		status int
		ok     bool
	}{{"warp=on\n", 200, true}, {"warp=off\n", 200, false}, {"warp=on\n", 503, false}, {"unrelated page", 200, false}, {strings.Repeat("x", 16385), 200, false}} {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = io.WriteString(w, tc.body)
		}))
		roots := x509.NewCertPool()
		roots.AddCert(server.Certificate())
		spec := probeSpec{Family: "ipv4", Target: "cloudflare-trace", URL: server.URL, Warp: true}
		got := requestProbeTLS(spec, roots)
		if got.OK != tc.ok {
			t.Fatalf("status=%d body=%q result=%+v", tc.status, tc.body[:min(len(tc.body), 30)], got)
		}
		// Production never disables certificate verification.
		if got := requestProbe(spec); got.OK || got.Stage != "tls" {
			t.Fatalf("untrusted TLS accepted: %+v", got)
		}
		server.Close()
	}
}

func TestJournalRotationBoundedAndRecordIntegrity(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 80; i++ {
		b, _ := json.Marshal(map[string]any{"sequence": i, "payload": strings.Repeat("x", 40)})
		if e := appendRotated(dir, "checks.jsonl", b, 220, 3); e != nil {
			t.Fatal(e)
		}
	}
	files, e := os.ReadDir(dir)
	if e != nil || len(files) != 3 {
		t.Fatal(files, e)
	}
	last := -1
	for _, name := range []string{"checks.jsonl.2", "checks.jsonl.1", "checks.jsonl"} {
		b, e := os.ReadFile(filepath.Join(dir, name))
		if e != nil || len(b) > 220 {
			t.Fatal(len(b), e)
		}
		st, _ := os.Stat(filepath.Join(dir, name))
		if st.Mode().Perm() != 0600 {
			t.Fatal("unsafe log permissions")
		}
		for _, line := range bytes.Split(bytes.TrimSpace(b), []byte{'\n'}) {
			var row struct{ Sequence int }
			if e := json.Unmarshal(line, &row); e != nil {
				t.Fatal(e)
			}
			if row.Sequence <= last {
				t.Fatal("record order lost")
			}
			last = row.Sequence
		}
	}
	if last != 79 {
		t.Fatal("latest record lost")
	}
	if e := appendRotated(dir, "checks.jsonl", make([]byte, 221), 220, 3); e == nil {
		t.Fatal("oversized record accepted")
	}
}

func TestIncidentRetentionAndUnknownFiles(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()
	for i := 0; i < 25; i++ {
		if e := os.Mkdir(filepath.Join(root, now.Add(-time.Duration(i)*time.Hour).Format("20060102T150405Z")), 0700); e != nil {
			t.Fatal(e)
		}
	}
	old := now.Add(-31 * 24 * time.Hour).Format("20060102T150405Z")
	_ = os.Mkdir(filepath.Join(root, old), 0700)
	_ = os.WriteFile(filepath.Join(root, "operator-notes"), []byte("keep"), 0600)
	if e := pruneIncidents(root, "", now); e != nil {
		t.Fatal(e)
	}
	files, _ := os.ReadDir(root)
	if len(files) != 21 {
		t.Fatal("retention did not cap incident count", len(files))
	}
	if _, e := os.Stat(filepath.Join(root, old)); !os.IsNotExist(e) {
		t.Fatal("expired incident retained")
	}
	if _, e := os.Stat(filepath.Join(root, "operator-notes")); e != nil {
		t.Fatal("unrelated file removed")
	}
}

func TestDiagnosticsDoNotPersistSecrets(t *testing.T) {
	for _, line := range []string{"api_token=abc", "private_key=secret", "registration_id=xyz", "Authorization: Bearer value", "cookie: a=b", "account_id=123"} {
		if !sensitiveLine.MatchString(line) {
			t.Fatal("sensitive daemon line accepted", line)
		}
	}
	s := cleanDiagnostic("https://example.com/path?token=hidden " + strings.Repeat("A", 64))
	if strings.Contains(s, "hidden") || strings.Contains(s, strings.Repeat("A", 64)) {
		t.Fatal("credential not redacted")
	}
	buf := &boundedOutput{limit: 16}
	n, e := buf.Write(make([]byte, 100))
	if n != 100 || e != nil || buf.Len() != 16 || !buf.truncated {
		t.Fatal("unbounded command capture")
	}
}

func TestLocalSOCKSRespectsAllowlist(t *testing.T) {
	c, _ := decode(fixture(t))
	c.Socks.Listen = "0.0.0.0"
	c.Socks.Allowed = []string{"127.0.0.1/32"}
	if endpoint, source := localSOCKS(c); endpoint != "127.0.0.1:1080" || source != "127.0.0.1" {
		t.Fatal(endpoint, source)
	}
	c.Socks.Allowed = []string{}
	if endpoint, _ := localSOCKS(c); endpoint != "" {
		t.Fatal("monitor bypasses deny-all")
	}
	c.Egress.IPv6 = "native"
	if r := checkFamily(c, "ipv6", "native"); !r.OK || r.TunnelFailed || len(r.Probes) != 0 {
		t.Fatal("native family triggers WARP recovery", r)
	}
}

func TestRecoveryConfirmationAndFixedCooldown(t *testing.T) {
	c := recoveryState{}
	bad := []familyResult{{Family: "ipv4", Policy: "warp", TunnelFailed: true}, {Family: "ipv6", Policy: "warp", OK: true}}
	for i := 0; i < 2; i++ {
		c.observe(bad)
		if c.eligibleAction(bad) != "" {
			t.Fatal("recovered before three failures")
		}
	}
	c.observe(bad)
	if c.eligibleAction(bad) != "tunnel" {
		t.Fatal("confirmed outage ignored")
	}
	now := time.Now().UTC()
	for i := 0; i < 30; i++ {
		at := now.Add(time.Duration(i) * recoveryCooldown)
		if !c.allowed(at) {
			t.Fatal("did not resume after cooldown")
		}
		c.reserve(at)
		if c.allowed(at.Add(recoveryCooldown - time.Second)) {
			t.Fatal("recovery rate exceeded")
		}
	}
	good := []familyResult{{Family: "ipv4", Policy: "warp", OK: true}, {Family: "ipv6", Policy: "warp", OK: true}}
	if !c.observe(good) || c.eligibleAction(bad) != "" {
		t.Fatal("healthy probes did not clear failures")
	}
	if c.allowed(c.LastAttempt.Add(time.Minute)) {
		t.Fatal("healthy probe bypassed cooldown")
	}
}

func TestRecoveryCooldownSurvivesRestart(t *testing.T) {
	c := recoveryState{Attempts: 5, Incident: "20261101T000000Z", LastAttempt: time.Now()}
	dir := t.TempDir()
	if e := persistRecovery(dir, c); e != nil {
		t.Fatal(e)
	}
	b, e := os.ReadFile(filepath.Join(dir, "recovery-state.json"))
	if e != nil {
		t.Fatal(e)
	}
	var restored recoveryState
	if e = json.Unmarshal(b, &restored); e != nil {
		t.Fatal(e)
	}
	if restored.allowed(time.Now()) || restored.Attempts != 5 || restored.Incident != c.Incident {
		t.Fatal("restart bypassed circuit", restored)
	}
}

func TestAlternatingFamiliesAndSOCKSFailureClassification(t *testing.T) {
	c := recoveryState{}
	for i := 0; i < 12; i++ {
		r := []familyResult{{Family: "ipv4", Policy: "warp", OK: i%2 == 0, TunnelFailed: i%2 != 0}, {Family: "ipv6", Policy: "warp", OK: i%2 != 0, TunnelFailed: i%2 == 0}}
		c.observe(r)
		if c.eligibleAction(r) != "" {
			t.Fatal("mixed address-family failures accumulated")
		}
	}
	r := []familyResult{{Family: "ipv4", Policy: "warp", ProxyFailed: true}, {Family: "ipv6", Policy: "native", OK: true}}
	for i := 0; i < 3; i++ {
		c.observe(r)
	}
	if c.eligibleAction(r) != "proxy" {
		t.Fatal("SOCKS failure must not restart WARP")
	}
}

func TestSnapshotRotationKeepsOriginalAndLatestAttempts(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"initial.json", "restored.json", "operator-note"} {
		_ = os.WriteFile(filepath.Join(dir, name), []byte("keep"), 0600)
	}
	for i := 1; i <= 8; i++ {
		for _, phase := range []string{"before", "after"} {
			_ = os.WriteFile(filepath.Join(dir, fmt.Sprintf("attempt-%06d-%s.json", i, phase)), []byte("snapshot"), 0600)
		}
	}
	if e := pruneSnapshots(dir); e != nil {
		t.Fatal(e)
	}
	files, _ := os.ReadDir(dir)
	if len(files) != 9 {
		t.Fatal("wrong snapshot count", len(files))
	}
	for _, name := range []string{"initial.json", "restored.json", "attempt-000006-before.json", "attempt-000008-after.json", "operator-note"} {
		if _, e := os.Stat(filepath.Join(dir, name)); e != nil {
			t.Fatal(name, e)
		}
	}
}
