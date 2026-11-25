# 🛠️ Development

## 🗺️ Layout

| Path | What |
|---|---|
| `cmd/socrates/` | `main`: reads the environment, wires the packages |
| `internal/engine/` | supervises llama-server: planning (`plan.go`), settings, start, child-protocol progress, crash restarts, placement measurement (`residency.go`: mincore, preload) |
| `internal/gguf/` | reads GGUF headers: metadata and the byte range of every tensor |
| `internal/hf/` | Hugging Face: search, repository files, quantised versions, the resumable parallel downloader |
| `internal/proxy/` | `/v1/*`: keys, wait-for-ready, streaming pass-through, live activity of every request |
| `internal/convert/` | safetensors → GGUF → quantised GGUF |
| `internal/api/` | the interface's JSON API ([internal API](internal-api.md)) and the live status stream, registered on the Z-Dash server |
| `internal/store/` | SQLite; the only place with SQL; `ServerKV` is the `kv` table as the Z-Dash server keeps its state in it |
| `internal/sysinfo/` | GPU (nvidia-smi), RAM, disk, CPU |
| `web/` | the interface: HTML, CSS and ES modules on Z-Dash and the shell of the Z-Dash server (served at `/z-dash/`) |
| `scripts/mock-server.mjs` | the interface on simulated data |

## 🧠 How the engine talks to llama.cpp

| What | How |
|---|---|
| Placement | `llama fit-params` (decides) and `--fit-print on` (MiB per device), then `llama-server … --fit off` |
| Loading progress and allocated memory | llama-server's router-child protocol: with `LLAMA_SERVER_ROUTER_PORT` set it writes `cmd_child_to_router:state:{json}` lines to stdout |
| Prompt progress of a request | `return_progress: true`; for API clients Socrates removes those extra chunks again |
| Queue and API requests without a stream | `GET /slots` while requests are in flight |
| Weights in RAM | `mincore` over the byte ranges of the CPU tensors; `/proc/<pid>/io` while the files are read in |

The llama.cpp build is pinned in the [Dockerfile](../Dockerfile) (`LLAMA_CPP_BUILD`, `LLAMA_CPP_COMMIT`). After
an update check the child protocol and the log lines still match (`go test ./...` covers the parsers).

## ✅ Checks

```sh
go build ./... && go vet ./... && go test ./...
go mod download github.com/saschazesiger/Z-Dash/server
ZD="$(go list -m -f '{{.Dir}}' github.com/saschazesiger/Z-Dash/server)/zdash/dist"
rm -rf web/.z-dash && cp -R "$ZD" web/.z-dash && chmod -R u+w web/.z-dash   # the module's copy of Z-Dash, for its tools
sh web/.z-dash/tools/verify.sh web/.z-dash
node web/.z-dash/tools/check-project.mjs --config web/ds-check.json   # Node 22+
node web/.z-dash/tools/check-theme.mjs web/theme.css --with web/.z-dash/themes/dark.css
```

## 🎭 The mock server

```sh
node scripts/mock-server.mjs     # http://localhost:8090, starts at the first-start password
MOCK_PASSWORD=secret node scripts/mock-server.mjs
```

It serves `web/`, Z-Dash and the shell from the Z-Dash server module (`go list -m`; needs Go and the module
downloaded, or `ZDASH_SERVER=<path to its server/>`), and simulates the whole `/api/*` contract with changing data: a
model that loads, a download, a streaming chat with prompt progress and thinking, the server's sign-in and an
untouched tunnel. Use it for interface work and screenshots.

## 🐳 Building the image

```sh
docker build -t socrates-api:dev .
docker run -d --name socrates -p 8080:8080 -v socrates:/data --device nvidia.com/gpu=all socrates-api:dev
```

Pushes to `main` build `ghcr.io/saschazesiger/socrates-api:latest` ([release.yml](../.github/workflows/release.yml));
tags `vX.Y.Z` also build `:X.Y.Z`.

## 🎨 The Z-Dash server and design system

Socrates API runs on the [Z-Dash](https://github.com/saschazesiger/Z-Dash) server, the Go module
`github.com/saschazesiger/Z-Dash/server` (its [docs/server.md](https://github.com/saschazesiger/Z-Dash/blob/main/docs/server.md)).
It brings the listener, the sign-in (the admin password on the local address, Cloudflare Access through the
tunnel), the Host and CSRF guards, the Content-Security-Policy, the change notices (`/api/events`), the Cloudflare
tunnel, and Z-Dash 3 itself, served at `/z-dash/`; the shell at `/z-dash/server/` draws the sign-in, the account
capsule and the tunnel and password parts of *Settings*. Socrates registers its own routes on it (`internal/api`):
`/api/*` for signed-in people, `/v1/` and `/health` as unguarded routes (any host name, no session: the proxy's
keys decide). It keeps its own database; the server keeps its password, sessions and tunnel in Socrates' `kv`
table (`store.ServerKV`), under the keys and in the format Socrates used before, so an update keeps them. The
cookie stays `socrates_session` (slug `socrates`).

The workflow `.github/workflows/update-z-dash-server.yml` takes the module's `main` once a day, runs the checks
and `go test ./...`, commits `go.mod` and `go.sum` and starts the release. By hand:

```sh
go get github.com/saschazesiger/Z-Dash/server@main && go mod tidy
go test ./...
```
