#!/bin/bash
set -euo pipefail
source /usr/local/lib/warpdeck/common.sh
source "$RUN_DIR/active/settings.env"
valid_mode "$MODE"
[[ "$REVISION" == "$(read_value "$RUN_DIR/applied-revision")" ]]
alive "$(read_value "$RUN_DIR/supervisor.pid")"
if [[ "$EGRESS4" == warp || "$EGRESS6" == warp ]]; then
  warp-cli --accept-tos status | grep 'Status update: Connected' >/dev/null
fi
for af in 4 6; do
  policy=$EGRESS4
  [[ "$af" != 6 ]] || policy=$EGRESS6
  table=1091
  if [[ "$policy" == warp ]]; then
    ip -"$af" route show table 1091 | grep CloudflareWARP >/dev/null
  else
    table=1092
  fi
  ip -"$af" rule show | grep "98:.*fwmark 0x10910000 lookup $table" >/dev/null
  if [[ "$MODE" != proxy ]]; then
    ip -"$af" rule show | grep "100:.*iif $INTERFACE lookup $table" >/dev/null
  fi
done
nft list table inet warpdeck_gateway >/dev/null
if [[ "$MODE" != gateway ]]; then
  alive "$(read_value "$RUN_DIR/proxy.pid")"
  warpdeck internal probe "$RUN_DIR/active/config.json"
fi
echo "mode=$MODE ipv4=$EGRESS4 ipv6=$EGRESS6"
