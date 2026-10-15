#!/bin/bash
set -Eeuo pipefail
umask 077
source /usr/local/lib/warpdeck/common.sh
mkdir -p "$STATE_DIR" "$RUN_DIR" /var/lib/cloudflare-warp /run/dbus /var/lib/dbus /tmp
chmod 1777 /tmp
CONFIG=$STATE_DIR/config.json
if [[ ! -f "$CONFIG" ]]; then
  "$BB" cp /etc/warpdeck/default.json "$CONFIG"
fi
# Restart from the last explicitly applied snapshot. Pending edits never activate
# merely because the container restarts. First boot bootstraps from config.json.
STARTUP_CONFIG=$STATE_DIR/applied.json
[[ -f "$STARTUP_CONFIG" ]] || STARTUP_CONFIG=$CONFIG
warpdeck internal prepare "$STARTUP_CONFIG" "$RUN_DIR/candidate"
rm -f "$RUN_DIR/applied-mode" "$RUN_DIR/applied-revision" "$RUN_DIR/proxy.pid" "$RUN_DIR/events" "$STATE_DIR/applied-mode"
rm -f "$RUN_DIR/config-error" "$RUN_DIR/failed-revision"
rm -rf "$RUN_DIR/active"
"$BB" mkfifo -m 600 "$RUN_DIR/events"
exec 9<>"$RUN_DIR/events"
atomic_write "$RUN_DIR/supervisor.pid" "$BASHPID"
atomic_write "$RUN_DIR/registration-state" "starting"
proxy_pid= dbus_pid= warp_pid= applied= active_iface= active_proxy_revision= active_egress_revision=
cleanup() {
  trap - EXIT TERM INT
  rm -f "$RUN_DIR/applied-mode" "$RUN_DIR/applied-revision" "$RUN_DIR/supervisor.pid" "$STATE_DIR/applied-mode"
  for pid in "$proxy_pid" "$warp_pid" "$dbus_pid"; do
    [[ -z "$pid" ]] || kill "$pid" 2>/dev/null || true
  done
  for ((i=0; i<20; i++)); do
    remaining=false
    for pid in "$proxy_pid" "$warp_pid" "$dbus_pid"; do alive "$pid" && remaining=true || true; done
    [[ "$remaining" == true ]] || break
    sleep 0.1
  done
  for pid in "$proxy_pid" "$warp_pid" "$dbus_pid"; do
    [[ -z "$pid" ]] || kill -KILL "$pid" 2>/dev/null || true
  done
  wait || true
}
trap cleanup EXIT
trap 'exit 0' TERM INT
dbus-uuidgen --ensure
# All policy tables and sysctls belong to the container network namespace.
# Capture the underlay before warp-svc can install its own tunnel routes.
for af in 4 6; do
  ip -"$af" route flush table 1092 2>/dev/null || true
  ip -"$af" route replace unreachable default metric 32760 table 1092
  ip -"$af" route replace unreachable default table 1093
  while read -r route; do
    [[ -z "$route" ]] || ip -"$af" route replace table 1092 $route
  done < <(ip -"$af" route show table main | sed 's/ linkdown//g; s/ expires [^ ]*//g')
  ip -"$af" route replace unreachable default metric 32760 table 1091
 done
nft add table inet warpdeck_gateway
freeze_traffic() {
  for af in 4 6; do
    while ip -"$af" rule del pref 90 2>/dev/null; do :; done
    while ip -"$af" rule del pref 91 2>/dev/null; do :; done
    ip -"$af" rule add pref 90 uidrange 65534-65534 lookup 1093 || return 1
    # Freeze forwarded traffic from every interface during policy changes.
    ip -"$af" rule add pref 91 not iif lo lookup 1093 || return 1
  done
}
thaw_traffic() {
  for af in 4 6; do
    while ip -"$af" rule del pref 90 2>/dev/null; do :; done
    while ip -"$af" rule del pref 91 2>/dev/null; do :; done
  done
}
setup_routes() {
  for af in 4 6; do
    ip -"$af" route flush table 1091 2>/dev/null || true
    ip -"$af" route flush table 1094 2>/dev/null || true
    ip -"$af" route replace unreachable default metric 32760 table 1091 || return 1
    # Reply packets are marked by the container OUTPUT route chain. A broad
    # source-address rule would also bypass the chosen egress on new sockets.
    while ip -"$af" rule del pref 97 2>/dev/null; do :; done
    ip -"$af" rule add pref 97 fwmark 0x10920000 lookup 1092 || return 1
    while read -r route; do
      case " $route " in
        *" dev $INTERFACE "*)
          ip -"$af" route replace table 1091 $route || return 1
          ip -"$af" route replace table 1094 $route || return 1;;
      esac
    done < <(ip -"$af" route show table 1092 | grep -v '^unreachable' | grep -v '^default')
    local policy=$EGRESS4 table=1091
    [[ "$af" != 6 ]] || policy=$EGRESS6
    [[ "$policy" != native ]] || table=1092
    while ip -"$af" rule del pref 98 2>/dev/null; do :; done
    while ip -"$af" rule del pref 99 2>/dev/null; do :; done
    while ip -"$af" rule del pref 100 2>/dev/null; do :; done
    ip -"$af" rule add pref 98 fwmark 0x10910000 lookup "$table" || return 1
    # Initial socket lookup selects the tunnel source address. This table has
    # no unreachable default: replies can reach OUTPUT when the tunnel is down.
    # OUTPUT then marks requests into strict table 1091 and replies into 1092.
    if [[ "$policy" == warp ]]; then ip -"$af" rule add pref 99 uidrange 65534-65534 lookup 1094 || return 1; fi
    if [[ "$MODE" != proxy ]]; then ip -"$af" rule add pref 100 iif "$INTERFACE" lookup "$table" || return 1; fi
  done
  active_iface=$INTERFACE
}
ensure_routes() {
  for af in 4 6; do
    local policy=$EGRESS4
    [[ "$af" != 6 ]] || policy=$EGRESS6
    if [[ "$policy" == warp ]] && ip -"$af" addr show dev CloudflareWARP 2>/dev/null | grep -q 'inet'; then
      if ! ip -"$af" route show table 1091 | grep -q 'default dev CloudflareWARP'; then
        ip -"$af" route replace default dev CloudflareWARP table 1091 || true
        ip -"$af" route replace default dev CloudflareWARP table 1094 || true
      fi
    else
      ip -"$af" route del default dev CloudflareWARP table 1091 2>/dev/null || true
      ip -"$af" route del default dev CloudflareWARP table 1094 2>/dev/null || true
    fi
  done
}
warp_command() { timeout 12 warp-cli --accept-tos "$@"; }
configure_warp() {
  if [[ "$(read_value "$RUN_DIR/registration-state")" != registered ]]; then
    [[ "$EGRESS4" == native && "$EGRESS6" == native ]] || return 1
    return 0
  fi
  warp_command disconnect || return 1
  warp_command mode tunnel_only || return 1
  # WarpDeck owns per-family routing. Keep the official client's automatic
  # routing/firewall exclusions broad, so it cannot override our marks, block
  # public SOCKS clients, or silently choose an egress before our rules run.
  for range in 0.0.0.0/0 ::/0; do
    if ! warp_command tunnel ip list | awk '{for(i=1;i<=NF;i++)print $i}' | grep -Fxq "$range"; then
      warp_command tunnel ip add-range "$range" || return 1
    fi
  done
  if [[ "$EGRESS4" == warp || "$EGRESS6" == warp ]]; then
    # Applying configuration is not an Internet connectivity test. The user's
    # selected policy stays active even while the transport is unavailable.
    warp_command connect || echo 'WarpDeck: connect request failed; selected egress remains configured. Check status.' >&2
    return 0
  fi
}
stop_proxy() {
  if [[ -n "$proxy_pid" ]]; then
    kill "$proxy_pid" 2>/dev/null || true
    for ((i=0; i<20; i++)); do alive "$proxy_pid" || break; sleep 0.1; done
    kill -KILL "$proxy_pid" 2>/dev/null || true
    wait "$proxy_pid" 2>/dev/null || true
    proxy_pid=
    rm -f "$RUN_DIR/proxy.pid"
  fi
}
start_proxy() {
  /usr/local/bin/3proxy "$RUN_DIR/proxy.cfg" 8>&- 9>&- &
  proxy_pid=$!
  atomic_write "$RUN_DIR/proxy.pid" "$proxy_pid"
  sleep 0.2
  for ((i=0; i<30; i++)); do
    alive "$proxy_pid" || return 1
    if warpdeck internal probe "$1" 2>/dev/null; then return 0; fi
    sleep 0.1
  done
  return 1
}
apply_prepared() {
  local dir=$1
  source "$dir/settings.env" || return 1
  nft -c -f "$dir/gateway.nft" || return 1
  if [[ "$active_egress_revision" != "$EGRESS_REVISION" ]]; then
    freeze_traffic || return 1
    stop_proxy
    configure_warp || return 1
  fi
  setup_routes || return 1
  ensure_routes
  nft -f "$dir/gateway.nft" || return 1
  if [[ "$MODE" == gateway ]]; then
    stop_proxy
  elif [[ -z "$proxy_pid" || "$active_proxy_revision" != "$PROXY_REVISION" ]]; then
    stop_proxy
    "$BB" cp "$dir/3proxy.cfg" "$RUN_DIR/proxy.cfg"
    start_proxy "$dir/config.json" || return 1
  fi
  thaw_traffic
  applied=$MODE
  active_proxy_revision=$PROXY_REVISION
  active_egress_revision=$EGRESS_REVISION
}
reconcile() {
  local input=${1:-$RUN_DIR/request.json}
  if ! warpdeck internal prepare "$input" "$RUN_DIR/candidate" 2>"$RUN_DIR/prepare-error"; then
    atomic_write "$RUN_DIR/config-error" "$(cat "$RUN_DIR/prepare-error")"
    return 0
  fi
  source "$RUN_DIR/candidate/settings.env"
  local requested_revision=$REVISION
  if [[ "$requested_revision" == "$(read_value "$RUN_DIR/applied-revision")" ]]; then
    rm -f "$RUN_DIR/config-error" "$RUN_DIR/failed-revision"
    return 0
  fi
  if apply_prepared "$RUN_DIR/candidate" 2>"$RUN_DIR/apply-error"; then
    mkdir -p "$RUN_DIR/active"
    for file in config.json 3proxy.cfg gateway.nft settings.env; do
      "$BB" cp "$RUN_DIR/candidate/$file" "$RUN_DIR/active/$file"
    done
    atomic_write "$STATE_DIR/applied.json" "$(cat "$RUN_DIR/active/config.json")"
    atomic_write "$STATE_DIR/applied-mode" "$applied"
    atomic_write "$RUN_DIR/applied-mode" "$applied"
    atomic_write "$RUN_DIR/applied-revision" "$requested_revision"
    rm -f "$RUN_DIR/config-error" "$RUN_DIR/failed-revision"
    echo "WarpDeck applied configuration: $requested_revision mode=$applied"
  else
    local failure="$(cat "$RUN_DIR/apply-error")"
    # Roll back only the running services. The requested JSON remains visible,
    # so status can report precisely which configuration failed to apply.
    if [[ ! -f "$RUN_DIR/active/config.json" ]]; then echo "$failure" >&2; exit 1; fi
    stop_proxy
    active_proxy_revision=
    active_egress_revision=
    apply_prepared "$RUN_DIR/active" || exit 1
    atomic_write "$RUN_DIR/config-error" "Apply failed; previous runtime restored. $failure"
    atomic_write "$RUN_DIR/failed-revision" "$requested_revision"
  fi
}
dbus-daemon --system --nofork --nopidfile 9>&- &
dbus_pid=$!
# Official daemon output can contain account details. Keep routine Docker logs
# to supervisor/CLI messages; status remains available through warpdeck/warp-cli.
warp-svc 9>&- >/dev/null 2>&1 &
warp_pid=$!
ready=false
for ((i=0; i<60; i++)); do
  if warp-cli --accept-tos status >/dev/null 2>&1; then ready=true; break; fi
  alive "$warp_pid" || exit 1
  sleep 1
done
[[ "$ready" == true ]] || exit 1
atomic_write "$RUN_DIR/registration-state" "waiting"
if warp-cli --accept-tos registration show >/dev/null 2>&1; then
  atomic_write "$RUN_DIR/registration-state" "registered"
fi
source "$RUN_DIR/candidate/settings.env"
if [[ "$(read_value "$RUN_DIR/registration-state")" == registered || ( "$EGRESS4" == native && "$EGRESS6" == native ) ]]; then
  reconcile "$STARTUP_CONFIG"
else
  echo 'WarpDeck: registration required. Run: docker compose exec warp warpdeck register --accept-tos'
fi
while :; do
  alive "$dbus_pid" && alive "$warp_pid" || exit 1
  [[ -z "$proxy_pid" ]] || alive "$proxy_pid" || exit 1
  [[ -z "$applied" ]] || ensure_routes
  if IFS= read -r -t 5 request <&9; then
    case "$request" in
      reload) reconcile;;
      registered)
        if warp-cli --accept-tos registration show >/dev/null 2>&1; then
          atomic_write "$RUN_DIR/registration-state" "registered"
          [[ -n "$applied" ]] || reconcile "$STARTUP_CONFIG"
        fi
        ;;
    esac
  fi
done
