FROM golang:1.25.6-alpine3.23@sha256:98e6cffc31ccc44c7c15d83df1d69891efee8115a5bb7ede2bf30a38af3e3c92 AS build
RUN apk add --no-cache gcc=15.2.0-r2 musl-dev=1.2.5-r23 alsa-lib-dev=1.2.14-r2 libogg-dev=1.3.6-r0 libvorbis-dev=1.3.7-r2 flac-dev=1.4.3-r2 mpg123-dev=1.33.3-r0
WORKDIR /upstream
COPY --from=sources /go-librespot/ ./
RUN test "$(cat ZOMBIE_UPSTREAM_COMMIT)" = 57d7278d94a9233060c2a6238f5926ffd1e72de4
RUN --mount=type=cache,id=zombie-go-mod,target=/go/pkg/mod,sharing=locked \
    --mount=type=cache,id=zombie-go-build,target=/root/.cache/go-build,sharing=locked \
    CGO_ENABLED=1 GOMAXPROCS=2 go build -p 2 -trimpath -ldflags="-s -w" -o /go-librespot ./cmd/daemon
WORKDIR /gateway
COPY gateway/ ./
RUN --mount=type=cache,id=zombie-go-build,target=/root/.cache/go-build,sharing=locked \
    CGO_ENABLED=0 GOMAXPROCS=2 go build -p 2 -trimpath -ldflags="-s -w" -o /zombie-worker ./cmd/zombie-worker

FROM alpine:3.23@sha256:85fe1e81d6758c208f3e1eed4338a1997e19d4be002d4dd32d3100c9a8c010a0
RUN apk add --no-cache ffmpeg=8.0.1-r1 alsa-lib=1.2.14-r2 libogg=1.3.6-r0 libvorbis=1.3.7-r2 flac=1.4.3-r2 mpg123=1.33.3-r0
COPY --from=build /go-librespot /zombie-worker /usr/local/bin/
COPY --from=build /upstream/LICENSE /usr/share/licenses/go-librespot/LICENSE
COPY --from=build /upstream/ZOMBIE_UPSTREAM_COMMIT /usr/share/licenses/go-librespot/COMMIT
USER 65532:65532
ENTRYPOINT ["zombie-worker"]
