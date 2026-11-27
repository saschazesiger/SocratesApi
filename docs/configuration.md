# 🔧 Configuration

## 🌱 Environment variables

| Variable | Default | |
|---|---|---|
| `SOCRATES_PORT` | `8080` | port of the interface and the API |
| `SOCRATES_HOST` | all addresses | address to listen on |
| `SOCRATES_LISTEN` | | `host:port` to listen on; replaces the two above |
| `SOCRATES_DATA` | `/data` | database, models, tools, the secret key, the tunnel's socket and `cloudflared` |
| `SOCRATES_ADMIN_PASSWORD` | | sets the admin password at start and signs everybody out; remove it afterwards |
| `SOCRATES_HOSTS` | | extra host names the interface answers to, comma-separated (`*` for any). IP addresses, single names and `.lan`, `.local`, `.internal`, `.home`, `.ts.net` … always work |
| `SOCRATES_SECRET_KEY` | `/data/secret.key` | the key that encrypts the Cloudflare and Hugging Face tokens, 32 bytes in base64; set it to keep the key out of the volume's backups |
| `SOCRATES_TUNNEL_LISTEN` | a Unix socket in `/data/run` | where `cloudflared` reaches Socrates; a loopback address or nothing |
| `SOCRATES_CLOUDFLARED` | the one on the `PATH` (the image brings a pinned one), else downloaded into `/data/bin` | the `cloudflared` the tunnel runs |
| `SOCRATES_LOAD_TIMEOUT` | `20m` | how long an API request waits for a loading model |
| `SOCRATES_ENGINE_PORT` | `8181` | port of llama-server on `127.0.0.1` inside the container |
| `SOCRATES_LLAMA_SERVER`, `SOCRATES_LLAMA` | `/app/llama-server`, `/app/llama` | llama.cpp binaries |
| `SOCRATES_CONVERT_SCRIPTS`, `SOCRATES_PYTHON` | `/opt/llama-convert`, `python3` | the converter |

The host-name check does not apply to `/v1` and `/health`: clients may use any name (keys protect `/v1`). Through the
Cloudflare tunnel `/v1` needs no Access login but always a key; see [API](api.md#-through-the-cloudflare-tunnel).

## 💾 The data volume

```
/data
├── socrates.db          settings, password hash, sessions, the tunnel's settings, models, key hashes, request log
├── secret.key           encrypts the Cloudflare and Hugging Face tokens in socrates.db (unless SOCRATES_SECRET_KEY is set)
├── bin/cloudflared      downloaded when a tunnel is set up and none is installed (not with the image)
├── run/tunnel.sock      where cloudflared reaches Socrates
├── models/<id>/         the GGUF files of each model
└── tools/convert-venv/  Python for conversions (created on first use)
```

**Backup:** stop the container and copy `socrates.db`; models can be downloaded again. Keep `secret.key` apart from
that copy (or set `SOCRATES_SECRET_KEY` from a secret store): both together open the Cloudflare and Hugging Face tokens.

## ⬆️ Updates

```sh
docker pull ghcr.io/saschazesiger/socrates-api:latest
docker rm -f socrates && docker run … (as before)
```

The image pins a llama.cpp build (`LLAMA_CPP_BUILD` in the [Dockerfile](../Dockerfile)); a new release brings a
newer one, with support for newer architectures. Models, keys and settings stay.

## 🌐 Docker networking

The engine talks to Socrates over `127.0.0.1` inside the container; only port 8080 is needed. On hosts where
containers on the default bridge have no internet access (no DNS), downloads fail: use `--network host` (then set
`SOCRATES_PORT` if 8080 is taken on the host).
