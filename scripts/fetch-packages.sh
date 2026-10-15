#!/bin/sh
# Fixed upstream packages, verified before extraction. No package scripts run.
set -eu
scratch=$(mktemp -d)
trap 'rm -rf "$scratch"' EXIT
curl -fSL --retry 3 --max-time 180 \
 https://pkg.cloudflareclient.com/pool/bookworm/main/c/cloudflare-warp/cloudflare-warp_2026.8.2110.0_amd64.deb \
 -o "$scratch/warp.deb"
echo "65c1467499a42d91f8f9eb6b5071fd49b41f54f9012d3fc5f3d90a2361aa1604  $scratch/warp.deb" | sha256sum -c -
curl -fSL --retry 3 --max-time 180 \
 https://github.com/3proxy/3proxy/releases/download/1.0.1/3proxy-1.0.1.x86_64.deb \
 -o "$scratch/3proxy.deb"
echo "230e63119b0655b0738cd1fcb3cff5f87289d943efb3d2519705548e25c1e908  $scratch/3proxy.deb" | sha256sum -c -
dpkg-deb -x "$scratch/warp.deb" "$scratch/warp"
dpkg-deb -x "$scratch/3proxy.deb" "$scratch/proxy"
mkdir -p /out/usr/bin /out/usr/local/bin /out/usr/share/doc
install -m755 "$scratch/warp/bin/warp-svc" /out/usr/bin/warp-svc
install -m755 "$scratch/warp/bin/warp-cli" /out/usr/bin/warp-cli
install -m755 "$scratch/proxy/bin/3proxy" /out/usr/local/bin/3proxy
cp -a "$scratch/warp/usr/share/doc/cloudflare-warp" /out/usr/share/doc/
cp -a "$scratch/proxy/usr/share/doc/3proxy" /out/usr/share/doc/
