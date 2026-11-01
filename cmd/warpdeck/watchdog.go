package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const monitorVersion = "0.0.3"
const checkInterval = 60 * time.Second
const recoveryCooldown = 10 * time.Minute

// Continuous minute probes, three failures to confirm, and a persistent
// ten-minute minimum between automatic recovery actions.
type recoveryState struct {
	Attempts    int            `json:"attempts"`
	Failures    map[string]int `json:"failures"`
	LastAttempt time.Time      `json:"last_attempt"`
	Incident    string         `json:"incident_id"`
	Revision    string         `json:"config_revision"`
}

func (c *recoveryState) observe(results []familyResult) bool {
	if c.Failures == nil {
		c.Failures = map[string]int{}
	}
	allOK := true
	for _, r := range results {
		if r.Policy != "warp" {
			continue
		}
		if r.OK {
			c.Failures[r.Family] = 0
		} else {
			allOK = false
			if c.Failures[r.Family] < 3 {
				c.Failures[r.Family]++
			}
		}
	}
	return allOK
}
func (c recoveryState) eligibleAction(results []familyResult) string {
	action := ""
	for _, r := range results {
		if c.Failures[r.Family] < 3 {
			continue
		}
		if r.TunnelFailed {
			return "tunnel"
		}
		if r.ProxyFailed {
			action = "proxy"
		}
	}
	return action
}
func (c recoveryState) allowed(now time.Time) bool {
	return c.LastAttempt.IsZero() || now.Sub(c.LastAttempt) >= recoveryCooldown
}
func (c *recoveryState) reserve(now time.Time) {
	c.Attempts++
	c.LastAttempt = now
}

func (c recoveryState) nextAllowed() time.Time {
	if c.LastAttempt.IsZero() {
		return time.Time{}
	}
	return c.LastAttempt.Add(recoveryCooldown)
}

type monitorStatus struct {
	Time         time.Time      `json:"time"`
	Revision     string         `json:"config_revision"`
	State        string         `json:"state"`
	Reason       string         `json:"reason"`
	Incident     string         `json:"incident_id,omitempty"`
	Attempts     int            `json:"recovery_attempts"`
	NextRecovery time.Time      `json:"next_recovery_not_before"`
	Results      []familyResult `json:"families,omitempty"`
	DNS          *probeResult   `json:"domain_probe,omitempty"`
	LogError     string         `json:"log_error,omitempty"`
}

func readMonitorStatus() monitorStatus {
	var s monitorStatus
	b, _ := os.ReadFile(runDir + "/monitor-status.json")
	_ = json.Unmarshal(b, &s)
	return s
}
func monitorHealth() error {
	s := readMonitorStatus()
	if s.Time.IsZero() || time.Since(s.Time) > 3*time.Minute {
		return errors.New("connectivity monitor missing or stale")
	}
	if s.LogError != "" {
		return errors.New("connectivity diagnostic logging failed")
	}
	switch s.State {
	case "starting", "healthy", "not_required", "suspect":
		return nil
	}
	return fmt.Errorf("connectivity %s: %s (incident %s)", s.State, s.Reason, s.Incident)
}

func persistRecovery(dir string, c recoveryState) error {
	b, e := json.Marshal(c)
	if e != nil {
		return e
	}
	if e = os.MkdirAll(dir, 0700); e != nil {
		return e
	}
	p := filepath.Join(dir, "recovery-state.json")
	f, e := os.OpenFile(p+".tmp", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if e != nil {
		return e
	}
	_, e = f.Write(b)
	if e == nil {
		e = f.Sync()
	}
	closeErr := f.Close()
	if e != nil {
		return e
	}
	if closeErr != nil {
		return closeErr
	}
	if e = os.Rename(p+".tmp", p); e != nil {
		return e
	}
	d, e := os.Open(dir)
	if e != nil {
		return e
	}
	defer d.Close()
	return d.Sync()
}

func underlayProbe() ([]probeResult, bool) {
	results := make([]probeResult, 2)
	var wg sync.WaitGroup
	for i, family := range []string{"ipv4", "ipv6"} {
		wg.Add(1)
		go func(i int, family string) {
			defer wg.Done()
			for _, s := range targets(family, false) {
				results[i] = childProbe(s, false)
				results[i].Path = "underlay"
				if results[i].OK {
					return
				}
			}
		}(i, family)
	}
	wg.Wait()
	return results, results[0].OK || results[1].OK
}

func recoverThroughSupervisor(action, id, revision string) map[string]any {
	_ = os.Remove(runDir + "/recovery-result")
	if e := wake("recover " + action + " " + id + " " + revision); e != nil {
		return map[string]any{"outcome": "request_failed", "error": e.Error()}
	}
	deadline := time.Now().Add(35 * time.Second)
	for time.Now().Before(deadline) {
		fields := strings.Fields(read(runDir + "/recovery-result"))
		if len(fields) == 5 && fields[0] == id && fields[1] == action {
			out, _ := tailFile(runDir+"/recovery-command-output", 4096)
			return map[string]any{"outcome": fields[2], "disconnect_exit": fields[3], "connect_or_proxy_exit": fields[4], "command_output": cleanDiagnostic(string(out)), "verification": "pending next scheduled probe"}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return map[string]any{"outcome": "supervisor_timeout", "verification": "pending next scheduled probe"}
}

func monitor() error {
	lock, e := os.OpenFile(runDir+"/monitor.lock", os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return e
	}
	defer lock.Close()
	if e = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		return errors.New("connectivity monitor already running")
	}
	j := journal{logDirectory()}
	recovery := recoveryState{}
	if b, e := os.ReadFile(filepath.Join(j.dir, "recovery-state.json")); e == nil {
		if e = json.Unmarshal(b, &recovery); e != nil {
			return errors.New("recovery-state.json is invalid; refusing to reset recovery cooldown")
		}
	} else if !os.IsNotExist(e) {
		return e
	}
	if e = j.write("events", "monitor_started", recovery.Incident, map[string]any{"version": monitorVersion, "interval_seconds": 60, "confirm_failures": 3, "cooldown_seconds": 600, "restored_recovery": recovery}); e != nil {
		return e
	}
	var revision string
	next := time.Now().Add(checkInterval)
	var lastStatus monitorStatus
	saveStatus := func(s monitorStatus) {
		s.Time = time.Now().UTC()
		lastStatus = s
		_ = atomic(runDir+"/monitor-status.json", canonical(s))
	}
	saveStatus(monitorStatus{State: "starting", Reason: "60 second startup grace"})
	for {
		time.Sleep(time.Second)
		activeRevision := read(runDir + "/applied-revision")
		if activeRevision == "" {
			saveStatus(monitorStatus{State: "starting", Reason: "waiting for applied configuration"})
			continue
		}
		if activeRevision != revision {
			revision = activeRevision
			next = time.Now().Add(checkInterval)
			recovery.Failures = nil
			recovery.Revision = revision
			// Preserve attempts/cooldown across reload and process restarts.
			if e = j.write("events", "configuration_observed", recovery.Incident, map[string]any{"revision": revision, "grace_seconds": 60}); e != nil {
				return e
			}
			saveStatus(monitorStatus{Revision: revision, State: "starting", Reason: "configuration grace; recovery cooldown retained"})
		}
		if time.Now().Before(next) {
			continue
		}
		started := time.Now()
		next = started.Add(checkInterval)
		c, e := load(runDir + "/active/config.json")
		if e != nil {
			saveStatus(monitorStatus{State: "error", Reason: "cannot read applied configuration"})
			continue
		}
		if digest(c) != revision {
			continue
		}
		if c.Egress.IPv4 != "warp" && c.Egress.IPv6 != "warp" {
			saveStatus(monitorStatus{Revision: revision, State: "not_required", Reason: "no WARP egress selected"})
			continue
		}
		results := checkFamilies(c)
		if read(runDir+"/applied-revision") != revision {
			continue
		}
		allOK := recovery.observe(results)
		s := monitorStatus{Revision: revision, State: "healthy", Results: results, Incident: recovery.Incident, Attempts: recovery.Attempts, NextRecovery: recovery.nextAllowed()}
		var loggingError error
		record := func(event string, data any) {
			if e := j.write("events", event, recovery.Incident, data); e != nil {
				loggingError = e
			}
		}
		if allOK {
			// A domain probe supplements the pinned tests; DNS failure alone
			// never requests WARP recovery. Mixed policies have no unique marker.
			if c.Egress.IPv4 == "warp" && c.Egress.IPv6 == "warp" && c.Mode != "gateway" {
				if endpoint, source := localSOCKS(c); endpoint != "" {
					dns := childProbe(probeSpec{Family: "domain", Target: "cloudflare-domain", URL: "https://www.cloudflare.com/cdn-cgi/trace", Proxy: endpoint, Source: source, Warp: true}, false)
					s.DNS = &dns
					if !dns.OK {
						s.State = "dns_degraded"
						s.Reason = "IP probes work; domain path failed; no tunnel recovery"
					}
				}
			}
			if recovery.Incident != "" {
				record("connectivity_restored", map[string]any{"verification": results, "attempts": recovery.Attempts})
				if e = j.snapshot(recovery.Incident, "restored", revision, results); e != nil {
					loggingError = e
				}
			}
			if recovery.Incident != "" {
				record("incident_closed", map[string]any{"attempts": recovery.Attempts})
				recovery = recoveryState{Revision: revision, LastAttempt: recovery.LastAttempt}
				s.Incident = ""
				s.Attempts = 0
			}
		} else {
			if recovery.Incident == "" {
				recovery.Incident = time.Now().UTC().Format("20060102T150405Z")
				s.Incident = recovery.Incident
				record("incident_opened", map[string]any{"config_revision": revision, "families": results, "scope": "HTTPS via SOCKS where locally allowed; otherwise policy-route. External gateway clients and UDP are not covered."})
				if e = j.snapshot(recovery.Incident, "initial", revision, results); e != nil {
					loggingError = e
				}
			}
			s.State = "suspect"
			s.Reason = "awaiting three consecutive failures of the same address family"
			action := recovery.eligibleAction(results)
			if action != "" {
				s.State = "unhealthy"
				s.Reason = "confirmed connectivity failure"
				canRecover := true
				if action == "tunnel" {
					underlay, ok := underlayProbe()
					canRecover = ok
					record("underlay_checked", map[string]any{"reachable": ok, "probes": underlay})
					if !ok {
						s.State = "underlay_down"
						s.Reason = "native control probes failed; waiting for native connectivity; no WARP reconnection"
					}
				}
				if canRecover && !recovery.allowed(time.Now()) {
					s.State = "cooldown"
					s.Reason = "reconnection rate limited; probing continues every 60 seconds"
				}
				if canRecover && recovery.allowed(time.Now()) {
					// Durable evidence and reservation precede the side effect.
					if e = j.snapshot(recovery.Incident, fmt.Sprintf("attempt-%06d-before", recovery.Attempts+1), revision, results); e != nil {
						loggingError = e
					}
					if loggingError == nil {
						recovery.reserve(time.Now().UTC())
						if e = persistRecovery(j.dir, recovery); e != nil {
							loggingError = e
						}
						record("recovery_requested", map[string]any{"action": action, "attempt": recovery.Attempts, "revision": revision, "next_recovery_not_before": recovery.nextAllowed(), "cooldown_seconds": 600})
						if loggingError == nil && read(runDir+"/applied-revision") == revision {
							saveStatus(monitorStatus{Revision: revision, State: "recovering", Incident: recovery.Incident, Attempts: recovery.Attempts, NextRecovery: recovery.nextAllowed(), Reason: "supervisor recovery in progress"})
							outcome := recoverThroughSupervisor(action, recovery.Incident, revision)
							record("recovery_command_finished", outcome)
							if e = j.snapshot(recovery.Incident, fmt.Sprintf("attempt-%06d-after", recovery.Attempts), revision, outcome); e != nil {
								loggingError = e
							}
							s.State = "recovering"
							s.Reason = "waiting for next scheduled probe to verify actual connectivity"

						}
					}
				}
			}
		}
		s.Attempts = recovery.Attempts
		s.NextRecovery = recovery.nextAllowed()
		if e = j.write("checks", "connectivity_check", recovery.Incident, map[string]any{"started": started.UTC(), "finished": time.Now().UTC(), "status": s, "failure_counts": recovery.Failures}); e != nil {
			loggingError = e
		}
		if e = persistRecovery(j.dir, recovery); e != nil {
			loggingError = e
		}
		if e = pruneIncidents(filepath.Join(j.dir, "incidents"), recovery.Incident, time.Now()); e != nil {
			loggingError = e
		}
		if loggingError != nil {
			s.LogError = cleanDiagnostic(loggingError.Error())
			s.State = "error"
			s.Reason = "diagnostic persistence failed; recovery requires durable evidence"
			fmt.Fprintln(os.Stderr, "WarpDeck monitor:", s.Reason, s.LogError)
		}
		if s.State != lastStatus.State {
			record("monitor_state_changed", map[string]any{"from": lastStatus.State, "to": s.State, "reason": s.Reason})
		}
		saveStatus(s)
		// Never overlap rounds or run catch-up bursts after a long command.
		if time.Now().After(next) {
			next = time.Now().Add(checkInterval)
		}
	}
}
