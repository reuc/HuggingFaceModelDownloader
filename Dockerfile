# syntax=docker/dockerfile:1
# =============================================================================
# HuggingFace Model Downloader - Docker Image (v3)
# =============================================================================
# Hardened Wolfi build, in three stages:
#
#   build   docker.io/reuc/wolfi-polyglot, distroless variant (Go toolchain,
#           no shell): compiles a static hfdownloader for the target platform.
#           There is no shell, so every RUN uses exec form.
#   rootfs  cgr.dev/chainguard/wolfi-base: installs the runtime packages into
#           /rootfs with apk --root, then adds the app user and directories.
#   final   FROM scratch + that rootfs: busybox (for the entrypoint script),
#           OpenSSH (only started when SSH_ENABLE=true), su-exec, CA bundle,
#           tzdata. No package manager.
#
# Both base images are pinned by digest; Dependabot (docker) keeps them current.
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
# Run Web Server (mount .config to keep settings and the download queue
# across restarts; queued/interrupted downloads resume on startup):
#   docker run --rm -p 8080:8080 \
#     -v ~/.cache/huggingface:/home/hfdownloader/.cache/huggingface \
#     -v ./hfd-config:/home/hfdownloader/.config \
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
#   FLATTEN    true: finished downloads become real files in models/ (no hub/)
#   CONVERT_ON_BOOT  true: at startup, convert completed downloads in hub/ to real files
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

# =============================================================================
# Build stage: wolfi-polyglot distroless. It runs on the build platform and
# cross-compiles, so multi-arch builds don't emulate the Go compiler.
# Published, signed and SLSA-attested by github.com/reuc/wolfi-polyglot; verify
# with: gh attestation verify oci://<image>@<digest> -R reuc/wolfi-polyglot
# =============================================================================
FROM --platform=$BUILDPLATFORM docker.io/reuc/wolfi-polyglot:20261107-distroless@sha256:917ed6d9eca1ffc9b928f37ef9fc1a640b905456325cdcfb5646fd31faad6c98 AS build

ARG TARGETOS
ARG TARGETARCH
# The image already sets GOTOOLCHAIN=local and GOFLAGS=-trimpath.
ENV CGO_ENABLED=0 \
    GOOS=${TARGETOS} \
    GOARCH=${TARGETARCH}

WORKDIR /home/nonroot/src

# Cache dependencies
COPY --chown=65532:65532 go.mod go.sum ./
RUN ["go", "mod", "download"]

# Build the binary
COPY --chown=65532:65532 . .
RUN ["go", "build", "-ldflags=-s -w", "-o", "/home/nonroot/hfdownloader", "./cmd/hfdownloader"]

# =============================================================================
# Runtime filesystem: Wolfi packages installed into /rootfs. Runs on the
# target platform (emulated for cross-arch builds; only apk and busybox run).
# =============================================================================
FROM cgr.dev/chainguard/wolfi-base:latest@sha256:8509ee4351a2e501b7f98201f2e2642396e82020c61a589cacdcfe74b9de46c1 AS rootfs

# Change it to force packages to be fetched again (e.g. for a new Wolfi fix).
ARG APK_CACHE_BUST=0

RUN set -eux; \
    echo "cache-bust: ${APK_CACHE_BUST}"; \
    mkdir -p /rootfs; \
    apk add --no-cache --no-scripts --initdb --root /rootfs \
        --keys-dir /etc/apk/keys \
        --repositories-file /etc/apk/repositories \
        wolfi-baselayout \
        ca-certificates-bundle \
        tzdata \
        busybox \
        su-exec \
        openssh-server \
        openssh-keygen; \
    # --no-scripts skips busybox's applet-link trigger, so create the links here.
    chroot /rootfs /bin/busybox --install -s; \
    # Everything the entrypoint calls must be present (some are symlinks that
    # only resolve inside the rootfs, so accept links).
    for b in /bin/sh /usr/bin/su-exec /usr/sbin/sshd /usr/bin/ssh-keygen \
             /usr/sbin/chpasswd /usr/bin/id /bin/grep /usr/bin/tr; do \
        [ -e "/rootfs$b" ] || [ -L "/rootfs$b" ] || { echo "ERROR: $b missing"; exit 1; }; \
    done; \
    for f in /sbin/apk /usr/bin/apk; do \
        if [ -e "/rootfs$f" ] || [ -L "/rootfs$f" ]; then echo "ERROR: $f present"; exit 1; fi; \
    done; \
    # App user (uid/gid 1000, like the previous Alpine image, so existing
    # volumes keep working). Locked password, like `adduser -D`.
    grep -q '^hfdownloader:' /rootfs/etc/passwd || \
        echo 'hfdownloader:x:1000:1000:hfdownloader:/home/hfdownloader:/bin/sh' >> /rootfs/etc/passwd; \
    grep -q '^hfdownloader:' /rootfs/etc/group || \
        echo 'hfdownloader:x:1000:' >> /rootfs/etc/group; \
    grep -q '^hfdownloader:' /rootfs/etc/shadow || \
        echo 'hfdownloader:!:0:0:99999:7:::' >> /rootfs/etc/shadow; \
    chmod 0600 /rootfs/etc/shadow; \
    # SSH config drop-in and host-key directories (used when SSH_ENABLE=true).
    mkdir -p /rootfs/etc/ssh/sshd_config.d /rootfs/etc/ssh/keys; \
    # HuggingFace cache (v3 default), config (settings, mirror targets and the
    # saved download queue) and legacy data directories.
    mkdir -p /rootfs/home/hfdownloader/.config/hfdownloader \
             /rootfs/home/hfdownloader/.cache/huggingface/hub \
             /rootfs/home/hfdownloader/.cache/huggingface/models \
             /rootfs/home/hfdownloader/.cache/huggingface/datasets \
             /rootfs/data/Models /rootfs/data/Datasets \
             /rootfs/tmp; \
    chown -R 1000:1000 /rootfs/home/hfdownloader /rootfs/data; \
    chmod 1777 /rootfs/tmp; \
    # HF_HOME for SSH login shells too.
    mkdir -p /rootfs/etc/profile.d; \
    echo 'export HF_HOME=/home/hfdownloader/.cache/huggingface' > /rootfs/etc/profile.d/hfdownloader.sh; \
    find /rootfs -xdev -type f -perm /6000 -exec chmod a-s {} +; \
    rm -rf /rootfs/var/cache/apk/* /rootfs/etc/apk/keys /rootfs/root/.cache

# =============================================================================
# Final stage
# =============================================================================
FROM scratch

COPY --from=rootfs /rootfs/ /
COPY --from=build /home/nonroot/hfdownloader /usr/local/bin/hfdownloader
COPY --chmod=0755 scripts/docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh

# The entrypoint starts as root only to set up the optional SSH server, then
# drops to the hfdownloader user before running the app. Pass --user 1000 to
# skip root entirely (SSH_ENABLE is then unavailable).

# HF_HOME for the container (SSH sessions get it from /etc/profile.d). HOME is
# pinned so settings and the download queue live in /home/hfdownloader/.config
# even when the container runs as a custom uid (--user 568:568) with no
# passwd entry.
ENV PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin \
    HF_HOME=/home/hfdownloader/.cache/huggingface \
    HOME=/home/hfdownloader \
    SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt

WORKDIR /home/hfdownloader

# Default to showing help
ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
CMD ["--help"]

# Expose web server port (publish SSH_PORT yourself if you set SSH_ENABLE)
EXPOSE 8080

# Labels
LABEL org.opencontainers.image.source="https://github.com/reuc/HuggingFaceModelDownloader"
LABEL org.opencontainers.image.description="Fast, concurrent CLI and Web UI for downloading models and datasets from HuggingFace"
LABEL org.opencontainers.image.licenses="Apache-2.0"
