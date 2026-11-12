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
| `internal/api/` | the interface's JSON API ([internal API](internal-api.md)), login, live events (SSE) |
| `internal/store/` | SQLite; the only place with SQL |
| `internal/sysinfo/` | GPU (nvidia-smi), RAM, disk, CPU |
| `web/` | the interface: HTML, CSS and ES modules on the vendored [Z-Dash](https://github.com/saschazesiger/Z-Dash) (`web/vendor/z-dash`, never edit) |
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
sh web/vendor/z-dash/tools/verify.sh web/vendor/z-dash
node web/vendor/z-dash/tools/check-project.mjs --config web/ds-check.json   # Node 22+
```

## 🎭 The mock server

```sh
node scripts/mock-server.mjs     # http://localhost:8090, starts at the first-start password
MOCK_PASSWORD=secret node scripts/mock-server.mjs
```

It serves `web/` and simulates the whole `/api/*` contract with changing data: a model that loads, a download, a
streaming chat with prompt progress and thinking. Use it for interface work and screenshots.

## 🐳 Building the image

```sh
docker build -t socrates-api:dev .
docker run -d --name socrates -p 8080:8080 -v socrates:/data --device nvidia.com/gpu=all socrates-api:dev
```

Pushes to `main` build `ghcr.io/saschazesiger/socrates-api:latest` ([release.yml](../.github/workflows/release.yml));
tags `vX.Y.Z` also build `:X.Y.Z`.
