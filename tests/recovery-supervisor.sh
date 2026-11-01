#!/bin/bash
# Exercise recovery dispatch without touching a network or a real WARP service.
set -euo pipefail
test_dir=$(mktemp -d)
trap 'rm -rf "$test_dir"' EXIT
source_file=${1:-$(dirname "$0")/../entrypoint.sh}
sed -n '/^recover_service() {/,/^}/p' "$source_file" > "$test_dir/function.sh"
source "$test_dir/function.sh"
RUN_DIR=$test_dir
revision=$(printf '%064d' 1)
incident=20261101T120000Z
printf '%s\n' "$revision" > "$RUN_DIR/applied-revision"
read_value(){ cat "$1"; }
atomic_write(){ printf '%s\n' "$2" > "$1"; }
sleep(){ :; }
warp_command(){ printf '%s\n' "$*" >> "$test_dir/commands"; [[ "$*" != disconnect || ${disconnect_fails:-no} != yes ]]; }
ensure_routes(){ printf 'routes\n' >> "$test_dir/commands"; }
stop_proxy(){ printf 'stop_proxy\n' >> "$test_dir/commands"; }
start_proxy(){ printf 'start_proxy %s\n' "$*" >> "$test_dir/commands"; }
recover_service tunnel "$incident" "$(printf '%064d' 2)"
test ! -e "$test_dir/commands"
test "$(cat "$RUN_DIR/recovery-result")" = "$incident tunnel skipped 0 0"
recover_service tunnel "$incident" "$revision"
test "$(cat "$test_dir/commands")" = $'disconnect\nconnect\nroutes'
test "$(cat "$RUN_DIR/recovery-result")" = "$incident tunnel success 0 0"
disconnect_fails=yes
recover_service tunnel "$incident" "$revision"
test "$(cat "$RUN_DIR/recovery-result")" = "$incident tunnel failed 1 0"
: > "$test_dir/commands"
recover_service proxy "$incident" "$revision"
test "$(cat "$test_dir/commands")" = "$(printf 'stop_proxy\nstart_proxy %s/active/config.json' "$RUN_DIR")"
test "$(cat "$RUN_DIR/recovery-result")" = "$incident proxy success 0 0"
echo 'PASS recovery is revision-guarded, records command failures, and separates proxy/tunnel actions'
