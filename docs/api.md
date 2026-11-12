# 🔌 API

The model API is OpenAI- and Anthropic-compatible and lives on the same port as the interface, under `/v1`.

| | |
|---|---|
| **Base URL** | `http://<host>:8080/v1` (Anthropic clients: `http://<host>:8080`) |
| **Key** | `Authorization: Bearer sk-soc-…`, or `x-api-key: sk-soc-…` |
| **Model** | any name: requests always go to the active model. `GET /v1/models` names it. |

## 🔑 API keys

*API* → **Create a key**, give it the name of the app (`opencode`, `laptop`). The key is shown **once**; Socrates
keeps only its SHA-256. Each key shows when it was last used, its requests and tokens; **Revoke** ends it at once.

Keys are required by default. *API* → *Settings* → **Require an API key** off makes the API open to everybody who
can reach the port; keys still work and are still counted.

## 📜 Endpoints

| Method | Path | Notes |
|---|---|---|
| POST | `/v1/chat/completions` | streaming, `tools` / `tool_choice`, `response_format` (JSON schema), images (`image_url`) with a vision model, reasoning in `reasoning_content`, `chat_template_kwargs` (e.g. `{"enable_thinking": false}`) |
| POST | `/v1/completions` | plain text completion |
| GET | `/v1/models` | the active model |
| POST | `/v1/embeddings` | needs an embedding model |
| POST | `/v1/messages` | Anthropic Messages: streaming, tools, `thinking` blocks |
| POST | `/v1/messages/count_tokens` | Anthropic token count |
| GET | `/health` | no key needed: `200 {"status":"ready"}` or `503` |

Everything else under `/v1/` is passed to llama-server as it is (for example `/v1/responses` where llama.cpp has it).

**While the model loads** a request waits (up to 20 minutes, `SOCRATES_LOAD_TIMEOUT`) and then runs. With no
active model it fails at once with `503` and a message that says so.

**Errors** come in the OpenAI shape: `{"error": {"message": "…", "type": "…", "code": 401}}`.

## 🧩 opencode

`opencode.json` in your project (or `~/.config/opencode/opencode.json`); the API page has it with your address and
model filled in:

```json
{
  "$schema": "https://opencode.ai/config.json",
  "provider": {
    "socrates": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "Socrates API",
      "options": { "baseURL": "http://<host>:8080/v1", "apiKey": "{env:SOCRATES_API_KEY}" },
      "models": {
        "huihui-qwen3.8-27b-abliterated-ud-q4_k_xl": {
          "name": "Huihui Qwen3.8 27B",
          "limit": { "context": 130560, "output": 32768 }
        }
      }
    }
  },
  "model": "socrates/huihui-qwen3.8-27b-abliterated-ud-q4_k_xl"
}
```

```sh
export SOCRATES_API_KEY=sk-soc-...
opencode
```

`limit.context` lets opencode compact the conversation before it outgrows the model's context. In scripts, close
stdin for `opencode run` (`opencode run "…" < /dev/null`), otherwise it waits for input before sending anything. Tested: opencode
1.19 writes and runs files with tool calls against Qwen3.8 27B.

## 🐍 OpenAI Python SDK

```python
import os
from openai import OpenAI

client = OpenAI(base_url="http://<host>:8080/v1", api_key=os.environ["SOCRATES_API_KEY"])
stream = client.chat.completions.create(
    model="local",
    messages=[{"role": "user", "content": "Hello!"}],
    stream=True,
)
for chunk in stream:
    print(chunk.choices[0].delta.content or "", end="", flush=True)
```

## 🅰️ Anthropic clients

```sh
export ANTHROPIC_BASE_URL="http://<host>:8080"
export ANTHROPIC_AUTH_TOKEN="$SOCRATES_API_KEY"
export ANTHROPIC_MODEL="local"
```

## 🌐 curl

```sh
curl http://<host>:8080/v1/chat/completions \
  -H "Authorization: Bearer $SOCRATES_API_KEY" -H "Content-Type: application/json" \
  -d '{"model":"local","messages":[{"role":"user","content":"What is the weather in Bern?"}],
       "tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object",
       "properties":{"city":{"type":"string"}},"required":["city"]}}}]}'
```

## 📈 Request log

*API* → **Recent requests**: time, key, endpoint, prompt / cached / generated tokens, tok/s, duration and status of
the last 100 requests (the database keeps 2 000). Requests in flight show on the Models page and in the chat's
status line, with the progress of the prompt.
