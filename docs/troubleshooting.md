# 🩺 Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| "No GPU found" / the GPU bar is missing | the container does not see the GPU | `docker run --rm --device nvidia.com/gpu=all ubuntu nvidia-smi -L` must list it; see [Quick start](quick-start.md#-1-prepare-the-host) |
| `docker: Error response from daemon: … CDI` | no CDI spec | `sudo nvidia-ctk cdi generate --output=/etc/cdi/nvidia.yaml` |
| Downloads fail with "Hugging Face does not answer" | the container has no internet (DNS) | test `docker run --rm alpine wget -qO- https://huggingface.co`; otherwise `--network host` |
| "access refused (401/403)" when adding a model | gated or private repository | accept the terms on huggingface.co, set a token under *API* → *Settings* |
| Planning failed: "does not know the model's architecture" | the architecture is newer than the bundled llama.cpp | update the image; or choose another model |
| Crashed: "the GPU ran out of memory" | another program took VRAM, or settings were forced | raise *VRAM kept free*, leave layers on *Automatic*, lower the context |
| Slow answers, NVMe bar shows weights read from disk | the model is larger than free RAM | a smaller quantisation, close programs that use RAM, or accept it (MoE models cope well) |
| *mlock* fails to start | not enough RAM, or no `IPC_LOCK` | use mmap, or start with the compose file |
| The interface says it does not know the host name | DNS-rebinding protection | add the name to `SOCRATES_HOSTS` |
| Forgot the admin password | | start once with `SOCRATES_ADMIN_PASSWORD=…` |

**The engine log** (*Models* → *Engine log*, or the chat's *Engine log*) shows llama-server's output live, with
Socrates's own steps marked `── socrates:`. The full command line is under *Command line* on the Models page:
run it in the container (`docker exec -it socrates bash`) to reproduce a problem by hand.
