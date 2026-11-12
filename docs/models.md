# 🤗 Models

## 🔎 Finding a model

*Models* → **Add a model**. Type a name, or paste `owner/name` or a huggingface.co link: a repository opens directly.

A repository with GGUF files lists its **quantisations**, split files grouped, smallest first:

| Shown | Meaning |
|---|---|
| `UD-Q4_K_XL · 17.4 GB` | the part of the file name that tells the variants apart, and the download size |
| ⭐ Recommended | the best quality that runs well on this machine (about 4.5–5 bits on the GPU if it fits, else the largest that fits GPU + RAM) |
| Fit chip | *Fits on the GPU*, *GPU + RAM*, *Spills to NVMe* or *Too large*, for this machine's VRAM, free RAM and disk |

> 💡 **Which quant?** Q4_K_M / UD-Q4_K_XL is the sweet spot. Q5/Q6 answer a little better and need more memory; Q3
> and below lose noticeably, except on very large MoE models. `UD-` (Unsloth Dynamic) and `i1` / imatrix quants
> are better than plain ones of the same size.

Gated or private models need a Hugging Face token: *API* → *Settings* → *Hugging Face token*.

## ⬇️ Downloading

- Six connections in parallel, 64 MB chunks; typically 90+ MB/s.
- Resumes after a container restart where it stopped.
- Checked against Hugging Face's SHA-256 before it is used.
- Files go to `/data/models/<model id>/`.

The model id (`huihui-qwen3.8-27b-abliterated-ud-q4_k_xl`) is what `/v1/models` reports. You can rename the model
in its settings; the id stays.

## 🛠️ Converting safetensors

A repository without GGUF files (for example `huihui-ai/Huihui-Qwen3.8-27B-abliterated` itself) shows the
**GGUF versions others made** of it: usually the better choice, often with imatrix quants. If you need your own:
**Convert it myself**, choose a quantisation.

| Step | What happens | Time (27B) |
|---|---|---|
| 1 Checking | Is there enough disk space? (original + 16-bit copy + result) | |
| 2 Preparing | Python and llama.cpp's converter into `/data/tools` (first time only, ~1 GB) | ~1 min |
| 3 Downloading | the original safetensors (54 GB for a 27B model) | ~10 min |
| 4 Converting | `convert_hf_to_gguf.py` writes a 16-bit GGUF, plus the vision projector | ~5 min |
| 5 Quantising | `llama quantize` to the chosen type; the originals are deleted | ~3 min |

The converter is the one of the bundled llama.cpp build, so every architecture llama.cpp can run can be converted.

## ⚙️ Activating, settings, deleting

| Action | Effect |
|---|---|
| **Activate** | stops the running model and starts this one; API requests wait meanwhile |
| **Settings** | context, offloading, sampling … with a placement preview ([Memory](memory.md#-what-you-can-change)); saving restarts the model if it is active |
| **Delete** | removes the files; an active model is unloaded first |
| **Retry** | continues a failed download or conversion |

### Advanced: extra llama.cpp flags

*Settings* → *Advanced* → **Extra flags** are appended to `llama-server`'s command line, for example
`--override-kv qwen3.expert_used_count=int:8` or `--chat-template-file /data/my.jinja`. `--host`, `--port`, `-m`
and API key flags are set by Socrates and refused here. The full command line of the running engine is under
*Command line* on the Models page.
