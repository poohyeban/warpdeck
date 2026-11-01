package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const maxSnapshotBytes = 256 * 1024

type journal struct{ dir string }

func logDirectory() string {
	if s := os.Getenv("WARPDECK_LOG_DIR"); s != "" {
		return s
	}
	return stateDir + "/logs"
}

// Files are opened only for one append. Rename rotation never races a held
// descriptor, and no copytruncate window can lose an incident record.
func appendRotated(dir, name string, b []byte, maxBytes int64, copies int) error {
	if copies < 1 || int64(len(b)+1) > maxBytes {
		return errors.New("log record exceeds limit")
	}
	if e := os.MkdirAll(dir, 0700); e != nil {
		return e
	}
	p := filepath.Join(dir, name)
	if st, e := os.Lstat(p); e == nil {
		if !st.Mode().IsRegular() {
			return errors.New("log path is not a regular file")
		}
		if st.Size()+int64(len(b)+1) > maxBytes {
			for i := copies - 1; i >= 1; i-- {
				src := p
				if i > 1 {
					src = fmt.Sprintf("%s.%d", p, i-1)
				}
				if e := os.Rename(src, fmt.Sprintf("%s.%d", p, i)); e != nil && !os.IsNotExist(e) {
					return e
				}
			}
			if copies == 1 {
				if e := os.Remove(p); e != nil {
					return e
				}
			}
		}
	} else if !os.IsNotExist(e) {
		return e
	}
	f, e := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if e != nil {
		return e
	}
	defer f.Close()
	if _, e = f.Write(append(b, '\n')); e != nil {
		return e
	}
	return f.Sync()
}

func (j journal) write(stream, event, id string, data any) error {
	record := map[string]any{"time": time.Now().UTC().Format(time.RFC3339Nano), "event": event, "incident_id": id, "data": data}
	b, e := json.Marshal(record)
	if e != nil {
		return e
	}
	copies := 6
	if stream == "events" {
		copies = 4
	}
	return appendRotated(j.dir, stream+".jsonl", b, 5*1024*1024, copies)
}

// Deliberately never collect registration/settings files, environment variables,
// process arguments, customer access logs, or unfiltered daemon output.
var sensitiveLine = regexp.MustCompile(`(?i)token|secret|private.?key|public.?key|license|account|registration|device.?id|authorization|cookie|certificate|settings|configuration`)
var urlQuery = regexp.MustCompile(`(https?://[^\s?"<>]+)\?[^\s"<>]*`)
var longCredential = regexp.MustCompile(`[A-Za-z0-9_+/=-]{48,}`)

func cleanDiagnostic(s string) string {
	s = urlQuery.ReplaceAllString(s, "$1?[redacted]")
	s = longCredential.ReplaceAllString(s, "[redacted-long-value]")
	s = strings.ReplaceAll(s, "\x00", "")
	if len(s) > 2048 {
		s = s[:2048] + " [truncated]"
	}
	return s
}

type boundedOutput struct {
	bytes.Buffer
	limit     int
	truncated bool
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	n := len(p)
	left := b.limit - b.Len()
	if left < n {
		b.truncated = true
	}
	if left > 0 {
		if left > n {
			left = n
		}
		_, _ = b.Buffer.Write(p[:left])
	}
	return n, nil
}
func diagnosticCommand(ctx context.Context, args ...string) map[string]any {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out := &boundedOutput{limit: 8192}
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Stdout = out
	cmd.Stderr = out
	e := cmd.Run()
	code := 0
	if e != nil {
		code = -1
		if x, ok := e.(*exec.ExitError); ok {
			code = x.ExitCode()
		}
	}
	return map[string]any{"command": args, "exit_code": code, "timed_out": ctx.Err() != nil, "truncated": out.truncated, "output": scrubOutput(out.String())}
}

func scrubOutput(s string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		if sensitiveLine.MatchString(line) {
			lines[i] = "[redacted sensitive diagnostic line]"
		} else {
			lines[i] = cleanDiagnostic(line)
		}
	}
	return strings.Join(lines, "\n")
}

func tailFile(p string, limit int64) ([]byte, error) {
	f, e := os.Open(p)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	s, e := f.Stat()
	if e != nil {
		return nil, e
	}
	start := s.Size() - limit
	if start > 0 {
		if _, e = f.Seek(start, io.SeekStart); e != nil {
			return nil, e
		}
	}
	b, e := io.ReadAll(io.LimitReader(f, limit))
	if start > 0 {
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			b = b[i+1:]
		}
	}
	return b, e
}

func officialStats() []map[string]any {
	b, _ := tailFile("/var/lib/cloudflare-warp/cfwarp_service_connection_stats.txt", 65536)
	var rows []map[string]any
	keys := []string{"stats_type", "protocol", "time_since_handshake_ms", "estimated_loss_pct", "estimated_latency_ms", "client_tunnel_packets_sent", "client_tunnel_packets_recvd", "client_packets_sent", "client_packets_recvd", "client_packets_lost", "client_bytes_sent", "client_bytes_recvd", "client_rtt_us", "client_pmtu", "edge_rtt_us", "edge_packets_recvd", "edge_packets_sent", "edge_packets_lost", "edge_pmtu", "client_dgram_drops"}
	for _, line := range bytes.Split(b, []byte{'\n'}) {
		var row struct {
			Timestamp string         `json:"timestamp"`
			Fields    map[string]any `json:"fields"`
		}
		if json.Unmarshal(line, &row) != nil {
			continue
		}
		x := map[string]any{"time": row.Timestamp}
		for _, k := range keys {
			if v, ok := row.Fields[k]; ok {
				x[k] = v
			}
		}
		rows = append(rows, x)
	}
	if len(rows) > 12 {
		rows = rows[len(rows)-12:]
	}
	return rows
}

func officialWarnings() []string {
	b, _ := tailFile("/var/lib/cloudflare-warp/cfwarp_service_log.txt", 65536)
	var lines []string
	for _, line := range strings.Split(string(b), "\n") {
		if sensitiveLine.MatchString(line) {
			continue
		}
		if !strings.Contains(line, "WARN") && !strings.Contains(line, "ERROR") {
			continue
		}
		// Keep only networking subsystem diagnostics, not arbitrary daemon logs.
		if !strings.Contains(line, "tunnel") && !strings.Contains(line, "quiche") && !strings.Contains(line, "connectivity") && !strings.Contains(line, "firewall") && !strings.Contains(line, "watchdog") {
			continue
		}
		lines = append(lines, cleanDiagnostic(line))
	}
	if len(lines) > 20 {
		lines = lines[len(lines)-20:]
	}
	return lines
}

func (j journal) snapshot(id, phase, revision string, results any) error {
	if !regexp.MustCompile(`^[0-9TZ-]+$`).MatchString(id) || !regexp.MustCompile(`^(initial|restored|attempt-[0-9]+-(before|after))$`).MatchString(phase) {
		return errors.New("invalid incident snapshot name")
	}
	root := filepath.Join(j.dir, "incidents")
	if e := pruneIncidents(root, id, time.Now()); e != nil {
		return e
	}
	dir := filepath.Join(root, id)
	if e := os.MkdirAll(dir, 0700); e != nil {
		return e
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	commands := [][]string{
		{"warp-cli", "--version"}, {"warp-cli", "--accept-tos", "status"}, {"warp-cli", "--accept-tos", "tunnel", "stats"},
		{"ip", "-s", "link", "show", "CloudflareWARP"}, {"ip", "-br", "addr"},
		{"ip", "-4", "rule"}, {"ip", "-6", "rule"},
		{"ip", "-4", "route", "show", "table", "1091"}, {"ip", "-6", "route", "show", "table", "1091"},
		{"ip", "-4", "route", "show", "table", "1094"}, {"ip", "-6", "route", "show", "table", "1094"},
		{"ip", "-4", "route", "show", "table", "1092"}, {"ip", "-6", "route", "show", "table", "1092"},
		{"nft", "list", "table", "inet", "warpdeck_gateway"},
		{"nft", "list", "chain", "inet", "cloudflare-warp", "tun"},
		{"ip", "-4", "route", "get", "1.1.1.1", "uid", "65534", "mark", "0x10910000"},
		{"ip", "-6", "route", "get", "2606:4700:4700::1111", "uid", "65534", "mark", "0x10910000"},
		{"cat", "/sys/fs/cgroup/memory.current", "/sys/fs/cgroup/memory.max", "/sys/fs/cgroup/memory.events", "/sys/fs/cgroup/pids.current", "/sys/fs/cgroup/pids.max"},
	}
	var diagnostics []map[string]any
	for _, args := range commands {
		diagnostics = append(diagnostics, diagnosticCommand(ctx, args...))
	}
	data := map[string]any{"time": time.Now().UTC().Format(time.RFC3339Nano), "incident_id": id, "phase": phase, "config_revision": revision, "probes": results, "diagnostics": diagnostics, "official_connection_stats": officialStats(), "official_network_warnings": officialWarnings()}
	b, e := json.MarshalIndent(data, "", "  ")
	if e != nil {
		return e
	}
	if len(b) > maxSnapshotBytes {
		return errors.New("snapshot exceeds 256 KiB limit")
	}
	p := filepath.Join(dir, phase+".json")
	// A synced file is required before requesting recovery. Only its fixed path
	// is replaced, so persistent outages cannot create unbounded snapshots.
	f, e := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if e != nil {
		return e
	}
	_, writeErr := f.Write(b)
	syncErr := f.Sync()
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	if syncErr != nil {
		return syncErr
	}
	if closeErr != nil {
		return closeErr
	}
	return pruneSnapshots(dir)
}

// At most 20 incident directories, each with eight <=256 KiB snapshots. Age and
// count are both enforced; unrelated files are never removed.
func pruneIncidents(root, keep string, now time.Time) error {
	if e := os.MkdirAll(root, 0700); e != nil {
		return e
	}
	entries, e := os.ReadDir(root)
	if e != nil {
		return e
	}
	var names []string
	for _, entry := range entries {
		name := entry.Name()
		when, e := time.Parse("20060102T150405Z", name)
		if e != nil || !entry.IsDir() || name == keep {
			continue
		}
		if now.Sub(when) > 30*24*time.Hour {
			if e := os.RemoveAll(filepath.Join(root, name)); e != nil {
				return e
			}
		} else {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	limit := 20
	if keep != "" {
		limit = 19
	}
	for len(names) > limit {
		if e := os.RemoveAll(filepath.Join(root, names[0])); e != nil {
			return e
		}
		names = names[1:]
	}
	return nil
}

// Keep the original failure, recovery confirmation, and the latest three
// recovery attempts (before/after). Prune only names owned by this monitor.
func pruneSnapshots(dir string) error {
	entries, e := os.ReadDir(dir)
	if e != nil {
		return e
	}
	pattern := regexp.MustCompile(`^attempt-([0-9]+)-(before|after)\.json$`)
	groups := map[int][]string{}
	for _, entry := range entries {
		m := pattern.FindStringSubmatch(entry.Name())
		if m == nil {
			continue
		}
		n, e := strconv.Atoi(m[1])
		if e != nil {
			continue
		}
		groups[n] = append(groups[n], entry.Name())
	}
	keys := []int{}
	for n := range groups {
		keys = append(keys, n)
	}
	sort.Ints(keys)
	for len(keys) > 3 {
		for _, name := range groups[keys[0]] {
			if e := os.Remove(filepath.Join(dir, name)); e != nil {
				return e
			}
		}
		keys = keys[1:]
	}
	return nil
}
