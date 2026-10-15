#!/usr/bin/env python3
"""Isolated integration test. Needs Linux root, Docker, Python 3 and nsenter.
Creates its own containers/volumes; never changes host routes or existing containers.
Cloudflare terms must be accepted explicitly with --accept-tos.
"""
import argparse, json, pathlib, subprocess, time, uuid
p = argparse.ArgumentParser()
p.add_argument('--image', default='warpdeck:local')
p.add_argument('--network', help='Use an existing network without changing/removing it')
p.add_argument('--server-ip', default='172.28.81.2')
p.add_argument('--client-ip', default='172.28.81.3')
p.add_argument('--server-ipv6')
p.add_argument('--client-ipv6')
p.add_argument('--ipv6', action='store_true', help='Require working native IPv6')
p.add_argument('--accept-tos', action='store_true', required=True)
a = p.parse_args()
name = 'warpdeck-test-' + uuid.uuid4().hex[:8]
server, client = name + '-server', name + '-client'
network = a.network or name
registration, state = name + '-registration', name + '-state'
probe = str(pathlib.Path(__file__).with_name('udp_probe.py').resolve())

def run(args, ok=True, timeout=180):
    r = subprocess.run(args, text=True, capture_output=True, timeout=timeout)
    if ok and r.returncode:
        # Do not dump daemon logs or registration data in failures.
        raise RuntimeError(f'Command failed ({r.returncode}): {args[:4]} {r.stderr[:250]}')
    return r

def cli(*args, ok=True):
    return run(['docker', 'exec', server, 'warpdeck', *args], ok)

def status():
    return json.loads(cli('status', '--json').stdout)

def wait(fn, timeout=150):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        try:
            if fn():
                return
        except (RuntimeError, ValueError):
            pass
        time.sleep(2)
    raise RuntimeError('Timed out waiting for expected state')

def trace(proxy=False, family=4, ok=True):
    args = ['docker', 'exec', client, 'curl', '-fsS', '--max-time', '12']
    if proxy:
        args += ['--socks5-hostname', a.server_ip + ':1080']
    addr = '1.1.1.1' if family == 4 else '[2606:4700:4700::1111]'
    r = run(args + ['https://' + addr + '/cdn-cgi/trace'], ok=False)
    if not ok:
        assert r.returncode != 0, 'unexpectedly allowed'
        return
    assert r.returncode == 0, 'trace connection failed'
    fields = dict(x.split('=', 1) for x in r.stdout.splitlines() if '=' in x)
    assert fields.get('warp') == ('on' if family == 4 else 'off'), fields.get('warp')
    print('TRACE PASS', 'SOCKS' if proxy else 'gateway', family, flush=True)

def udp(proxy=False, family=4):
    pid = run(['docker', 'inspect', '-f', '{{.State.Pid}}', client]).stdout.strip()
    r = run(['nsenter', '-t', pid, '-n', 'python3', probe, a.server_ip + ':1080' if proxy else '-', str(family)])
    print(r.stdout.strip(), flush=True)

def create_server():
    args = ['docker', 'run', '-d', '--name', server, '--network', network, '--ip', a.server_ip,
            '--memory', '256m', '--memory-swap', '384m', '--cpus', '0.75', '--pids-limit', '96',
            '--security-opt', 'no-new-privileges:true', '--cap-add', 'NET_ADMIN', '--device', '/dev/net/tun',
            '--sysctl', 'net.ipv4.ip_forward=1', '--sysctl', 'net.ipv4.conf.all.rp_filter=0',
            '--sysctl', 'net.ipv4.conf.default.rp_filter=0', '--restart', 'unless-stopped',
            '--mount', f'source={registration},target=/var/lib/cloudflare-warp',
            '--mount', f'source={state},target=/var/lib/warpdeck', '--log-opt', 'max-size=2m', '--log-opt', 'max-file=2']
    if a.server_ipv6:
        args += ['--ip6', a.server_ipv6]
    run(args + [a.image])

try:
    if not a.network:
        args = ['docker', 'network', 'create', '--subnet', '172.28.81.0/24']
        if a.ipv6:
            args += ['--ipv6', '--subnet', 'fd80:81::/64']
            a.server_ipv6 = a.server_ipv6 or 'fd80:81::2'
            a.client_ipv6 = a.client_ipv6 or 'fd80:81::3'
        run(args + [network])
    create_server()
    wait(lambda: status().get('registration') == 'waiting')
    assert cli('register', ok=False).returncode != 0
    assert cli('reload', ok=False).returncode != 0
    cli('register', '--accept-tos')
    wait(lambda: status()['tunnel'] == 'connected' and status()['config_in_sync'])
    print('PASS empty-volume explicit registration', flush=True)
    # Compare internally, never print the account record or its checksum.
    identity = run(['docker', 'exec', server, 'warp-cli', '--accept-tos', 'registration', 'show']).stdout
    cli('register', '--accept-tos')
    assert identity == run(['docker', 'exec', server, 'warp-cli', '--accept-tos', 'registration', 'show']).stdout
    print('PASS repeated registration preserves identity', flush=True)
    args = ['docker', 'run', '-d', '--name', client, '--network', network, '--ip', a.client_ip,
            '--memory', '64m', '--pids-limit', '24', '--cap-add', 'NET_ADMIN', '--entrypoint', '/bin/sleep']
    if a.client_ipv6:
        args += ['--ip6', a.client_ipv6]
    run(args + [a.image, 'infinity'])
    trace(proxy=True, ok=False)
    for field in ['socks.allowed_sources', 'gateway.allowed_sources']:
        cli('config', 'set', field, json.dumps([a.client_ip + '/32']))
    time.sleep(7)
    assert not status()['config_in_sync']
    trace(proxy=True, ok=False)
    cli('reload')
    trace(proxy=True); udp(proxy=True)
    if a.ipv6:
        trace(proxy=True, family=6); udp(proxy=True, family=6)
    print('PASS save-only and explicit reload ACL', flush=True)
    # Route only the disposable client's IPv4 namespace through the test server.
    run(['docker', 'exec', client, 'ip', '-4', 'route', 'replace', 'default', 'via', a.server_ip])
    trace(); udp()
    if a.ipv6:
        trace(family=6); udp(family=6)
    cli('mode', 'proxy')
    run(['docker', 'restart', server])
    wait(lambda: status()['tunnel'] == 'connected' and status()['mode'] == 'both')
    assert not status()['config_in_sync']
    assert identity == run(['docker', 'exec', server, 'warp-cli', '--accept-tos', 'registration', 'show']).stdout
    run(['docker', 'rm', '-f', server]); create_server()
    wait(lambda: status()['tunnel'] == 'connected' and status()['mode'] == 'both')
    assert not status()['config_in_sync']
    print('PASS restart/recreate keep identity and last applied config', flush=True)
    cli('reload'); assert status()['mode'] == 'proxy'
    trace(ok=False); trace(proxy=True); udp(proxy=True)
    cli('mode', 'gateway'); cli('reload')
    trace(); trace(proxy=True, ok=False)
    cli('mode', 'both'); cli('reload')
    trace(); trace(proxy=True); udp(); udp(proxy=True)
    if a.ipv6:
        trace(family=6); trace(proxy=True, family=6)
    print('ALL ISOLATED INTEGRATION TESTS PASSED', flush=True)
finally:
    # Deregister only this test's identity; no existing registration is ever mounted.
    run(['docker', 'exec', server, 'warp-cli', '--accept-tos', 'registration', 'delete'], ok=False)
    run(['docker', 'rm', '-f', server, client], ok=False)
    run(['docker', 'volume', 'rm', registration, state], ok=False)
    if not a.network:
        run(['docker', 'network', 'rm', network], ok=False)
