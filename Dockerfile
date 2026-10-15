# syntax=docker/dockerfile:1
# All stages use public images. No existing VPS image or registration is needed.
FROM golang:1.27.2-bookworm AS control
ENV CGO_ENABLED=0 GOMAXPROCS=2 GOMEMLIMIT=256MiB
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY config.example.json ./
RUN go test ./... && GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o /out/warpdeck ./cmd/warpdeck

FROM debian:bookworm-slim AS packages
RUN test "$(dpkg --print-architecture)" = amd64 \
 && apt-get update && apt-get install -y --no-install-recommends ca-certificates curl \
 && rm -rf /var/lib/apt/lists/*
COPY scripts/fetch-packages.sh /fetch-packages.sh
RUN sh /fetch-packages.sh

FROM debian:bookworm-slim AS runtime
# Only headless runtime dependencies; the WARP GUI and its WebKit stack are not installed.
RUN test "$(dpkg --print-architecture)" = amd64 \
 && apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates curl dbus iproute2 nftables tini busybox-static \
    libnss3 libpcap0.8 libtss2-esys-3.0.2-0 libtss2-tctildr0 libtss2-tcti-device0 \
 && rm -rf /var/lib/apt/lists/* /var/cache/apt/archives/* \
 && mkdir -p /usr/local/lib/warpdeck /var/lib/warpdeck /var/lib/cloudflare-warp \
 && ln -s /bin/busybox /usr/local/lib/warpdeck/busybox \
 && rm -f /etc/machine-id /var/lib/dbus/machine-id
COPY --from=packages /out/ /
COPY --from=control /out/warpdeck /usr/local/bin/warpdeck
COPY common.sh /usr/local/lib/warpdeck/common.sh
COPY --chmod=755 entrypoint.sh healthcheck.sh /usr/local/sbin/
COPY config.example.json /etc/warpdeck/default.json
ENV LANG=C MALLOC_ARENA_MAX=2 MALLOC_TRIM_THRESHOLD_=131072
RUN ldd /usr/bin/warp-svc /usr/bin/warp-cli /usr/local/bin/3proxy > /tmp/link-check \
 && ! grep 'not found' /tmp/link-check && rm /tmp/link-check \
 && warp-cli --version && warpdeck --help
HEALTHCHECK --interval=15s --timeout=5s --start-period=60s --retries=2 CMD ["/usr/local/sbin/healthcheck.sh"]
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/sbin/entrypoint.sh"]
