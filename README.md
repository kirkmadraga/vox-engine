<div align="center">

# vox-engine

**A vibe-coded multi-tool Discord bot you self-host on low-end hardware.**

[![CI](https://github.com/kirkmadraga/vox-engine/actions/workflows/ci.yml/badge.svg?branch=master)](https://github.com/kirkmadraga/vox-engine/actions/workflows/ci.yml) [![Latest version](https://img.shields.io/github/v/tag/kirkmadraga/vox-engine?sort=semver&label=version)](https://github.com/kirkmadraga/vox-engine/tags) [![Go](https://img.shields.io/github/go-mod/go-version/kirkmadraga/vox-engine)](go.mod) [![License: AGPL-3.0](https://img.shields.io/badge/license-AGPL--3.0-blue)](LICENSE)

</div>

## ✨ Features

- 🎵 **Music.** Play YouTube and Spotify links in voice, or search YouTube and
  pick a result. Queue, skip, stop.
- ⏰ **Reminders.** One-off or repeating, pinging you in the channel where you set
  them.
- 🤖 **Ask.** Chat with a language model (xAI's Grok or OpenAI). It remembers the
  channel's recent conversation, looks at images, and searches the web when
  asked.
- 🔐 **Owner-controlled access.** The bot's owner decides which servers and people
  can use what, with daily limits on `ask`.
- ⚡ **Slash commands and mentions.** `/play` and `@Bot play` work the same way.

## Commands

Use a slash command (`/play`) or mention the bot (`@Bot play`); both work the same.

| Command | What it does | Who |
|---|---|---|
| `ping` | Replies "@you pong" | Anyone, in allowed servers |
| `play <YouTube or Spotify link>` | Adds the track to this server's queue and joins your voice channel (Spotify: single tracks only, for now) | Granted users |
| `play <search words>` | Lists the top YouTube results | Granted users |
| `play <number>` | Queues that result from your last search (within 5 minutes) | Granted users |
| `queue` / `skip` / `stop` | List the queue, skip the current track, clear it and leave | Same as `play` |
| `test` | Plays a short test tone | Same as `play` |
| `remindme <when> <what>` | Pings you in this channel at that time, e.g. `in 2h`, `at 18:30`, `on friday`, `every day at 08:00`, `every 2h until 18:00` (`remindme` alone lists the formats). With `ask` and allowance left, the model words it | Granted users (its own grant) |
| `remindme list` / `cancel <number>` | Your reminders in this server; remove one | Same as `remindme` |
| `ask <prompt>`, `@Bot <prompt>`, or a reply to one of its answers | Asks a language model (experimental, off by default) | Granted users (its own grant) |
| `forget` | Clears this channel's `ask` conversation memory | Same as `ask` |
| `allow` / `deny guild [id]` | Allow or remove a server | Owners |
| `allow` / `deny @user [command]` | Grant or revoke a command for one person (default `play`) | Owners |
| `allow` / `deny everyone [command]` | Open a command to everyone in this server, or close it | Owners |
| `allow` / `deny public [command]` | Open a command to everyone in every server the bot is in, allowed or not, or close it | Owners |

Owners (from `config.yaml`) can use everything, everywhere. Everyone else gets a
command if it's public, or, in an allowed server, if it's open to everyone there
or granted to them. The bot stays silent to anyone not allowed. Taking
`remindme` away deletes the reminders that went with it.

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
   No privileged intents are needed (unless you opt into the one below). The bot registers its
   slash commands itself at startup. On the **Bot** page, also turn off **Public Bot**, so only
   you can add the bot to servers.
2. Copy `.env.example` to `.env` and set `DISCORD_TOKEN`.
3. Copy `config.example.yaml` to `config.yaml`, then:
   - put your Discord user ID in `owner_user_ids` (the bot refuses to start
     with the example IDs still there);
   - pick `ytdlp_js_runtime` (`deno` or `node`; `quickjs` on old CPUs).

   The other settings are documented in the file.
4. Run it from the folder holding `config.yaml` (relative paths resolve from
   there): `./vox-engine`, or `go run ./cmd/vox-engine`. Add `-debug` to log why messages
   are ignored. `./vox-engine -check-config` only checks `.env` and `config.yaml`
   and exits (status 1 on errors), e.g. before restarting after an edit.
5. In Discord: `@Bot allow guild`, then `@Bot allow @friend` for anyone who
   should be able to play music (or `@Bot allow everyone` for the whole server).

It writes `cache/` (audio) and `data/bot.db` (access lists, cache index), and
needs `assets/tone.opus` for `test`. How you keep it running (systemd, a
scheduled task, a container) is up to you.

### Optional: Message Content intent

Only `ask` uses it: it lets a question that replies to someone's message (e.g.
`@Bot explain this` on a meme) include that message's text or image. Music and
every other command work without it.

1. In the Discord developer portal: your application → **Bot** → **Privileged
   Gateway Intents** → turn on **Message Content Intent** and save.
2. Then set `discord_message_content: true` in `config.yaml` and restart.

Do them in that order: if the bot asks for the intent while the portal switch
is off, Discord refuses the connection and the bot won't start. Existing
invites keep working. With it on, the bot receives every message in channels it
can see, but acts on and remembers only those addressed to it, and logs none.
Bots in 100 or more servers need Discord's approval for it.

## License

Copyright (C) 2026 Kirk Madraga

This program is free software: you can redistribute it and/or modify it under
the terms of the GNU Affero General Public License as published by the Free
Software Foundation, version 3 of the License. See [LICENSE](LICENSE).

In short: you may use, study, modify and share it, but if you run a modified
version for users over a network (for example as a public bot), you must offer
those users the source of your version under the same license.
