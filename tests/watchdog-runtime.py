#!/usr/bin/env python3
"""Isolated monitor/supervisor integration test, without WARP registration.

Requires Linux Docker and native IPv4 Internet access. Uses a fake WARP daemon
and synthetic unreachable policy routes, then permits native test egress. This
tests watchdog decisions, IPC, logs and restart cooldown, not Cloudflare itself.
Never attaches production volumes, publishes ports or changes host rules.
"""
import argparse
import datetime
import json
import pathlib
import subprocess
import tempfile
import time
import uuid

parser = argparse.ArgumentParser()
parser.add_argument('--image', required=True)
args = parser.parse_args()
name = 'warpdeck-watchdog-test-' + uuid.uuid4().hex[:8]

def run(*command, check=True):
    result = subprocess.run(command, capture_output=True, text=True, timeout=60)
    if check and result.returncode:
        raise RuntimeError(f'{command[:4]}: {result.stderr[-500:]}')
    return result

def wait_for(predicate, seconds):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        try:
            if predicate():
                return
        except (FileNotFoundError, ValueError, KeyError):
            pass
        time.sleep(3)
    raise RuntimeError('timed out waiting for expected watchdog state')

with tempfile.TemporaryDirectory(prefix='warpdeck-watchdog-runtime-') as temp:
    root = pathlib.Path(temp)
    for directory in ['state', 'logs', 'control']:
        (root / directory).mkdir()
    config = json.loads(pathlib.Path(__file__).parents[1].joinpath('config.example.json').read_text())
    config['egress']['ipv6'] = 'native'
    (root / 'state/config.json').write_text(json.dumps(config))
    (root / 'control/allow-heal').touch()
    fake_cli = root / 'warp-cli'
    fake_cli.write_text('''#!/bin/bash
set -eu
[[ "${1:-}" != --accept-tos ]] || shift
case "$*" in
 status) echo 'Status update: Connected';;
 'registration show') echo 'synthetic test registration';;
 'tunnel ip list') printf '0.0.0.0/0\\n::/0\\n';;
 'tunnel stats') echo 'Synthetic integration-test daemon; no real WARP session';;
 connect)
   n=0; [[ ! -f /test-control/connect-count ]] || n=$(cat /test-control/connect-count)
   n=$((n+1)); echo "$n" > /test-control/connect-count
   if [[ "$n" -ge 2 && -e /test-control/allow-heal ]]; then
     gateway=$(ip -4 route show table 1092 | awk '$1=="default"&&$2=="via"{print $3;exit}')
     ip -4 route replace default via "$gateway" dev eth0 metric 100 table 1091
     ip -4 route replace default via "$gateway" dev eth0 metric 100 table 1094
   fi
   echo Success;;
 *) echo Success;;
esac
''')
    fake_cli.chmod(0o755)
    fake_svc = root / 'warp-svc'
    fake_svc.write_text('#!/bin/sh\nexec sleep infinity\n')
    fake_svc.chmod(0o755)
    last_state = [None]
    def status():
        result = run('docker', 'exec', name, 'cat', '/run/warpdeck/monitor-status.json', check=False)
        if result.returncode:
            return {}
        value = json.loads(result.stdout)
        state = value.get('state')
        if state != last_state[0]:
            print(datetime.datetime.now(datetime.timezone.utc).isoformat(), state, value.get('reason'), flush=True)
            last_state[0] = state
        return value
    def events():
        return [json.loads(line) for line in (root / 'logs/events.jsonl').read_text().splitlines()]
    try:
        run('docker', 'run', '-d', '--name', name, '--network', 'bridge', '--memory', '256m', '--pids-limit', '96',
            '--cap-add', 'NET_ADMIN', '--device', '/dev/net/tun', '--security-opt', 'no-new-privileges:true',
            '--sysctl', 'net.ipv6.conf.all.disable_ipv6=0', '--sysctl', 'net.ipv6.bindv6only=0',
            '--sysctl', 'net.ipv4.conf.all.rp_filter=0', '--sysctl', 'net.ipv4.conf.default.rp_filter=0',
            '-e', 'WARPDECK_LOG_DIR=/var/log/warpdeck',
            '-v', f'{root}/state:/var/lib/warpdeck', '-v', f'{root}/logs:/var/log/warpdeck',
            '-v', f'{root}/control:/test-control', '-v', f'{fake_cli}:/usr/local/bin/warp-cli:ro',
            '-v', f'{fake_svc}:/usr/local/bin/warp-svc:ro',
            '--log-opt', 'max-size=1m', '--log-opt', 'max-file=1', args.image)
        print('Synthetic tunnel outage; native control path remains reachable.', flush=True)
        wait_for(lambda: status().get('state') == 'healthy' and any(e['event'] == 'incident_closed' for e in events()), 380)
        history = events()
        requests = [e for e in history if e['event'] == 'recovery_requested']
        assert len(requests) == 1, requests
        incident = requests[0]['incident_id']
        for filename in ['initial.json', 'attempt-000001-before.json', 'attempt-000001-after.json', 'restored.json']:
            snapshot = root / 'logs/incidents' / incident / filename
            assert snapshot.stat().st_size <= 256 * 1024
            assert json.loads(snapshot.read_text())['incident_id'] == incident
        assert int((root / 'control/connect-count').read_text()) == 2
        print('PASS confirmed failure -> one recovery -> verified connectivity; durable before/after evidence.', flush=True)
        print(run('docker', 'stats', '--no-stream', '--format', '{{.MemUsage}} {{.CPUPerc}} {{.PIDs}}', name).stdout.strip(), flush=True)
        # Restart into the same synthetic failure. The prior cooldown must not
        # reset, and monitoring must continue while recovery is suppressed.
        (root / 'control/allow-heal').unlink()
        run('docker', 'restart', name)
        wait_for(lambda: status().get('state') == 'cooldown', 260)
        requests_after = [e for e in events() if e['event'] == 'recovery_requested']
        assert len(requests_after) == 1
        assert int((root / 'control/connect-count').read_text()) == 3  # one normal startup connect
        checks = [json.loads(line) for line in (root / 'logs/checks.jsonl').read_text().splitlines()]
        assert len(checks) >= 7
        assert status()['next_recovery_not_before']
        print('PASS restart retains cooldown; probes continue; no extra automatic recovery.', flush=True)
        # Prove that underlay failure changes the reason and suppresses recovery.
        run('docker', 'exec', name, 'nft', 'add', 'table', 'inet', 'test_underlay')
        run('docker', 'exec', name, 'nft', 'add', 'chain', 'inet', 'test_underlay', 'output', '{ type filter hook output priority -190; policy accept; }')
        run('docker', 'exec', name, 'nft', 'add', 'rule', 'inet', 'test_underlay', 'output', 'meta', 'skuid', '0', 'tcp', 'dport', '443', 'reject')
        wait_for(lambda: status().get('state') == 'underlay_down', 100)
        assert len([e for e in events() if e['event'] == 'recovery_requested']) == 1
        print('PASS underlay outage suppresses recovery and remains observable.', flush=True)
    finally:
        try:
            print(run('docker', 'logs', '--tail', '8', name, check=False).stdout.strip(), flush=True)
        finally:
            run('docker', 'rm', '-f', name, check=False)
