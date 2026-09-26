FROM alpine:3.23@sha256:85fe1e81d6758c208f3e1eed4338a1997e19d4be002d4dd32d3100c9a8c010a0
RUN apk add --no-cache \
    python3=3.12.14-r0 \
    py3-pip=25.1.1-r1 \
    nodejs=24.18.1-r0 \
    ca-certificates-bundle=20260909-r0
COPY wrappers/youtube-pot/requirements.lock /tmp/requirements.lock
RUN python3 -m pip install --no-cache-dir --break-system-packages --no-deps \
    --requirement /tmp/requirements.lock \
    && python3 -m pip check \
    && rm /tmp/requirements.lock
WORKDIR /app
COPY --chown=1000:1000 --chmod=0644 wrappers/youtube-pot/*.py ./
COPY LICENSE NOTICE /usr/share/doc/zombiebox/
USER 1000:1000
ENV HOME=/tmp PORT=8097 ZOMBIE_YOUTUBE_POT_CONFIG=/config/pot.json
CMD ["python3", "-u", "server.py"]
