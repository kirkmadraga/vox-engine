# Generates the test tone that "@Bot test" plays: 10 s, 440 Hz, Ogg Opus,
# 48 kHz stereo, 20 ms frames (the only frame size the bot accepts).
# The tone is committed as assets/tone.opus; only rerun this to change it.
# Usage: powershell -ExecutionPolicy Bypass -File scripts\make-tone.ps1 [-Out assets\tone.opus]
# Linux equivalent:
#   ffmpeg -f lavfi -i sine=frequency=440:duration=10 -ac 2 -ar 48000 -c:a libopus -b:a 96k -frame_duration 20 assets/tone.opus
param([string]$Out = (Join-Path 'assets' 'tone.opus'))
$ErrorActionPreference = 'Stop'
Set-Location (Split-Path -Parent $PSScriptRoot)

New-Item -ItemType Directory -Force (Split-Path -Parent $Out) | Out-Null
& ffmpeg -hide_banner -loglevel error -y -f lavfi -i 'sine=frequency=440:duration=10' `
    -ac 2 -ar 48000 -c:a libopus -b:a 96k -frame_duration 20 $Out
if ($LASTEXITCODE -ne 0) { throw "ffmpeg failed (exit $LASTEXITCODE)" }
Write-Host "Wrote $Out"
