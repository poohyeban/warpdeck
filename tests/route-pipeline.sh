#!/bin/bash
# Exercise the real functions with fake producers; never access network state.
set -euo pipefail
source_file=${1:-$(dirname "$0")/../entrypoint.sh}
test_dir=$(mktemp -d)
trap 'rm -rf "$test_dir"' EXIT
mutation_log=$test_dir/mutations
touch "$mutation_log"

sed -n '/^ensure_routes() {/,/^}/p' "$source_file" > "$test_dir/functions.sh"
sed -n '/^configure_warp() {/,/^}/p' "$source_file" >> "$test_dir/functions.sh"
source "$test_dir/functions.sh"

# Match early, then exceed the pipe buffer so an early-exiting consumer reliably
# breaks its producer. Normal grep must consume all output before succeeding.
large_output() {
  printf '%s\n' "$1"
  printf '%262144s\n' 'remaining output'
}

address_present=yes
route_present=yes
ip() {
  local af=$1
  shift
  case "$*" in
    'addr show dev CloudflareWARP')
      [[ "$address_present" == yes ]] || return 1
      if [[ "$af" == -6 ]]; then
        large_output '    inet6 2001:db8::2/128 scope global'
      else
        large_output '    inet 172.16.0.2/32 scope global'
      fi
      ;;
    'route show table 1091')
      if [[ "$route_present" == yes ]]; then
        large_output 'default dev CloudflareWARP'
      else
        printf '%s\n' 'unreachable default metric 32760'
      fi
      ;;
    'route replace '*|'route del '*)
      printf '%s %s\n' "$af" "$*" >> "$mutation_log"
      ;;
    *) echo "Unexpected ip command: $af $*" >&2; return 1 ;;
  esac
}

EGRESS4=warp
EGRESS6=warp
ensure_routes
if [[ -s "$mutation_log" ]]; then
  echo 'FAIL: valid routes were changed after a producer was cut short' >&2
  cat "$mutation_log" >&2
  exit 1
fi
echo 'PASS: present addresses and routes survive large producer output'

route_present=no
ensure_routes
printf '%s\n' \
  '-4 route replace default dev CloudflareWARP table 1091' \
  '-4 route replace default dev CloudflareWARP table 1094' \
  '-6 route replace default dev CloudflareWARP table 1091' \
  '-6 route replace default dev CloudflareWARP table 1094' > "$test_dir/expected"
cmp "$test_dir/expected" "$mutation_log"
echo 'PASS: genuinely missing routes are restored'

: > "$mutation_log"
address_present=no
ensure_routes
printf '%s\n' \
  '-4 route del default dev CloudflareWARP table 1091' \
  '-4 route del default dev CloudflareWARP table 1094' \
  '-6 route del default dev CloudflareWARP table 1091' \
  '-6 route del default dev CloudflareWARP table 1094' > "$test_dir/expected"
cmp "$test_dir/expected" "$mutation_log"
echo 'PASS: genuinely missing tunnel addresses remove stale routes'

RUN_DIR=$test_dir
read_value() { printf '%s\n' registered; }
warp_command() {
  case "$*" in
    'tunnel ip list') large_output $'0.0.0.0/0\n::/0' ;;
    'tunnel ip add-range '*) printf '%s\n' "$*" >> "$mutation_log" ;;
    disconnect|'mode tunnel_only'|connect) return 0 ;;
    *) echo "Unexpected warp command: $*" >&2; return 1 ;;
  esac
}
: > "$mutation_log"
configure_warp
if [[ -s "$mutation_log" ]]; then
  echo 'FAIL: existing exclusions were misdetected as absent' >&2
  exit 1
fi
echo 'PASS: existing exclusions are not added again after large output'
