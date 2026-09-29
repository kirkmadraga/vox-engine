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

Needs Go 1.26+ to build, and on the machine running it: `yt-dlp`, `ffmpeg`, and a
JavaScript runtime for yt-dlp (`node`, `deno`, or `quickjs`).

1. Create a bot in the Discord developer portal and invite it with permissions
   `3148800` (View Channel, Send Messages, Connect, Speak). No privileged
   intents are needed.
2. `cp .env.example .env` and set `DISCORD_TOKEN`.
3. `cp config.example.yaml config.yaml` and set your user ID in `owner_user_ids`
   and `ytdlp_js_runtime`.
4. `go run ./cmd/bot` (add `-debug` to log why messages are ignored).
5. In Discord: `@Bot allow guild`, then `@Bot allow @friend` for anyone who
   should be able to play music.

Checks and builds (vet, race tests, Windows and Linux binaries):
`powershell -File scripts\check.ps1`. Linux server setup is in
[deploy/SETUP.md](deploy/SETUP.md).

**Hosting note:** YouTube blocks many datacenter IPs ("Sign in to confirm you're
not a bot"). A home connection just works; on a VPS, set `ytdlp_cookies` to a
cookies.txt from a spare Google account (see [deploy/SETUP.md](deploy/SETUP.md)).
On slow CPUs, yt-dlp's JavaScript challenge solving can take about a minute per
new video.
