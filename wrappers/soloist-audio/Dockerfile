ARG BASE_IMAGE=zombie-box-tv/gateway:0.1.0-dev.126-qa
FROM ${BASE_IMAGE}

USER 0:0
RUN apk add --no-cache \
    pulseaudio=17.0-r5 \
    pulseaudio-utils=17.0-r5

COPY entrypoint.sh /usr/local/bin/soloist-audio-qa
RUN chmod 0555 /usr/local/bin/soloist-audio-qa

USER 65532:65532
ENTRYPOINT ["/usr/local/bin/soloist-audio-qa"]
