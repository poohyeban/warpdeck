#!/bin/sh
# Latest stable WARP from signed APT metadata; pinned 3proxy with SHA-256.
# Only extract the packages. No package scripts run.
set -eu
scratch=$(mktemp -d)
trap 'rm -rf "$scratch"' EXIT
# APT verifies the repository signature and package hash before extraction.
curl -fSL --retry 3 --max-time 180 \
 https://pkg.cloudflareclient.com/pubkey.gpg -o "$scratch/cloudflare-key.gpg"
gpg --batch --yes --dearmor -o /usr/share/keyrings/cloudflare-warp.gpg "$scratch/cloudflare-key.gpg"
echo 'deb [signed-by=/usr/share/keyrings/cloudflare-warp.gpg] https://pkg.cloudflareclient.com/ bookworm main' \
 > /etc/apt/sources.list.d/cloudflare-warp.list
apt-get update --error-on=any
(cd "$scratch" && apt-get download cloudflare-warp)
set -- "$scratch"/cloudflare-warp_*.deb
test "$#" -eq 1 && test -f "$1"
test "$(dpkg-deb --field "$1" Package)" = cloudflare-warp
test "$(dpkg-deb --field "$1" Architecture)" = amd64
printf 'Selected WARP version: %s\n' "$(dpkg-deb --field "$1" Version)"
mv "$1" "$scratch/warp.deb"
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
