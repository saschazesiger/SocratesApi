# syntax=docker/dockerfile:1
# The llama.cpp build Socrates API runs on. Update both together: the
# server image and the conversion scripts of the same build.
ARG LLAMA_CPP_BUILD=b12316
ARG LLAMA_CPP_COMMIT=5d4c8eeb24db049acf74bd6f354bed6eaeffc7dc

FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/socrates ./cmd/socrates

# Only the Python scripts of llama.cpp's converter (safetensors -> GGUF);
# their packages are installed into /data the first time they are used.
FROM alpine:3.22 AS convert
ARG LLAMA_CPP_COMMIT
RUN apk add --no-cache curl tar \
 && mkdir -p /out /tmp/src \
 && curl -fsSL "https://github.com/ggml-org/llama.cpp/archive/${LLAMA_CPP_COMMIT}.tar.gz" | tar -xz -C /tmp/src --strip-components=1 \
 && cd /tmp/src && cp -r convert_hf_to_gguf.py conversion gguf-py requirements LICENSE /out/

# cloudflared for the Cloudflare tunnel, part of the image, pinned with its
# checksum (as the Z-Dash server's template does; the server would download
# the same version into /data/bin otherwise). The release page of
# cloudflare/cloudflared lists both.
FROM alpine:3.22 AS cloudflared
ARG CLOUDFLARED_VERSION=2026.10.7
ARG CLOUDFLARED_SHA256=1f99fb518b83dec2ba8d870a7cc991d82a77e6196295a3d2754ba17ab9f91872
ADD https://github.com/cloudflare/cloudflared/releases/download/${CLOUDFLARED_VERSION}/cloudflared-linux-amd64 /cloudflared
RUN echo "${CLOUDFLARED_SHA256}  /cloudflared" | sha256sum -c - && chmod 0755 /cloudflared

FROM ghcr.io/ggml-org/llama.cpp:server-cuda12-${LLAMA_CPP_BUILD}
RUN apt-get update \
 && apt-get install -y --no-install-recommends python3-venv ca-certificates \
 && rm -rf /var/lib/apt/lists/*
COPY --from=convert /out /opt/llama-convert
COPY --from=cloudflared /cloudflared /usr/local/bin/cloudflared
COPY --from=build /out/socrates /usr/local/bin/socrates
# The tunnel's listener stays the Unix socket in /data/run (not the
# template's 127.0.0.1:8081): Socrates often runs with --network host, where
# a fixed loopback port would be the host's.
ENV SOCRATES_DATA=/data \
    SOCRATES_PORT=8080 \
    SOCRATES_LLAMA_SERVER=/app/llama-server \
    SOCRATES_LLAMA=/app/llama \
    SOCRATES_CONVERT_SCRIPTS=/opt/llama-convert
VOLUME /data
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s CMD ["socrates", "healthcheck"]
ENTRYPOINT ["socrates"]
