# ⚡ Quick start

From an empty Linux server with an NVIDIA GPU to the first answer. Plan for 10 minutes plus the download.

## 🧰 1. Prepare the host

You need the NVIDIA driver (570 or newer) and Docker. Check:

```sh
nvidia-smi          # shows your GPU and "CUDA Version: 12.8" or higher
docker --version    # 25 or newer
```

Install the [NVIDIA Container Toolkit](https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/latest/install-guide.html)
and create the device list (CDI) Docker uses for `--device nvidia.com/gpu=all`:

```sh
# Ubuntu / Debian
curl -fsSL https://nvidia.github.io/libnvidia-container/gpgkey | sudo gpg --dearmor -o /usr/share/keyrings/nvidia-container-toolkit-keyring.gpg
curl -s -L https://nvidia.github.io/libnvidia-container/stable/deb/nvidia-container-toolkit.list \
  | sed 's#deb https://#deb [signed-by=/usr/share/keyrings/nvidia-container-toolkit-keyring.gpg] https://#g' \
  | sudo tee /etc/apt/sources.list.d/nvidia-container-toolkit.list
sudo apt-get update && sudo apt-get install -y nvidia-container-toolkit
sudo nvidia-ctk cdi generate --output=/etc/cdi/nvidia.yaml
```

Test it; you should see your GPU:

```sh
docker run --rm --device nvidia.com/gpu=all ubuntu nvidia-smi -L
```

> 💡 `--gpus all` works too where Docker is set up for it (`sudo nvidia-ctk runtime configure --runtime=docker`,
> then restart Docker). CDI needs no restart.

## 🐳 2. Start Socrates API

```sh
docker run -d --name socrates --restart unless-stopped \
  -p 8080:8080 -v socrates:/data --device nvidia.com/gpu=all \
  ghcr.io/saschazesiger/socrates-api:latest
```

Or with Compose: copy [docker-compose.yml](../docker-compose.yml) and run `docker compose up -d`. It also allows the
*mlock* load mode (`IPC_LOCK`, unlimited `memlock`).

All state lives in the volume `socrates` (`/data`): the database, the models, the conversion tools. The image is
about 7 GB, most of it the CUDA runtime of llama.cpp.

> ⚠️ **Open the interface right away.** Whoever opens it first chooses the admin password. Keep port 8080 off the
> internet.

## 🔐 3. Set the admin password

Open `http://<address of the machine>:8080`. Choose a password of at least 10 characters and repeat it. You are
signed in.

Forgot it later? Start the container once with `-e SOCRATES_ADMIN_PASSWORD=<new password>`; that signs everybody
out. Remove the variable afterwards.

## 🤗 4. Add a model

*Models* → **Add a model**.

1. **Search** (`qwen3.8 27b gguf`) or paste `owner/name` or a huggingface.co link.
2. **Choose a quantisation.** The marked one is the best fit for your machine. Each says where it would run:

   | Chip | Meaning |
   |---|---|
   | 🟢 Fits on the GPU | everything in VRAM: fastest |
   | 🔵 GPU + RAM | some weights in RAM: fine for MoE models, slower for dense ones |
   | 🟠 Spills to NVMe | larger than GPU + free RAM: works, parts are read from disk |
   | 🔴 Too large | not enough disk space |

3. Keep **Vision** ticked if the repository has an image projector (`mmproj`) and you want to send pictures.
4. **Download.** Progress, speed and time left show on the Models page. A download resumes after a restart and is
   checked with SHA-256.

The first model you add becomes active by itself. Repositories without GGUF files: see
[Models](models.md#-converting-safetensors).

## 🚀 5. Watch it load

The active model card goes through:

| State | What happens |
|---|---|
| 🧭 Planning | llama.cpp computes what fits where (a second or two) |
| 📖 Reading | the weights are read from the NVMe into memory (GB and GB/s shown) |
| ⏳ Loading | weights are copied to the GPU (percent) |
| ✅ Ready | requests are answered; the bars show the placement as measured |

Then try it in **Chat**.

## 🔑 6. Use it from your apps

*API* → **Create a key** → copy the key (it is shown once). The page has ready-made snippets for opencode, the OpenAI
Python SDK, curl and Anthropic clients, with your address filled in. See [API](api.md).
