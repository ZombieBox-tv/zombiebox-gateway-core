FROM golang:1.25.6-alpine3.23@sha256:98e6cffc31ccc44c7c15d83df1d69891efee8115a5bb7ede2bf30a38af3e3c92 AS worker
WORKDIR /src
COPY gateway/ ./
RUN --mount=type=cache,id=zombie-go-build,target=/root/.cache/go-build,sharing=locked \
    CGO_ENABLED=0 GOMAXPROCS=2 go build -p 2 -trimpath -ldflags="-s -w" -o /zombie-worker ./cmd/zombie-worker
FROM alpine:3.23@sha256:85fe1e81d6758c208f3e1eed4338a1997e19d4be002d4dd32d3100c9a8c010a0 AS build
RUN apk add --no-cache gcc=15.2.0-r2 g++=15.2.0-r2 musl-dev=1.2.5-r23 make=4.4.1-r3 cmake=4.1.3-r0 openssl-dev=3.5.8-r0 libplist-dev=2.7.0-r0 gstreamer-dev=1.26.9-r0 gst-plugins-base-dev=1.26.9-r0
WORKDIR /src
COPY --from=sources /uxplay/ ./
RUN test "$(cat ZOMBIE_UPSTREAM_COMMIT)" = 57ea83411d5f7e0b38c5841987439340543f025c
RUN cmake -S . -B build -DNO_X11_DEPS=ON -DUSE_MDNS=ON -DCMAKE_BUILD_TYPE=Release && cmake --build build -j2
FROM alpine:3.23@sha256:85fe1e81d6758c208f3e1eed4338a1997e19d4be002d4dd32d3100c9a8c010a0
RUN apk add --no-cache ffmpeg=8.0.1-r1 libplist=2.7.0-r0 gst-plugins-good=1.26.9-r0 gst-plugins-bad=1.26.9-r0 gst-libav=1.26.9-r0
COPY --from=build /src/build/uxplay /usr/local/bin/uxplay
COPY --from=worker /zombie-worker /usr/local/bin/zombie-worker
COPY --from=build /src/LICENSE /usr/share/licenses/uxplay/LICENSE
COPY --from=build /src/ZOMBIE_UPSTREAM_COMMIT /usr/share/licenses/uxplay/COMMIT
ENV HOME=/state GST_REGISTRY=/state/gstreamer-registry.bin
USER 65532:65532
ENTRYPOINT ["zombie-worker"]
