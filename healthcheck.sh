#!/bin/bash
set -euo pipefail
source /usr/local/lib/warpdeck/common.sh
source "$RUN_DIR/active/settings.env"
valid_mode "$MODE"
[[ "$REVISION" == "$(read_value "$RUN_DIR/applied-revision")" ]]
alive "$(read_value "$RUN_DIR/supervisor.pid")"
warp-cli --accept-tos status | grep 'Status update: Connected' >/dev/null
ip -4 route show table 1091 | grep CloudflareWARP >/dev/null
nft list table ip warpdeck_gateway >/dev/null
if [[ $MODE != proxy ]]; then
  ip -4 rule show | grep "100:.*iif $INTERFACE lookup 1091" >/dev/null
fi
if [[ $MODE != gateway ]]; then
  alive "$(read_value "$RUN_DIR/proxy.pid")"
  # A local connect verifies the listener, with a bounded Docker health timeout.
  warpdeck internal probe "$RUN_DIR/active/config.json"
fi
echo "mode=$MODE"
