#!/usr/bin/env bash
# Generates the test tone that "@Bot test" plays: 10 s, 440 Hz, Ogg Opus,
# 48 kHz stereo, 20 ms frames (the only frame size the bot accepts).
# The tone is committed as assets/tone.opus; only rerun this to change it.
# Usage: scripts/make-tone.sh [output]   (default: assets/tone.opus)
set -euo pipefail
cd "$(dirname "$0")/.."

out="${1:-assets/tone.opus}"
command -v ffmpeg >/dev/null || { echo "ffmpeg not found on PATH" >&2; exit 1; }
mkdir -p "$(dirname "$out")"
ffmpeg -hide_banner -loglevel error -y -f lavfi -i 'sine=frequency=440:duration=10' \
    -ac 2 -ar 48000 -c:a libopus -b:a 96k -frame_duration 20 "$out"
echo "Wrote $out"
