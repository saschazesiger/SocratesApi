# 🔌 Internal API (interface ↔ server)

The web interface talks to the Go server over JSON under `/api/`. This is **not** the public model API (that is
`/v1/*`, see [API](api.md)). Everything here needs the admin session cookie; every request that is not `GET` must
carry the header `X-Socrates: 1` (CSRF guard). Errors come as `{"error": "text for the user", "fields": {"name": "text"}}`
with a 4xx/5xx status. Times are Unix milliseconds, sizes are bytes.

## 🔐 Session

| Method | Path | Body | Answer |
|---|---|---|---|
| GET | `/api/session` | | `{signedIn, needsPassword, version}` (no auth needed) |
| POST | `/api/setup` | `{password}` | first start only: sets the admin password and signs in |
| POST | `/api/login` | `{password}` | `{signedIn: true}` |
| POST | `/api/logout` | | |
| POST | `/api/password` | `{current, password}` | `{message}` |

## 📡 Live updates: `GET /api/events` (server-sent events)

| Event | Data | When |
|---|---|---|
| `status` | the full [Status](#-status) object | on every change, at most 4 per second, and every 2 s while anything is busy |
| `change` | `{topics: ["models", "keys", "requests", "settings"]}` | something the page lists changed: refetch it |
| `log` | `{lines: ["..."]}` | new lines of the engine log (llama-server output) |

## 🩺 Status

`GET /api/status` returns the same object the `status` event carries:

```jsonc
{
  "engine": {
    "state": "stopped",        // stopped | planning | starting | loading | ready | error | crashed
    "message": "Loading weights into memory", // one short sentence for the person, always set
    "progress": 0.42,          // 0..1 while planning/loading, else null
    "stage": "text_model",     // text_model | spec_model | mmproj_model while loading
    "since": 1731400000000,    // when the state began
    "error": "",               // last error in plain English (state error/crashed)
    "modelId": "huihui-qwen3-8-27b-q4-k",
    "modelName": "Huihui Qwen3.8 27B Q4_K",
    "ctx": 32768,              // context the engine runs with
    "slots": 1,
    "restarts": 0,
    "uptimeMs": 123456,
    "args": ["-m", "/data/models/...", "-c", "32768", "..."] // the full llama-server command line, for debugging
  },
  "activity": {
    "running": 1, "queued": 0,
    "requests": [ {                         // in flight now
      "id": "r12", "source": "chat",         // chat | api
      "key": "opencode",                     // API key name, "" for chat
      "phase": "prompt",                     // queued | prompt | thinking | generating
      "promptTokens": 5120, "promptDone": 2048, "cachedTokens": 1024, // promptTokens 0 = total not known yet (non-streaming or Anthropic requests): show "2 048 tokens read so far"
      "generated": 0, "tps": 0,              // tokens per second of the current phase
      "startedAt": 1731400000000 } ],
    "last": { "promptTokens": 812, "cachedTokens": 0, "completionTokens": 233, "promptTps": 1450.2, "genTps": 38.5, "ms": 6600, "at": 1731400000000 }
  },
  "placement": {                             // of the active model; null when nothing is loaded or planned
    "source": "measured",                    // estimated (before load) | measured (from the running engine)
    "fileBytes": 16810000000,
    "layers": { "total": 65, "gpu": 65, "expertLayersOnCpu": 0, "moe": false },
    "gpu":  { "weights": 16000000000, "kv": 2100000000, "compute": 500000000 },
    "ram":  { "weights": 810000000, "weightsResident": 810000000, "kv": 0, "compute": 60000000 },
    "disk": { "weights": 0 },                // weights that are only on NVMe right now (mmap, not in the page cache)
    "notes": ["Everything fits on the GPU."]
  },
  "system": {
    "gpus": [ { "name": "NVIDIA GeForce RTX 3090", "total": 25769803776, "used": 19000000000, "util": 87, "tempC": 61, "powerW": 280 } ],
    "ram":  { "total": 66000000000, "used": 30000000000, "available": 36000000000, "cached": 12000000000, "engineRss": 3000000000 },
    "disk": { "path": "/data", "total": 2000000000000, "free": 1700000000000 },
    "cpu":  { "name": "Intel Core i7-11700KF", "threads": 16, "load": 0.35 }
  },
  "downloads": [ { "modelId": "...", "file": "x.gguf", "done": 1000, "total": 2000, "bytesPerSec": 98000000, "state": "downloading" } ]
}
```

## 🧠 Models

A **Model** as returned by the server:

```jsonc
{
  "id": "huihui-qwen3-8-27b-q4-k", "name": "Huihui Qwen3.8 27B · Q4_K",
  "repo": "huihui-ai/Huihui-Qwen3.8-27B-abliterated-GGUF", "quant": "Q4_K",
  "files": [{ "path": "Huihui-Qwen3.8-27B-abliterated.Q4_K.gguf", "size": 16810000000 }],
  "mmproj": "mmproj-model-bf16.gguf", "size": 17740000000,
  "state": "ready",            // queued | downloading | converting | ready | failed
  "error": "", "progress": 1,  // 0..1 while downloading or converting
  "active": true,
  "meta": { "arch": "qwen35", "params": 27800000000, "ctxTrain": 262144, "layers": 65, "experts": 0, "expertsUsed": 0,
            "moe": false, "vision": true, "mtp": true, "chatTemplate": true },
  "settings": { /* Settings, see below */ },
  "createdAt": 0
}
```

**Settings** (every field optional; missing = default):

| Field | Type | Default | Meaning |
|---|---|---|---|
| `context` | int | 0 | context size in tokens; 0 = automatic: as large as fits on the GPU, between 32k and 128k (never below 32k: weights move to RAM first) |
| `gpuLayers` | `"auto"` or int | auto | layers on the GPU |
| `cpuMoe` | `"auto"` or int | auto | MoE: layers whose experts stay in RAM |
| `kvType` | `f16`,`q8_0`,`q4_0` | q8_0 | KV cache precision |
| `flashAttn` | `auto`,`on`,`off` | auto | |
| `loadMode` | `mmap`,`mlock`,`ram` | mmap | how weights not on the GPU are held: mmap (page cache, can spill to NVMe), mlock (pinned in RAM), ram (copied into RAM) |
| `parallel` | int | 1 | requests served at the same time (the context is shared) |
| `threads` | int | 0 | CPU threads, 0 = automatic |
| `batch`, `ubatch` | int | 2048, 512 | prompt batch sizes |
| `speculative` | `off`,`mtp` | mtp if the model has MTP | multi-token prediction |
| `vision` | bool | true if an mmproj exists | load the vision projector |
| `reasoning` | `auto`,`on`,`off` | auto | thinking |
| `reasoningBudget` | int | -1 | -1 = unlimited |
| `gpuReserveMb` | int | 1024 | VRAM kept free by the automatic fit |
| `preload` | bool | true | read the weights that live in RAM into the page cache right after start |
| `temperature`, `topP`, `topK`, `minP`, `presencePenalty`, `repeatPenalty` | number | model card | default sampling |
| `extraArgs` | string | "" | more llama-server flags, as on a command line |

| Method | Path | Body | Answer |
|---|---|---|---|
| GET | `/api/models` | | `{models: [Model], activeId, dataDir, freeBytes}` |
| POST | `/api/models` | `{repo, files: ["a.gguf"], mmproj: "mmproj.gguf" or "", name}` | the new Model (download starts) |
| POST | `/api/models/convert` | `{repo, quant: "Q4_K_M", name}` | the new Model (state converting) |
| PATCH | `/api/models/{id}` | `{name?, settings?}` | the Model; if active, the engine restarts with it |
| DELETE | `/api/models/{id}` | | removes files too |
| POST | `/api/models/{id}/activate` | | starts the engine with it |
| POST | `/api/models/{id}/plan` | `{settings}` | [Plan](#plan) without loading anything (takes a few seconds) |
| POST | `/api/models/{id}/retry` | | restarts a failed download |
| POST | `/api/engine/stop` | | unloads, no active model |
| POST | `/api/engine/restart` | | |
| GET | `/api/logs` | | `{lines: [...]}` the last 1000 lines of the engine log |

### Plan

```jsonc
{ "ok": true, "error": "",
  "ctx": 32768, "gpuLayers": 65, "cpuMoe": 0, "args": ["..."],
  "placement": { /* like status.placement, source "estimated" */ },
  "fits": "gpu",           // gpu | hybrid | disk | no  (no = not even RAM + NVMe)
  "summary": "Everything on the GPU: 19.2 GB of 24 GB.",
  "tokensPerSecondHint": "fast" } // fast | medium | slow
```

## 🤗 Hugging Face

| Method | Path | Answer |
|---|---|---|
| GET | `/api/hf/search?q=qwen` | `{models: [{id, downloads, likes, gguf, updatedAt, pipeline}]}` |
| GET | `/api/hf/repo?id=owner/name` | see below |

```jsonc
{
  "id": "huihui-ai/Huihui-Qwen3.8-27B-abliterated-GGUF", "gated": false, "license": "apache-2.0",
  "pipeline": "image-text-to-text", "params": 27800000000, "baseModel": "Qwen/Qwen3.8-27B",
  "gguf": true,
  "variants": [                       // one entry per quantisation (split files grouped)
    { "quant": "Q4_K", "files": ["Huihui-Qwen3.8-27B-abliterated.Q4_K.gguf"], "size": 16810000000,
      "fit": "gpu", "note": "Fits on the GPU with 32k context", "recommended": true } ],
  "mmproj": [ { "file": "mmproj-model-bf16.gguf", "size": 930000000 } ],
  "quantized": [ { "id": "mradermacher/...-GGUF", "downloads": 49375 } ], // GGUF versions made by others (when gguf is false)
  "convertible": true                 // safetensors of an architecture llama.cpp can convert
}
```

## 💬 Chat

`POST /api/chat` takes an OpenAI chat-completions body and answers with the same server-sent event stream as
`/v1/chat/completions` (`stream: true` is forced). The server adds `return_progress` and `timings_per_token`, so
chunks may carry `prompt_progress: {total, cache, processed, time_ms}` and `timings`. Deltas carry `content` and
`reasoning_content`.

## 🔑 API keys and settings

| Method | Path | Body | Answer |
|---|---|---|---|
| GET | `/api/keys` | | `{keys: [Key], baseUrl}` |
| POST | `/api/keys` | `{name}` | `{key: Key, secret: "sk-soc-..."}` (the secret is shown once) |
| DELETE | `/api/keys/{id}` | | |
| GET | `/api/requests` | | `{requests: [Request]}` the latest 100 |
| GET | `/api/settings` | | `{hfToken: "set" or "", requireKey: true, modelsDir}` |
| PUT | `/api/settings` | `{hfToken?, requireKey?}` | the settings |

Key: `{id, name, prefix, createdAt, lastUsedAt, requests, tokensIn, tokensOut}`.
Request: `{id, at, source, keyName, path, model, status, promptTokens, cachedTokens, completionTokens, promptTps, genTps, ms, error}`.
