# vox-engine

A small, self-hosted Discord music bot in Go. It plays YouTube and Spotify links
in voice channels, with end-to-end voice encryption (DAVE), and is built to run on weak
hardware: one pure-Go binary, no cgo, Opus passed straight through with no
re-encoding.

## Commands

Use a slash command (`/play`, `/queue`, …) or mention the bot, then the command: `@Bot play <link>`,
or `@Bot play <search words>` to list the top YouTube results and then `@Bot play <number>` to pick
one. Both work the same way and follow the same access rules; `/allow` and `/deny` take a `guild` or
`user` subcommand. `/play` also has an optional `lucky` switch: with search words, it plays the first
result that isn't a cover, karaoke or live version (unless you asked for one) instead of listing them. Someone without access who uses a slash command gets a short reply only they can
see (`unauthorized_message` in `config.yaml`); a mention from them gets no reply.

| Command | What it does | Who |
|---|---|---|
| `ping` | Replies "@you pong" | Anyone, in allowed servers |
| `play <YouTube link>` | Adds the track to this server's queue and joins your voice channel | Granted users |
| `play <Spotify track link>` | Same, for a single Spotify track (albums and playlists aren't supported yet) | Granted users |
| `play <search words>` | Lists the top YouTube results (10 by default, `search_results` in `config.yaml`) | Granted users |
| `play <number>` | Queues that result from your last search (within 5 minutes) | Granted users |
| `queue` / `skip` / `stop` | List the queue, skip the current track, clear it and leave | Same as `play` |
| `test` | Plays a short test tone | Same as `play` |
| `ask <prompt>`, `@Bot <prompt>`, or a reply to one of its answers | Asks a language model (experimental, off by default; see below) | Granted users (its own grant), in channels an owner enabled |
| `forget` | Clears this channel's `ask` conversation memory | Same as `ask` |
| `allow` / `deny guild [id]` | Allow or remove a server | Owners |
| `allow` / `deny @user [command]` | Grant or revoke a command (default `play`) | Owners |
| `allow` / `deny ask [#channel or ID]` | Enable or disable `ask` in a channel (default: this one; threads follow their channel) | Owners |

Owners (from `config.yaml`) can use everything, everywhere. The bot stays silent
to anyone not allowed.

About `ask` (settings and comments in `config.example.yaml`):

- It uses xAI (Grok) or OpenAI through their Responses API, with the key in
  `.env` as `LLM_API_KEY`. Questions, the channel's recent `ask` conversation
  and any image are sent to that provider.
- It remembers each channel's recent conversation (in memory only), and a
  reply to one of its answers (with the reply's @ on) continues it.
- It's limited per person (a cooldown and a daily allowance), in question
  length, and in how many questions run at once.
- Web search is `off`, `on-request` (start the question with `search`, or
  `/ask search:True`) or `always`. Searches cost several times a plain answer.
- With `discord_message_content` on, a question that replies to someone's
  message (text or image) includes that message. The bot then receives every
  message in channels it can see, but acts on and remembers only those
  addressed to it.

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
  them, or `quickjs` on older CPUs. Set it as `ytdlp_js_runtime`. On slow CPUs,
  yt-dlp's challenge solving can take about a minute per new video (queued
  tracks download in the background, so mostly only the first song after an
  idle period waits).

### Build

```bash
go build -o vox-engine ./cmd/vox-engine
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "-s -w" -o vox-engine ./cmd/vox-engine   # cross-compile for Linux
```

`scripts/check.sh` (bash; on Windows run it from MSYS2) runs vet, the
race-enabled tests (needs gcc on PATH, else without `-race`), and both Windows
and Linux builds into `dist/`. `scripts/make-tone.sh` regenerates the test tone.

### Configure and run

1. Create an application in the [Discord developer portal](https://discord.com/developers/applications),
   copy the bot token from its **Bot** page, and invite the bot with permissions
   `3148800` (View Channel, Send Messages, Connect, Speak):
   `https://discord.com/oauth2/authorize?client_id=<APPLICATION_ID>&scope=bot+applications.commands&permissions=3148800`.
   No privileged intents are needed. The bot registers its slash commands itself at startup.
2. Copy `.env.example` to `.env` and set `DISCORD_TOKEN`.
3. Copy `config.example.yaml` to `config.yaml`, then:
   - put your Discord user ID in `owner_user_ids` (in Discord: Settings →
     Advanced → Developer Mode on, then right-click your name → Copy User ID;
     the bot refuses to start with the example IDs still there);
   - pick `ytdlp_js_runtime` (`deno` or `node`; `quickjs` on old CPUs).

   The other settings are documented in the file.
4. Run it from the folder holding `config.yaml` (relative paths resolve from
   there): `./vox-engine`, or `go run ./cmd/vox-engine`. Add `-debug` to log why messages
   are ignored. `./vox-engine -check-config` only checks `.env` and `config.yaml`
   and exits (status 1 on errors), e.g. before restarting after an edit.
5. In Discord: `@Bot allow guild`, then `@Bot allow @friend` for anyone who
   should be able to play music.

It writes `cache/` (audio) and `data/bot.db` (access lists, cache index), and
needs `assets/tone.opus` for `test`. How you keep it running (systemd, a
scheduled task, a container) is up to you.

## License

Copyright (C) 2026 Kirk Madraga

This program is free software: you can redistribute it and/or modify it under
the terms of the GNU Affero General Public License as published by the Free
Software Foundation, version 3 of the License. See [LICENSE](LICENSE).

In short: you may use, study, modify and share it, but if you run a modified
version for users over a network (for example as a public bot), you must offer
those users the source of your version under the same license.
