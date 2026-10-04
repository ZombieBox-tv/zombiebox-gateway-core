FROM golang:1.25.6-alpine3.23@sha256:98e6cffc31ccc44c7c15d83df1d69891efee8115a5bb7ede2bf30a38af3e3c92 AS build
WORKDIR /gateway
COPY gateway/ ./
RUN --mount=type=cache,id=zombie-go-build,target=/root/.cache/go-build,sharing=locked \
    CGO_ENABLED=0 GOMAXPROCS=2 go build -p 2 -trimpath -ldflags="-s -w" -o /zombie-soloist ./cmd/zombie-soloist

# Full's initial runtime targets the amd64 evaluation host. Manifest from
# docker-debian-artifacts 8f962b15d7884a90e17876a9303cbac909d119aa.
FROM --platform=linux/amd64 debian:trixie-20260918-slim@sha256:7792b1f7702a86946cd518db72b6a407302c3e9bc1635634368b878189e8221c
# Freeze transitive libraries as well as direct packages.
RUN rm -f /etc/apt/sources.list.d/* && \
    printf '%s\n' 'deb [check-valid-until=no] http://snapshot.debian.org/archive/debian/20260918T000000Z/ trixie main' > /etc/apt/sources.list && \
    apt-get update && apt-get install -y --no-install-recommends \
      pipewire-bin=1.4.2-1 wireplumber=0.5.8-2 dbus-daemon=1.16.2-2 \
      libpipewire-0.3-0t64=1.4.2-1 dbus-x11=1.16.2-2 libpulse0 libasound2t64 libatomic1 ca-certificates && \
    mkdir -p /usr/share/doc/zombiebox && \
    dpkg-query -W > /usr/share/doc/zombiebox/runtime-packages.txt && \
    rm -rf /var/lib/apt/lists/*
COPY --from=build /zombie-soloist /usr/local/bin/zombie-soloist
COPY wrappers/soloist-runtime/soloist-launch.sh /usr/local/libexec/soloist-launch
COPY wrappers/soloist-runtime/soloist-pipewire.conf /usr/local/share/zombiebox/soloist-pipewire.conf
COPY wrappers/soloist-runtime/wireplumber.conf /etc/wireplumber/wireplumber.conf.d/90-zombiebox.conf
COPY LICENSE NOTICE /usr/share/doc/zombiebox/
RUN chmod 0555 /usr/local/libexec/soloist-launch && \
    groupadd --gid 65531 zombie-soloist && \
    useradd --uid 65531 --gid 65531 --no-create-home --home-dir /tmp --shell /usr/sbin/nologin zombie-soloist && \
    mkdir -p /state /runtime /config /run/secrets && chown 65531:65531 /state
ENV XDG_RUNTIME_DIR=/tmp/runtime PIPEWIRE_REMOTE=zombie-soloist \
    XDG_CONFIG_HOME=/tmp/config XDG_CACHE_HOME=/tmp/cache HOME=/tmp \
    LD_LIBRARY_PATH=/runtime/lib
USER 65531:65531
ENTRYPOINT ["/usr/local/bin/zombie-soloist"]
