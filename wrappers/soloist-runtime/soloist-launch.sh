#!/bin/sh
# Executed only after the dedicated container's admission checks. Never trace.
set -eu
umask 077
[ "$(id -u)" = 65531 ] && [ "$(id -g)" = 65531 ] || exit 1
[ "$#" = 1 ] && [ "$1" = /run/secrets/soloist-api-key ] || exit 1
exec 3<"$1"
key=
IFS= read -r key <&3 || [ -n "$key" ]
extra=
if IFS= read -r extra <&3 || [ -n "$extra" ]; then exit 1; fi
exec 3<&-
# Reject empty/control/whitespace-bearing values without printing the value.
case "$key" in '' | *[!\!-\~]*) exit 1 ;; esac
ulimit -c 0
exec /runtime/soloist --api-key "$key" \
    --device-name 'Zombie Box Soloist' \
    --data-dir /state/session --cache-dir /tmp/playback-cache --cache-size 100 \
    --ws 127.0.0.1:8096 --pipewire-device zombiebox_soloist
