#!/usr/bin/env python3
"""Real dual-stack/IPv4-only/IPv6-only tests in disposable network namespaces.
Requires a Linux host with working native IPv4 AND IPv6, Docker, Python3,
nsenter, and root. Host routes and existing containers are never modified.
"""
import argparse, itertools, json, pathlib, subprocess, time, uuid
p=argparse.ArgumentParser()
p.add_argument('--image',default='warpdeck:local')
p.add_argument('--accept-tos',action='store_true',required=True)
p.add_argument('--scenario',choices=['all','dual','ipv4','ipv6'],default='all')
a=p.parse_args()
name='warpdeck-test-'+uuid.uuid4().hex[:8]
server,client=name+'-server',name+'-client'
network=name
registration,state=name+'-registration',name+'-state'
probe=str(pathlib.Path(__file__).with_name('udp_probe.py').resolve())
server4,client4='172.28.82.2','172.28.82.3'
server6,client6='fd80:82::2','fd80:82::3'

def run(args,ok=True,timeout=180):
 r=subprocess.run(args,text=True,capture_output=True,timeout=timeout)
 if ok and r.returncode: raise RuntimeError(f'command failed: {args[:5]} {r.stderr[:350]}')
 return r

def cli(*args,ok=True):return run(['docker','exec',server,'warpdeck',*args],ok)
def status():return json.loads(cli('status','--json').stdout)
def wait(fn,timeout=180):
 deadline=time.monotonic()+timeout
 while time.monotonic()<deadline:
  try:
   if fn():return
  except (RuntimeError,ValueError,KeyError):pass
  time.sleep(2)
 raise RuntimeError('expected state timed out')
def health():
 # Docker's cached healthy flag can still describe the previous policy just
 # after reload. Check the currently applied policy directly.
 return run(['docker','exec',server,'/usr/local/sbin/healthcheck.sh'],ok=False).returncode==0
def curl(proxy,family,works=True,policy='warp'):
 target='https://1.1.1.1/cdn-cgi/trace' if family==4 else 'https://[2606:4700:4700::1111]/cdn-cgi/trace'
 args=['docker','exec',client,'curl','-g','-fsS','--connect-timeout','3','--max-time','8']
 if proxy:args+=['--socks5-hostname',server4+':1080' if proxy==4 else '['+server6+']:1080']
 r=run(args+[target],ok=False)
 if not works:
  assert r.returncode!=0,f'unexpected connectivity / native fallback proxy={proxy} family={family}'
  return
 assert r.returncode==0, f'HTTPS failed proxy={proxy} family={family}: {r.stderr[:180]}'
 fields=dict(x.split('=',1) for x in r.stdout.splitlines() if '=' in x)
 assert fields.get('warp')==('on' if policy=='warp' else 'off'), 'wrong egress policy'
def udp(proxy,family,works=True):
 pid=run(['docker','inspect','-f','{{.State.Pid}}',client]).stdout.strip()
 endpoint='-' if not proxy else (server4 if proxy==4 else server6)+':1080'
 r=run(['nsenter','-t',pid,'-n','python3',probe,endpoint,str(family),'10' if works else '3'],ok=False)
 assert (r.returncode==0)==works,f'UDP unexpected result proxy={proxy} family={family} works={works}: {r.stderr[-250:]}'
def gateway(on):
 for family,via in [(4,server4 if on else '172.28.82.1'),(6,server6 if on else 'fd80:82::1')]:
  run(['docker','exec',client,'ip','-'+str(family),'route','replace','default','via',via])
def apply(v4,v6,mode='both'):
 cli('config','set','egress',json.dumps({'ipv4':v4,'ipv6':v6}))
 cli('mode',mode);cli('reload')
 wait(lambda:status()['config_in_sync'] and (health() if 'warp' in [v4,v6] else True))
 assert status()['egress']=={'ipv4':v4,'ipv6':v6}
def identity():
 # Keep registration fields in memory only; do not print/log them.
 return run(['docker','exec',server,'cat','/var/lib/cloudflare-warp/reg.json']).stdout

def create_server(scenario):
 wrapper=''
 if scenario=='ipv4':
  wrapper="ip -6 route del default; nft 'add table inet test_underlay'; nft 'add chain inet test_underlay out { type filter hook output priority -200; policy accept; }'; nft 'add rule inet test_underlay out oifname eth0 ip6 daddr != { fd80:82::/64, fe80::/10 } counter drop'; "
 elif scenario=='ipv6':
  wrapper="ip -4 route del default; nft 'add table inet test_underlay'; nft 'add chain inet test_underlay out { type filter hook output priority -200; policy accept; }'; nft 'add rule inet test_underlay out oifname eth0 ip daddr != 172.28.82.0/24 counter drop'; "
 # Resolver reachability is part of the test topology, not runtime auto-detection.
 dns=['2606:4700:4700::1111','2606:4700:4700::1001'] if scenario=='ipv6' else ['1.1.1.1','1.0.0.1']
 args=['docker','run','-d','--name',server,'--dns',dns[0],'--dns',dns[1],'--network',network,'--ip',server4,'--ip6',server6,'--mac-address','02:42:ac:1c:52:02',
  '--memory','384m','--memory-swap','512m','--cpus','1','--pids-limit','128','--cap-add','NET_ADMIN',
  '--security-opt','no-new-privileges:true','--device','/dev/net/tun',
  '--sysctl','net.ipv4.ip_forward=1','--sysctl','net.ipv6.conf.all.forwarding=1','--sysctl','net.ipv6.bindv6only=0',
  '--sysctl','net.ipv4.conf.all.rp_filter=0','--sysctl','net.ipv4.conf.default.rp_filter=0',
  '--mount',f'source={registration},target=/var/lib/cloudflare-warp','--mount',f'source={state},target=/var/lib/warpdeck',
  '--log-opt','max-size=2m','--log-opt','max-file=2','--entrypoint','/bin/sh',a.image,'-ec',wrapper+'exec /usr/bin/tini -- /usr/local/sbin/entrypoint.sh']
 run(args)
 # Recreated servers reuse an IP but receive a new MAC. Clear only the test
 # client's neighbor cache so an old IPv6 neighbor cannot mask connectivity.
 for family in [4,6]:run(['docker','exec',client,'ip','-'+str(family),'neigh','flush','dev','eth0'])

try:
 run(['docker','network','create','--ipv6','--subnet','172.28.82.0/24','--subnet','fd80:82::/64',network])
 run(['docker','run','-d','--name',client,'--network',network,'--ip',client4,'--ip6',client6,'--memory','64m','--pids-limit','24','--cap-add','NET_ADMIN','--entrypoint','/bin/sleep',a.image,'infinity'])
 # This is a real native dual-stack baseline, not a WARP-backed substitute.
 for family in [4,6]:curl(0,family,policy='native');udp(0,family)
 print('PASS test host native IPv4/IPv6 HTTPS and UDP',flush=True)
 saved_identity=None
 scenarios=['dual','ipv4','ipv6'] if a.scenario=='all' else [a.scenario]
 for scenario in scenarios:
  print('SCENARIO',scenario,flush=True)
  create_server(scenario)
  if saved_identity is None:
   wait(lambda:status().get('registration')=='waiting')
   assert cli('register',ok=False).returncode!=0
   assert cli('reload',ok=False).returncode!=0
   # Native-only does not need an account or a connected WARP tunnel.
   apply('native','native','proxy')
   for family in [4,6]:
    works=scenario=='dual' or scenario=='ipv'+str(family)
    curl(4,family,works,'native');udp(4,family,works)
   assert status()['registration']=='waiting'
   print('PASS unregistered native-only configuration',flush=True)
   cli('register','--accept-tos');wait(lambda:status()['registration']=='registered')
   # Registering alone must not activate an unrelated pending config.
   apply('warp','warp','proxy')
   saved_identity=identity();cli('register','--accept-tos');assert identity()==saved_identity
  else:wait(lambda:status()['config_in_sync'] and health())
  assert status()['mode']=='proxy'
  for ingress,family in itertools.product([4,6],[4,6]):curl(ingress,family);udp(ingress,family)
  print('PASS default proxy, allow-all sources, IPv4/IPv6 clients and destinations',flush=True)
  gateway(True);curl(0,4,works=False);curl(0,6,works=False);gateway(False)
  cli('config','set','socks.allowed_sources','[]');time.sleep(6)
  assert not status()['config_in_sync'];curl(4,4)
  cli('reload');curl(4,4,works=False);curl(6,6,works=False)
  cli('config','set','socks.allowed_sources','["0.0.0.0/0","::/0"]');cli('reload')
  for ingress,source,other in [(4,client4+'/32',6),(6,client6+'/128',4)]:
   cli('config','set','socks.allowed_sources',json.dumps([source]));cli('reload')
   curl(ingress,4);curl(other,4,works=False)
  cli('config','set','socks.allowed_sources','["0.0.0.0/0","::/0"]');cli('reload')
  print('PASS manual reload, /0 ranges and independent IPv4/IPv6 source ACLs',flush=True)
  for v4,v6 in itertools.product(['warp','native'],repeat=2):
   print('CHECK policy',scenario,v4,v6,flush=True)
   apply(v4,v6)
   for proxy in [4,0]:
    gateway(not proxy)
    for family,policy in [(4,v4),(6,v6)]:
     works=policy=='warp' or scenario=='dual' or scenario=='ipv'+str(family)
     curl(proxy,family,works,policy);udp(proxy,family,works)
   gateway(False)
   print('PASS policy matrix',scenario,v4,v6,'TCP/UDP proxy/gateway; unavailable native rejected',flush=True)
  apply('warp','warp')
  cli('config','set','gateway.allowed_sources','[]');cli('reload');gateway(True)
  curl(0,4,works=False);curl(0,6,works=False)
  cli('config','set','gateway.allowed_sources','["0.0.0.0/0","::/0"]');cli('reload')
  curl(0,4);curl(0,6);gateway(False)
  cli('mode','gateway');cli('reload');curl(4,4,works=False);gateway(True);curl(0,4);curl(0,6);gateway(False)
  cli('mode','both');cli('reload')
  cli('config','set','socks.listen','"203.0.113.123"');assert cli('reload',ok=False).returncode!=0
  curl(4,4);assert not status()['config_in_sync']
  run(['docker','restart',server])
  for family in [4,6]:run(['docker','exec',client,'ip','-'+str(family),'neigh','flush','dev','eth0'])
  wait(lambda:health() and status()['mode']=='both')
  assert not status()['config_in_sync'];curl(4,4);curl(6,6)
  cli('config','set','socks.listen','"::"');cli('reload')
  assert identity()==saved_identity
  print('PASS modes, failed apply, restart and identity persistence',flush=True)
  run(['docker','exec',server,'warp-cli','--accept-tos','disconnect'])
  wait(lambda:status()['tunnel']=='disconnected')
  cli('config','set','gateway.tcp_mss','1200');cli('reload')
  assert status()['config_in_sync'] and status()['tunnel']=='disconnected'
  cli('config','set','gateway.tcp_mss','1220');cli('reload')
  print('PASS reload does not require external connectivity',flush=True)
  for proxy in [4,0]:
   gateway(not proxy)
   for family in [4,6]:curl(proxy,family,works=False)
  gateway(False)
  run(['docker','exec',server,'warp-cli','--accept-tos','connect'])
  wait(lambda:health() and status()['tunnel']=='connected')
  for proxy in [4,0]:
   gateway(not proxy)
   for family in [4,6]:curl(proxy,family);udp(proxy,family)
  gateway(False)
  print('PASS disconnect blocks fallback; reconnect restores TCP/UDP',flush=True)
  cli('mode','proxy');cli('reload')
  if scenario==scenarios[-1]:run(['docker','exec',server,'warp-cli','--accept-tos','registration','delete'])
  run(['docker','rm','-f',server])
 print('ALL DUAL-STACK MATRIX TESTS PASSED',flush=True)
except Exception:
 print('TEST CLIENT NEIGHBORS',run(['docker','exec',client,'ip','neigh'],ok=False).stdout,flush=True)
 print('TEST SERVER MAC',run(['docker','inspect','-f','{{range .NetworkSettings.Networks}}{{.MacAddress}}{{end}}',server],ok=False).stdout,flush=True)
 print('SUPERVISOR',run(['docker','logs','--tail','20',server],ok=False).stdout,flush=True)
 for command in [['warpdeck','status','--json'],['ip','-4','rule'],['ip','-6','rule'],['ip','-4','route','show','table','1091'],['ip','-6','route','show','table','1091'],['nft','list','table','inet','warpdeck_gateway']]:
  print('DIAGNOSTIC',command,run(['docker','exec',server,*command],ok=False).stdout,flush=True)
 raise
finally:
 run(['docker','exec',server,'warp-cli','--accept-tos','registration','delete'],ok=False)
 run(['docker','rm','-f',server,client],ok=False)
 run(['docker','volume','rm',registration,state],ok=False)
 run(['docker','network','rm',network],ok=False)
