# 🔒 Security

| | |
|---|---|
| 🔐 **Interface** | On the [Z-Dash server](https://github.com/saschazesiger/Z-Dash/blob/main/docs/server.md): admin password (Argon2id), sessions in an HttpOnly, SameSite=Strict cookie; five wrong passwords, then a growing wait. Every change needs a header only the interface sends (no CSRF), the interface only answers to known host names (no DNS rebinding), and a strict Content-Security-Policy. |
| 🌐 **Remote access** | Optional, under *Settings*: a Cloudflare tunnel (no open port) behind Cloudflare Access, which signs people in by e-mail (one-time PIN); Socrates checks the Access token and the address on every request. The Cloudflare token is stored encrypted (`secret.key`). Through the tunnel the model API needs the Access login too. |
| 🔑 **API** | Keys `sk-soc-…`, stored as SHA-256 only; required by default. |
| 🌍 **Exposure** | Made for a local network, a VPN (Tailscale works well) or the Cloudflare tunnel. Do not publish port 8080 on the internet; if you must, use a reverse proxy with TLS. |
| 🤗 **Models** | Downloaded from Hugging Face and checked with SHA-256. GGUF files hold data, no code. For a conversion only weights, JSON and tokenizer files are downloaded, never `.py` files, so no code from a repository runs. |
| 🧪 **Uncensored models** | Abliterated models answer anything. You are responsible for what you serve to whom. |
| 🐳 **Container** | Runs as root inside the container (GPU access and mlock); the engine listens on `127.0.0.1` only. |

To report a vulnerability, see [SECURITY.md](../SECURITY.md).
