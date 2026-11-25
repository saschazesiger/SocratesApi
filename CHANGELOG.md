# Changelog

## Unreleased

- 🌐 **Remote access.** *Settings* → *Remote access* sets up a Cloudflare tunnel from an API token: the tunnel, a DNS
  record, Cloudflare Access with a one-time PIN by e-mail for the addresses you list, `cloudflared` (downloaded into
  `/data/bin` when missing, checksummed) and a check from outside. No open port; the local address and the password
  keep working. Through the tunnel every path needs the Access login, the model API included.
- 🏗️ **On the Z-Dash server.** The listener, the sign-in, the Host and CSRF guards, the Content-Security-Policy, the
  live change notices and Z-Dash itself now come from the Go module `github.com/saschazesiger/Z-Dash/server`;
  Socrates keeps its API, its database and its interface. Nothing to do on an update: the admin password and the
  sessions stay (same keys, same format, same `socrates_session` cookie), and `SOCRATES_PORT`, `SOCRATES_HOST`,
  `SOCRATES_HOSTS`, `SOCRATES_ADMIN_PASSWORD` and `SOCRATES_DATA` work as before. New: `SOCRATES_LISTEN`,
  `SOCRATES_SECRET_KEY` (default `/data/secret.key`, encrypts the Cloudflare token), `SOCRATES_TUNNEL_LISTEN`,
  `SOCRATES_CLOUDFLARED`. An admin password in `SOCRATES_ADMIN_PASSWORD` that is too short now stops the start
  instead of being ignored.
- ⚙️ **Settings page** in the drawer: remote access, the admin password (moved from the API page) and the version.
  The sign-in and first-password cards are the server's shell.
- 🔌 Internal API: the first password is `POST /api/setup/password`, the CSRF header `X-Z-Dash: 1`, the live status
  and engine log `GET /api/stream`; `GET /api/events` carries only the change notices
  ([internal API](docs/internal-api.md)). The nightly workflow *Update Z-Dash server* takes the module's `main`,
  tests it and starts the release.

## 0.1.0 · 2026-11-12

First release.

- 🤗 Models from Hugging Face: search, quantisations with size and fit for this machine, recommended choice,
  parallel resumable downloads with SHA-256, vision projectors, split files.
- 🛠️ Conversion of safetensors repositories to GGUF with llama.cpp's converter and quantiser.
- 🧠 Placement planned by llama.cpp's fit (context, layers, MoE experts in RAM), a preview that also works while
  another model runs, live bars for GPU, RAM and NVMe measured from the running engine.
- 🔥 One active model kept loaded: crash restarts, read-in of RAM weights, requests wait while it loads.
- 💬 Chat with thinking, Markdown and a live status line (queue, prompt progress, tok/s), engine log.
- 🔌 OpenAI- and Anthropic-compatible API with API keys, usage per key and a request log; tested with opencode.
- 🎨 The look of Socrates: its logo and icons, Z-Dash's own colours, Source Code Pro for code; installable as an app.
- 🐳 One image on llama.cpp b12316 (CUDA 12.8).
