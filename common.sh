#!/bin/bash
STATE_DIR=/var/lib/warpdeck
RUN_DIR=/run/warpdeck
BB=/usr/local/lib/warpdeck/busybox
valid_mode() { case "$1" in proxy|gateway|both) return 0;; *) return 1;; esac; }
read_value() { local value=''; [[ ! -r "$1" ]] || IFS= read -r value < "$1" || true; printf '%s' "$value"; }
atomic_write() {
  local target=$1 value=$2 tmp="$1.tmp.$BASHPID"
  printf '%s\n' "$value" > "$tmp"
  "$BB" mv -f "$tmp" "$target"
}
alive() { [[ "$1" =~ ^[0-9]+$ ]] && kill -0 "$1" 2>/dev/null; }
