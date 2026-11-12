# Changelog

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
