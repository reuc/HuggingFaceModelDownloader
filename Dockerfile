# =============================================================================
# HuggingFace Model Downloader - Docker Image (v3)
# =============================================================================
# Multi-stage build for minimal image size
#
# Build:
#   docker build -t hfdownloader .
#
# Run CLI (v3 - uses HuggingFace cache structure):
#   docker run --rm -v ~/.cache/huggingface:/home/hfdownloader/.cache/huggingface \
#     hfdownloader download TheBloke/Mistral-7B-Instruct-v0.2-GGUF
#
# Run CLI with filter:
#   docker run --rm -v ~/.cache/huggingface:/home/hfdownloader/.cache/huggingface \
#     hfdownloader download TheBloke/Mistral-7B-Instruct-v0.2-GGUF:q4_k_m
#
# Run Web Server:
#   docker run --rm -p 8080:8080 \
#     -v ~/.cache/huggingface:/home/hfdownloader/.cache/huggingface \
#     hfdownloader serve --port 8080
#
# With HuggingFace token (for private/gated models):
#   docker run --rm -e HF_TOKEN=hf_xxx -p 8080:8080 \
#     -v ~/.cache/huggingface:/home/hfdownloader/.cache/huggingface \
#     hfdownloader serve
#
# Environment variables (all optional):
#   HTTP_PORT  Web server port for `serve` (default 8080)
#   AUTH_USER  Basic-auth username for `serve`; also the SSH login name
#   AUTH_PASS  Basic-auth password for `serve`; also the SSH password
#   SSH_ENABLE Start an SSH/SFTP server (default false; needs AUTH_USER/AUTH_PASS)
#   SSH_PORT   SSH server port when SSH_ENABLE=true (default 22)
#
#   docker run --rm -p 3000:3000 -p 2222:2222 \
#     -e HTTP_PORT=3000 -e SSH_ENABLE=true -e SSH_PORT=2222 \
#     -e AUTH_USER=admin -e AUTH_PASS=secret \
#     -v ~/.cache/huggingface:/home/hfdownloader/.cache/huggingface \
#     hfdownloader serve
#
# Legacy mode (v2 behavior - flat directory structure):
#   docker run --rm -v ./models:/data hfdownloader download TheBloke/Mistral-7B-Instruct-v0.2-GGUF --legacy -o /data
#
# Credits: Original Docker support suggested by cdeving (#50)
# =============================================================================

# Build stage
FROM golang:1.26-alpine AS builder

# Install build dependencies
RUN apk add --no-cache git ca-certificates

WORKDIR /src

# Cache dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build the binary
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /hfdownloader ./cmd/hfdownloader

# =============================================================================
# Final stage - minimal image
# =============================================================================
FROM alpine:3.19

# Install ca-certificates for HTTPS, su-exec to drop root, and an optional
# SSH/SFTP server (only started when SSH_ENABLE=true)
RUN apk add --no-cache ca-certificates tzdata su-exec openssh-server && \
    mkdir -p /etc/ssh/sshd_config.d /etc/ssh/keys

# Create non-root user
RUN adduser -D -u 1000 hfdownloader

# Copy binary from builder
COPY --from=builder /hfdownloader /usr/local/bin/hfdownloader
COPY scripts/docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh

# Create HuggingFace cache directory (v3 default) and legacy data directory
RUN mkdir -p /home/hfdownloader/.cache/huggingface/hub \
             /home/hfdownloader/.cache/huggingface/models \
             /home/hfdownloader/.cache/huggingface/datasets \
             /data/Models /data/Datasets && \
    chown -R hfdownloader:hfdownloader /home/hfdownloader /data

# The entrypoint starts as root only to set up the optional SSH server, then
# drops to the hfdownloader user before running the app. Pass --user 1000 to
# skip root entirely (SSH_ENABLE is then unavailable).

# Set HF_HOME for the container (and for SSH sessions)
ENV HF_HOME=/home/hfdownloader/.cache/huggingface
RUN echo "export HF_HOME=$HF_HOME" > /etc/profile.d/hfdownloader.sh && \
    chmod +x /usr/local/bin/docker-entrypoint.sh

WORKDIR /home/hfdownloader

# Default to showing help
ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
CMD ["--help"]

# Expose web server port (publish SSH_PORT yourself if you set SSH_ENABLE)
EXPOSE 8080

# Labels
LABEL org.opencontainers.image.source="https://github.com/bodaay/HuggingFaceModelDownloader"
LABEL org.opencontainers.image.description="Fast, concurrent CLI and Web UI for downloading models and datasets from HuggingFace"
LABEL org.opencontainers.image.licenses="Apache-2.0"

