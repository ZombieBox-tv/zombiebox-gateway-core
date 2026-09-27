#!/bin/sh
set -eu

umask 077
export XDG_CONFIG_HOME=/tmp/config
export XDG_RUNTIME_DIR=/tmp/runtime
export PULSE_SERVER=unix:/tmp/pulse/native

pulse_pid=
sender_pid=
capture_pid=

stop_child() {
    child_pid=$1
    [ -n "$child_pid" ] || return 0
    kill -TERM "$child_pid" 2>/dev/null || true
    sleep 0.1
    kill -KILL "$child_pid" 2>/dev/null || true
    wait "$child_pid" 2>/dev/null || true
}

cleanup() {
    exit_status=$?
    trap - EXIT HUP INT TERM
    stop_child "$capture_pid"
    stop_child "$sender_pid"
    stop_child "$pulse_pid"
    exit "$exit_status"
}

trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

if [ -e /dev/snd ]; then
    echo "FAIL: synthetic audio QA container unexpectedly has /dev/snd" >&2
    exit 1
fi

mkdir -m 0700 -p "$XDG_CONFIG_HOME" "$XDG_RUNTIME_DIR" /tmp/pulse

pulseaudio \
    -n \
    --no-cpu-limit \
    --daemonize=no \
    --exit-idle-time=-1 \
    --log-level=warning \
    --log-target=stderr \
    --load="module-native-protocol-unix socket=/tmp/pulse/native auth-anonymous=yes auth-cookie=no" \
    --load="module-null-sink sink_name=soloist_qa_sink format=s16le channels=2 rate=44100 sink_properties=device.description=SoloistQASink" \
    >/tmp/pulse-server.log 2>&1 &
pulse_pid=$!

ready=0
attempt=0
while [ "$attempt" -lt 50 ]; do
    if pactl info >/dev/null 2>&1; then
        ready=1
        break
    fi
    if ! kill -0 "$pulse_pid" 2>/dev/null; then
        echo "FAIL: private PulseAudio server exited before readiness" >&2
        exit 1
    fi
    attempt=$((attempt + 1))
    sleep 0.1
done
[ "$ready" -eq 1 ] || {
    echo "FAIL: private PulseAudio server did not become ready within 5 seconds" >&2
    exit 1
}

sink_list=$(pactl list short sinks)
source_list=$(pactl list short sources)
sink_count=$(printf '%s\n' "$sink_list" | awk 'NF { count++ } END { print count + 0 }')
source_count=$(printf '%s\n' "$source_list" | awk 'NF { count++ } END { print count + 0 }')
[ "$sink_count" -eq 1 ] || {
    echo "FAIL: expected exactly one isolated PulseAudio sink" >&2
    exit 1
}
[ "$source_count" -eq 1 ] || {
    echo "FAIL: expected only the isolated sink monitor source" >&2
    exit 1
}
printf '%s\n' "$sink_list" | awk '$2 == "soloist_qa_sink" { found=1 } END { exit !found }' || {
    echo "FAIL: the isolated PulseAudio sink name did not match" >&2
    exit 1
}
printf '%s\n' "$source_list" | awk '$2 == "soloist_qa_sink.monitor" { found=1 } END { exit !found }' || {
    echo "FAIL: the only PulseAudio source is not the dedicated monitor" >&2
    exit 1
}
pactl list sinks | awk '
    /^[[:space:]]*Name: soloist_qa_sink$/ { in_sink=1 }
    in_sink && /Sample Specification: s16le 2ch 44100Hz/ { found=1 }
    in_sink && /^$/ { in_sink=0 }
    END { exit !found }
' || {
    echo "FAIL: the dedicated sink is not s16le, stereo, 44100Hz" >&2
    exit 1
}

if ! ffmpeg -hide_banner -h demuxer=pulse 2>&1 | grep -q 'Pulse audio input'; then
    echo "FAIL: Full FFmpeg does not expose PulseAudio input" >&2
    exit 1
fi
if ! ffmpeg -hide_banner -h muxer=pulse 2>&1 | grep -q 'Pulse audio output'; then
    echo "FAIL: Full FFmpeg does not expose PulseAudio output" >&2
    exit 1
fi

echo "PASS: private PulseAudio has one null sink and only its monitor source"
echo "PASS: no host sound device is available and Full FFmpeg has PulseAudio input/output"

ffmpeg \
    -nostdin \
    -hide_banner \
    -loglevel error \
    -filter_threads 1 \
    -threads 1 \
    -re \
    -f lavfi \
    -i 'sine=frequency=440:sample_rate=44100:duration=6' \
    -ar 44100 \
    -ac 2 \
    -c:a pcm_s16le \
    -f pulse \
    -device soloist_qa_sink \
    soloist_qa_synthetic \
    >/tmp/tone-sender.log 2>&1 &
sender_pid=$!

sender_ready=0
attempt=0
while [ "$attempt" -lt 50 ]; do
    if pactl list short sink-inputs | awk 'NF { found=1 } END { exit !found }'; then
        sender_ready=1
        break
    fi
    if ! kill -0 "$sender_pid" 2>/dev/null; then
        echo "FAIL: synthetic tone sender exited before connecting to the dedicated sink" >&2
        exit 1
    fi
    attempt=$((attempt + 1))
    sleep 0.1
done
[ "$sender_ready" -eq 1 ] || {
    echo "FAIL: synthetic tone sender did not connect within 5 seconds" >&2
    exit 1
}

ffmpeg \
    -nostdin \
    -hide_banner \
    -loglevel error \
    -filter_threads 1 \
    -f pulse \
    -sample_rate 44100 \
    -channels 2 \
    -i soloist_qa_sink.monitor \
    -t 4 \
    -c:a pcm_s16le \
    -threads 1 \
    -f wav \
    /tmp/capture.wav \
    >/tmp/tone-capture.log 2>&1 &
capture_pid=$!
if ! wait "$capture_pid"; then
    capture_pid=
    echo "FAIL: bounded FFmpeg monitor capture failed" >&2
    exit 1
fi
capture_pid=

capture_size=$(wc -c </tmp/capture.wav)
[ "$capture_size" -gt 44 ] && [ "$capture_size" -le 1048576 ] || {
    echo "FAIL: PCM capture size was outside the bounded range" >&2
    exit 1
}

if ! wait "$sender_pid"; then
    sender_pid=
    echo "FAIL: synthetic tone sender exited unsuccessfully" >&2
    exit 1
fi
sender_pid=

echo "PASS: captured only the synthetic 440Hz tone from soloist_qa_sink.monitor as bounded PCM WAV"
touch /tmp/capture-ready

attempt=0
while [ ! -e /tmp/release ] && [ "$attempt" -lt 100 ]; do
    attempt=$((attempt + 1))
    sleep 0.1
done
[ -e /tmp/release ] || {
    echo "FAIL: QA runner did not release the captured PCM within 10 seconds" >&2
    exit 1
}
