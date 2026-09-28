# Generates the test tone that "@Bot play" uses: 10 s, 440 Hz, Ogg Opus,
# 48 kHz stereo, 20 ms frames (the only frame size the bot accepts).
# Usage: powershell -ExecutionPolicy Bypass -File scripts\make-tone.ps1 [-Out cache\tone.opus]
# Linux equivalent:
#   ffmpeg -f lavfi -i sine=frequency=440:duration=10 -ac 2 -ar 48000 -c:a libopus -b:a 96k -frame_duration 20 cache/tone.opus
param([string]$Out = (Join-Path 'cache' 'tone.opus'))
$ErrorActionPreference = 'Stop'
Set-Location (Split-Path -Parent $PSScriptRoot)

New-Item -ItemType Directory -Force (Split-Path -Parent $Out) | Out-Null
& ffmpeg -hide_banner -loglevel error -y -f lavfi -i 'sine=frequency=440:duration=10' `
    -ac 2 -ar 48000 -c:a libopus -b:a 96k -frame_duration 20 $Out
if ($LASTEXITCODE -ne 0) { throw "ffmpeg failed (exit $LASTEXITCODE)" }
Write-Host "Wrote $Out"
