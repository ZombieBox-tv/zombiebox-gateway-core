FROM golang:1.25.6-alpine3.23@sha256:98e6cffc31ccc44c7c15d83df1d69891efee8115a5bb7ede2bf30a38af3e3c92 AS build
WORKDIR /src
COPY --from=sources /threadfin/ ./
RUN test "$(cat ZOMBIE_UPSTREAM_COMMIT)" = 6b9c0ccf16164eb362af0a44660228267734c5aa
RUN --mount=type=cache,id=zombie-go-mod,target=/go/pkg/mod,sharing=locked \
    --mount=type=cache,id=zombie-go-build,target=/root/.cache/go-build,sharing=locked \
    CGO_ENABLED=0 GOMAXPROCS=2 go build -mod=readonly -p 2 -trimpath -ldflags="-s -w" -o /threadfin .
FROM alpine:3.23@sha256:85fe1e81d6758c208f3e1eed4338a1997e19d4be002d4dd32d3100c9a8c010a0
RUN apk add --no-cache ffmpeg=8.0.1-r1
COPY --from=build /threadfin /usr/local/bin/threadfin
COPY --from=build /src/LICENSE /usr/share/licenses/threadfin/LICENSE
COPY --from=build /src/ZOMBIE_UPSTREAM_COMMIT /usr/share/licenses/threadfin/COMMIT
USER 65532:65532
ENTRYPOINT ["threadfin", "-config", "/state", "-bind", "0.0.0.0", "-port", "34400"]
