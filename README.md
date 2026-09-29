# vox-engine

A small, self-hosted Discord music bot in Go. It plays YouTube audio in voice
channels, with end-to-end voice encryption (DAVE), and is built to run on weak
hardware: one pure-Go binary, no cgo, Opus passed straight through with no
re-encoding.

## Commands

Mention the bot, then the command: `@Bot play <link>`.

| Command | What it does | Who |
|---|---|---|
| `ping` | Replies "@you pong" | Anyone, in allowed servers |
| `play <YouTube link>` | Adds the track to this server's queue and joins your voice channel | Granted users |
| `queue` / `skip` / `stop` | List the queue, skip the current track, clear it and leave | Same as `play` |
| `test` | Plays a short test tone | Same as `play` |
| `allow` / `deny guild [id]` | Allow or remove a server | Owners |
| `allow` / `deny @user [command]` | Grant or revoke a command (default `play`) | Owners |
| `access` | Show allowed servers and grants | Owners |

Owners (from `config.yaml`) can use everything, everywhere. The bot stays silent
to anyone not allowed.

## How it works

- Queue per server (limit configurable), one server in voice at a time. The bot
  leaves when the queue ends, and drops the queue if someone disconnects it.
- Audio is fetched once with yt-dlp, checked, and cached as Ogg Opus. The cache is
  capped by size and by time since last played.
- Access lists and the cache index live in one SQLite file.

## Running it

### Dependencies

To build:

- **Go 1.26+.** No C toolchain: the build is pure Go (`CGO_ENABLED=0`), and the
  binary runs on any x86-64 CPU.

On the machine that runs the bot (found on `PATH`, or set in `config.yaml`):

- **yt-dlp**, kept up to date (YouTube breaks old versions). Install it with
  pip/pipx as `yt-dlp[default]` so its YouTube challenge scripts are included.
- **ffmpeg**, which yt-dlp uses to repackage the audio (no re-encoding).
- **A JavaScript runtime for yt-dlp:** `deno` or `node` where the CPU supports
  them, or `quickjs` on older CPUs. Set it as `ytdlp_js_runtime`.

### Build

```bash
go build -o bot ./cmd/bot
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "-s -w" -o bot ./cmd/bot   # cross-compile for Linux
```

`scripts/check.sh` (bash; on Windows run it from MSYS2) runs vet, the
race-enabled tests (needs gcc on PATH, else without `-race`), and both Windows
and Linux builds into `dist/`. `scripts/make-tone.sh` regenerates the test tone.

### Configure and run

1. Create a bot in the Discord developer portal and invite it with permissions
   `3148800` (View Channel, Send Messages, Connect, Speak). No privileged
   intents are needed.
2. Copy `.env.example` to `.env` and set `DISCORD_TOKEN`.
3. Copy `config.example.yaml` to `config.yaml`, then set your user ID in
   `owner_user_ids` and pick `ytdlp_js_runtime`. The other settings are
   documented in the file.
4. Run it from the folder holding `config.yaml` (relative paths resolve from
   there): `./bot`, or `go run ./cmd/bot`. Add `-debug` to log why messages
   are ignored.
5. In Discord: `@Bot allow guild`, then `@Bot allow @friend` for anyone who
   should be able to play music.

It writes `cache/` (audio) and `data/bot.db` (access lists, cache index), and
needs `assets/tone.opus` for `test`. How you keep it running (systemd, a
scheduled task, a container) is up to you.

**Hosting note:** YouTube blocks many datacenter IPs ("Sign in to confirm you're
not a bot"). A home connection just works. On a server, point `ytdlp_cookies` at
a cookies.txt exported from a spare Google account (the file must be writable:
yt-dlp saves refreshed cookies back). On slow CPUs, yt-dlp's JavaScript challenge
solving can take about a minute per new video.
