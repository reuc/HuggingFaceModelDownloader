<div align="center">

# HuggingFace Downloader

Fork of [bodaay/HuggingFaceModelDownloader](https://github.com/bodaay/HuggingFaceModelDownloader).

**Parallel downloads** • **Smart GGUF analyzer** • **Python compatible** • **Full proxy support**

[Quick Start](#quick-start) •
[Why This Tool](#why-this-tool) •
[Smart Analyzer](#smart-analyzer) •
[Web UI](#web-ui) •
[Mirror Sync](#mirror-sync) •
[Proxy Support](#proxy-support)

</div>

---

## Why This Tool?

### Parallel Downloads

Maximize your bandwidth with **multiple connections per file** and **concurrent file downloads**:

- Up to 16 parallel connections per file (chunked download)
- Up to 8 files downloading simultaneously
- Automatic resume on interruption

![CLI Download Progress](docs/screenshots/cli-tui.png)

Real-time progress with per-file status, speed, and ETA.

### Interactive GGUF Picker

Don't guess which quantization to download. Use `-i` for an **interactive picker** with quality ratings and RAM estimates:

```bash
hfdownloader analyze -i TheBloke/Mistral-7B-Instruct-v0.2-GGUF
```

![GGUF Analyzer TUI](docs/screenshots/cli-tui2.png)

**Interactive mode features:**
- **Keyboard navigation** - Use ↑↓ to browse, space to toggle selection
- **Quality ratings** - Stars (★★★★☆) show relative quality
- **RAM estimates** - Know if it'll fit in your VRAM
- **"Recommended" badge** - We highlight the best balance (Q4_K_M)
- **Live totals** - See combined size as you select
- **One-click download** - Press Enter to start, or `c` to copy command

**Without `-i`**, output is text/JSON — perfect for scripts and piping to other tools.

### Python Just Works

Downloads go to the standard HuggingFace cache. Python libraries find them automatically:

```python
from transformers import AutoModel
model = AutoModel.from_pretrained("TheBloke/Mistral-7B-Instruct-v0.2-GGUF")  # Just works
```

Plus, you get **human-readable paths** at `~/.cache/huggingface/models/` for easy browsing.

### Works Behind Corporate Firewalls

Full proxy support including **SOCKS5**, **authentication**, and **CIDR bypass rules**:

```bash
hfdownloader download meta-llama/Llama-2-7b --proxy socks5://localhost:1080
```

---

## Quick Start

**Try it first — no installation required:**

```bash
# Analyze a model with interactive GGUF picker
bash <(curl -sSL https://raw.githubusercontent.com/reuc/HuggingFaceModelDownloader/main/scripts/gist_gethfd.sh) analyze -i TheBloke/Mistral-7B-Instruct-v0.2-GGUF

# Download a model
bash <(curl -sSL https://raw.githubusercontent.com/reuc/HuggingFaceModelDownloader/main/scripts/gist_gethfd.sh) download TheBloke/Mistral-7B-Instruct-v0.2-GGUF

# Start web UI
bash <(curl -sSL https://raw.githubusercontent.com/reuc/HuggingFaceModelDownloader/main/scripts/gist_gethfd.sh) serve

# Start web UI with authentication
bash <(curl -sSL https://raw.githubusercontent.com/reuc/HuggingFaceModelDownloader/main/scripts/gist_gethfd.sh) serve --auth-user admin --auth-pass secret
```

**Like it? Install permanently (no sudo):**

```bash
bash <(curl -sSL https://raw.githubusercontent.com/reuc/HuggingFaceModelDownloader/main/scripts/gist_gethfd.sh) install
```

By default this installs to `~/.local/bin` (or `~/bin` if that's already on
your PATH) so no sudo prompt is needed. Pass an explicit path to override:

```bash
# System-wide install (may prompt for sudo)
bash <(curl -sSL https://raw.githubusercontent.com/reuc/HuggingFaceModelDownloader/main/scripts/gist_gethfd.sh) install /usr/local/bin
```

Now use directly:

```bash
hfdownloader analyze -i TheBloke/Mistral-7B-Instruct-v0.2-GGUF
hfdownloader download TheBloke/Mistral-7B-Instruct-v0.2-GGUF:q4_k_m
hfdownloader serve
hfdownloader serve --auth-user admin --auth-pass secret   # with authentication
```

Files go to `~/.cache/huggingface/` — Python libraries find them automatically.

---

## Smart Analyzer

Not sure what's in a repository? Analyze it first:

```bash
hfdownloader analyze <any-repo>
```

For **GGUF models**, you get an interactive picker (see screenshot above). For other types, the analyzer **auto-detects** and shows relevant information:

| Type | What It Shows |
|------|---------------|
| **GGUF** | Interactive picker with quality ratings, RAM estimates, multi-select |
| **Transformers** | Architecture, parameters, context length, vocabulary size |
| **Diffusers** | Pipeline type, components, variants (fp16, bf16) |
| **LoRA** | Base model, rank, alpha, target modules |
| **GPTQ/AWQ** | Bits, group size, estimated VRAM |
| **Dataset** | Formats, configs, splits, sizes |

### Multi-Branch Support

Some repos have multiple branches (fp16, onnx, flax). The analyzer lets you pick:

```bash
hfdownloader analyze -i CompVis/stable-diffusion-v1-4
```

![Branch Picker](docs/screenshots/cli-tui3.png)

### Diffusers Component Picker

For Stable Diffusion models, pick exactly which components you need:

![Diffusers Picker](docs/screenshots/cli-tui4.png)

Select `unet`, `vae`, `text_encoder` — skip what you don't need. The command is generated automatically.

---

## Download Features

### Inline Filter Syntax

Download specific files without extra flags:

```bash
# Download only Q4_K_M quantization
hfdownloader download TheBloke/Mistral-7B-Instruct-v0.2-GGUF:q4_k_m

# Download multiple quantizations
hfdownloader download TheBloke/Mistral-7B-Instruct-v0.2-GGUF:q4_k_m,q5_k_m

# Or use flags
hfdownloader download TheBloke/Mistral-7B-Instruct-v0.2-GGUF -F q4_k_m -E ".md,fp16"
```

### Resume & Verify

```bash
# Failed or interrupted (network error, Ctrl+C, crash, reboot)? Resume it -
# same repo, filters, destination and settings; partial files continue
# where they stopped
hfdownloader resume            # the failed/interrupted download
hfdownloader resume --list     # several? list them, then: resume <ID> or --all

# Running the same command again also resumes
hfdownloader download owner/repo

# Branch updated upstream? Run again - only new or changed files are
# downloaded; unchanged ones (matched by content hash) are reused
hfdownloader download owner/repo

# Large (LFS) files are always SHA256-verified; --verify sets the check
# for the remaining files (none|size|etag|sha256, default size)
hfdownloader download owner/repo --verify sha256

# Preview what would download
hfdownloader download owner/repo --dry-run
```

### High-Speed Mode

```bash
# Maximum parallelism
hfdownloader download owner/repo -c 16 --max-active 8
```

| Flag | Default | Description |
|------|---------|-------------|
| `-c, --connections` | 8 | Connections per file |
| `--max-active` | 3 | Concurrent file downloads |
| `-F, --filters` | | Include patterns |
| `-E, --exclude` | | Exclude patterns |
| `-b, --revision` | main | Branch, tag, or commit |

---

## Storage Modes

Two modes are fully supported. Pick whichever fits your workflow — neither
is going away.

### Mode 1 — HuggingFace cache (default, dual-layer)

```bash
hfdownloader download TheBloke/Mistral-7B-Instruct-v0.2-GGUF
```

Files go into the standard HuggingFace cache so Python libraries
(`transformers`, `diffusers`, `huggingface_hub`, llama.cpp's Python
bindings, …) find them automatically — nothing to configure.

```
~/.cache/huggingface/
├── hub/                              # Layer 1: HF cache (Python compatible)
│   └── models--TheBloke--Mistral.../
│       ├── blobs/                    # real files, content-addressed
│       ├── snapshots/a1b2c3d4.../
│       │   └── model.gguf            → symlink to blobs/<sha>
│       └── refs/main
│
└── models/                           # Layer 2: human-readable view
    └── TheBloke/
        └── Mistral-7B-GGUF/
            ├── model.gguf            → symlink to hub/.../snapshots/...
            └── hfd.yaml              # download manifest
```

**Layer 1 (`hub/`)**: Standard HF cache structure. Python tools just work.
**Layer 2 (`models/`)**: Human-readable paths via symlinks — browse your
downloads like normal folders.

> **Windows**: Symlinks need Administrator or Developer Mode on Windows, so
> without them hfdownloader uses **hardlinks** instead — real files that
> share the same disk space, which Python, the HF CLI and you can all use.
> On drives without hardlinks either (FAT/exFAT), it copies, and asks once
> first. Choose explicitly with `--link-mode auto|symlink|hardlink|copy`.
>
> Caches written by older Windows builds (files only in `blobs/`) are
> repaired by `hfdownloader rebuild`.

### Flattened storage: real files in `models/` only

```bash
hfdownloader download TheBloke/Mistral-7B-Instruct-v0.2-GGUF --flatten
hfdownloader serve --flatten          # every download in the web UI; Docker: FLATTEN=true
```

Files download into `hub/` as usual, so an interrupted download resumes.
When **every** file is complete, they are moved into
`models/<owner>/<name>/` (or `datasets/...`) as real files — not links — and
the repo's `hub/` entry is deleted, so each file is stored exactly once, as
plain data you can open over SMB/NFS or copy anywhere:

```
~/.cache/huggingface/
└── models/
    └── TheBloke/
        └── Mistral-7B-Instruct-v0.2-GGUF/
            ├── mistral-7b-instruct-v0.2.Q4_K_M.gguf   # the real file
            └── hfd.yaml                               # storage: flat
```

Downloading the same repo again (or after the branch moved) only fetches new
or changed files; unchanged ones already in the folder are kept. The web UI
Cache browser, `list`, `info`, `export` and delete all work on flattened repos.

**Python:** there is no `hub/` entry, so load a flattened repo by its folder
path instead of its repo id — Transformers, Diffusers, llama.cpp and
`huggingface_hub` all accept a local directory:

```python
from transformers import AutoModelForCausalLM, AutoTokenizer

path = "/home/user/.cache/huggingface/models/Qwen/Qwen2.5-0.5B-Instruct"
model = AutoModelForCausalLM.from_pretrained(path)
tokenizer = AutoTokenizer.from_pretrained(path)
```

Trade-offs: only the last downloaded revision of a repo is kept, and files a
newer commit deleted stay in the folder until you remove them.

**Already downloaded?** Convert repos in `hub/` without downloading anything:

```bash
hfdownloader flatten TheBloke/Mistral-7B-Instruct-v0.2-GGUF   # one repo
hfdownloader flatten --all                                     # everything in hub/
```

or use **Convert to real files** on a repo in the web UI's Cache browser, or
let the server do it at every start with `serve --flatten --convert-on-boot`
(Docker: `FLATTEN=true` and `CONVERT_ON_BOOT=true`). The startup conversion
runs before any download resumes, skips repos with a queued, running or
paused download, and never deletes other revisions: repos that have them are
skipped and logged with the command that converts them anyway. If the server
is stopped meanwhile, it stops after the current repo; the rest are converted
on the next start. All
files cached for the kept revision (the last one downloaded, else `main`) are
moved; repos downloaded by Python's `huggingface_hub` work too. If a repo has
other revisions or an unfinished download cached, those would be deleted, so
it's skipped unless you pass `--yes` (the web UI asks). Files are staged
first and only then moved into place, so a failure (a full disk, say) leaves
the cache as it was. A repo with its own `hfd.yaml` at the root can't be
flattened, since the download manifest would overwrite it.

### Export: plain files from what you already downloaded

Need real files for LM Studio, Ollama or llama.cpp but already downloaded
into the cache? Export them — nothing is re-downloaded; the files are copied
into a folder of your choice (add `--mode hardlink` to share the cache's disk
space instead):

```bash
hfdownloader export TheBloke/Mistral-7B-Instruct-v0.2-GGUF ~/lmstudio/models/TheBloke/Mistral-7B
hfdownloader export unsloth/Qwen3-Coder-30B-A3B-Instruct-GGUF ./qwen -F q4_k_m   # just one quant
```

In the Web UI, start the server with `--export-dir <folder>` and use
**Export as real files** on any cached repo.

### Mode 2 — Flat files in a directory you choose

If you want **real files** at a path of your choice — no cache, no blob
hashes, no symlinks — use `--local-dir` (matching
`huggingface-cli download --local-dir`):

```bash
hfdownloader download TheBloke/Mistral-7B-Instruct-v0.2-GGUF \
    --local-dir ./my-model
# files land in ./my-model/TheBloke/Mistral-7B-Instruct-v0.2-GGUF/
```

Unlike `huggingface-cli`, the files go into an `<owner>/<name>/` subfolder of
the directory you pass. To put a repo's files directly into a folder of your
choice, download normally and use [`export`](#export-plain-files-from-what-you-already-downloaded).

This is the right mode for:

- Feeding files directly to llama.cpp, ollama, or any tool that expects a
  plain directory of weights.
- Windows users who don't want to enable Developer Mode.
- Sharing a model over NFS, SMB, or a USB drive — hardlinks and symlinks
  don't travel well; real files do.
- Air-gapped transfers and manual backups.

The v2.x-compatible spelling `--legacy -o <dir>` produces the **exact same
result** and is kept permanently for script compatibility:

```bash
hfdownloader download TheBloke/Mistral-7B-Instruct-v0.2-GGUF \
    --legacy -o ./my-model
```

Both spellings are interchangeable; pick whichever reads better in your
scripts. They are mutually exclusive on a single command line.

### Manifest Tracking

Every download creates `hfd.yaml` so you know exactly what you have:

```yaml
version: "1.0"
type: model
repo: TheBloke/Mistral-7B-Instruct-v0.2-GGUF
branch: main
commit: a1b2c3d4...
repo_path: hub/models--TheBloke--Mistral-7B-Instruct-v0.2-GGUF
started_at: 2024-01-15T10:25:12Z
completed_at: 2024-01-15T10:30:00Z
command: hfdownloader download TheBloke/Mistral-7B-Instruct-v0.2-GGUF -F q4_k_m
total_size: 4368438272
total_files: 1
files:
  - name: mistral-7b-instruct-v0.2.Q4_K_M.gguf
    blob: blobs/<sha256>
    size: 4368438272
    lfs: true
```

```bash
# List everything you've downloaded
hfdownloader list

# Get details about a specific download
hfdownloader info Mistral
```

---

## Web UI

A modern web interface with real-time progress:

```bash
hfdownloader serve
# Open http://localhost:8080
```

![Web Dashboard](docs/screenshots/web-dashboard.png)

### Cache Browser

Browse everything you've downloaded with stats, search, and filters:

![Cache Browser](docs/screenshots/web-dashboard2.png)

### All Pages

| Page | Features |
|------|----------|
| **Analyze** | Enter any repo, auto-detect type, see files/sizes, pick GGUF quantizations |
| **Jobs** | Real-time WebSocket progress, pause/resume/cancel, download history |
| **Cache** | Browse downloaded repos, disk usage stats, search & filter |
| **Mirror** | Configure targets, compare differences, push/pull sync |
| **Settings** | Token, connections, proxy, verification mode |

### Server Options

```bash
hfdownloader serve \
  --port 3000 \
  --auth-user admin \
  --auth-pass secret \
  -t hf_xxxxx
```

---

## Mirror Sync

Sync your model cache between machines — home, office, NAS, USB drive.

![Mirror Sync](docs/screenshots/web-dashboar3.png)

```bash
# Add mirror targets
hfdownloader mirror target add office /mnt/nas/hf-models
hfdownloader mirror target add usb /media/usb/hf-cache

# Compare local vs target
hfdownloader mirror diff office

# Push local cache to target
hfdownloader mirror push office

# Pull from target to local
hfdownloader mirror pull office

# Sync specific repos only (case-insensitive substring of owner/name)
hfdownloader mirror push office --repo Llama

# Verify integrity after sync
hfdownloader mirror push office --verify
```

Perfect for:
- **Air-gapped environments**: Download at home, sync to office
- **Team sharing**: Central NAS with all models
- **Backup**: Keep a copy on external drive

---

## Proxy Support

Full proxy support for corporate environments:

```bash
# HTTP proxy
hfdownloader download owner/repo --proxy http://proxy:8080

# SOCKS5 (e.g., SSH tunnel)
hfdownloader download owner/repo --proxy socks5://localhost:1080

# With authentication
hfdownloader download owner/repo \
  --proxy http://proxy:8080 \
  --proxy-user myuser \
  --proxy-pass mypassword

# Test proxy connectivity before downloading
hfdownloader proxy test --proxy http://proxy:8080
```

### Supported Types

| Type | URL Format |
|------|------------|
| HTTP | `http://host:port` |
| HTTPS | `https://host:port` |
| SOCKS5 | `socks5://host:port` |
| SOCKS5h | `socks5h://host:port` (remote DNS) |

### Configuration File

Save proxy settings in `~/.config/hfdownloader.yaml`:

```yaml
proxy:
  url: http://proxy.corp.com:8080
  username: myuser
  password: mypassword
  no_proxy: localhost,.internal.com,10.0.0.0/8
```

---

## Installation

### One-Liner (Recommended)

```bash
bash <(curl -sSL https://raw.githubusercontent.com/reuc/HuggingFaceModelDownloader/main/scripts/gist_gethfd.sh) install
```

That's it. Works on Linux, macOS, and WSL. Installs to `~/.local/bin` by
default — no sudo required. Pass an explicit path to install somewhere else:

```bash
bash <(curl -sSL https://raw.githubusercontent.com/reuc/HuggingFaceModelDownloader/main/scripts/gist_gethfd.sh) install /usr/local/bin   # system-wide
bash <(curl -sSL https://raw.githubusercontent.com/reuc/HuggingFaceModelDownloader/main/scripts/gist_gethfd.sh) install ~/bin            # custom
```

**Or run without installing:**

```bash
bash <(curl -sSL https://raw.githubusercontent.com/reuc/HuggingFaceModelDownloader/main/scripts/gist_gethfd.sh) download TheBloke/Mistral-7B-Instruct-v0.2-GGUF
bash <(curl -sSL https://raw.githubusercontent.com/reuc/HuggingFaceModelDownloader/main/scripts/gist_gethfd.sh) serve   # Web UI
```

### Download Binary

Get from [Releases](https://github.com/reuc/HuggingFaceModelDownloader/releases):

| Platform | Architecture | File |
|----------|--------------|------|
| Linux | x86_64 | `hfdownloader_linux_amd64_*` |
| Linux | ARM64 | `hfdownloader_linux_arm64_*` |
| macOS | Apple Silicon | `hfdownloader_darwin_arm64_*` |
| macOS | Intel | `hfdownloader_darwin_amd64_*` |
| Windows | x86_64 | `hfdownloader_windows_amd64_*.exe` |

#### Verify a download

Each release has a `checksums.txt` with the SHA-256 of every binary. Releases built by the current release workflow also include SLSA provenance, `hfdownloader_<tag>.intoto.jsonl`. It proves a binary was built by this repository's release workflow from that tag, not uploaded by hand. Older releases don't have it.

```bash
TAG=v3.4.0   # the release you downloaded
FILE=hfdownloader_linux_amd64_${TAG}

# Checksum (macOS: shasum -a 256 -c)
sha256sum --ignore-missing -c checksums.txt

# Provenance (https://github.com/slsa-framework/slsa-verifier)
slsa-verifier verify-artifact "$FILE" \
  --provenance-path "hfdownloader_${TAG}.intoto.jsonl" \
  --source-uri github.com/reuc/HuggingFaceModelDownloader \
  --source-tag "$TAG"
```

### Build from Source

```bash
git clone https://github.com/reuc/HuggingFaceModelDownloader
cd HuggingFaceModelDownloader
go build -o hfdownloader ./cmd/hfdownloader
```

### Docker

```bash
# Pull from Docker Hub
docker pull reuc/hfdownloader:latest

# Or build locally
docker build -t hfdownloader .

# Run (mounts your local HF cache)
docker run --rm -v ~/.cache/huggingface:/home/hfdownloader/.cache/huggingface \
  reuc/hfdownloader download TheBloke/Mistral-7B-Instruct-v0.2-GGUF
```

#### Docker environment variables

| Variable | Default | Description |
|---|---|---|
| `HTTP_PORT` | `8080` | Web server port for `serve` (same as `--port`) |
| `AUTH_USER` | unset | Basic-auth username for `serve`, and the SSH login name |
| `AUTH_PASS` | unset | Basic-auth password for `serve`, and the SSH password |
| `SSH_ENABLE` | `false` | Start an SSH/SFTP server in the container (`true`/`false`) |
| `SSH_PORT` | `22` | SSH server port, used only when `SSH_ENABLE=true` |
| `FLATTEN` | unset | Store finished downloads only as real files in `models/` / `datasets/` (same as `serve --flatten`). When set to `true` or `false` it overrides `flatten` in the config file |
| `CONVERT_ON_BOOT` | unset | At startup, convert every completed download already in `hub/` to real files (same as `serve --convert-on-boot`; use with `FLATTEN=true`). Set/unset like `FLATTEN` |

`AUTH_USER` and `AUTH_PASS` must be set together, and `SSH_ENABLE` requires both.
Flags passed on the command line override these variables.

```bash
# Web UI on port 3000 with login, plus SSH/SFTP on port 2222
docker run -d -p 3000:3000 -p 2222:2222 \
  -e HTTP_PORT=3000 -e SSH_ENABLE=true -e SSH_PORT=2222 \
  -e AUTH_USER=admin -e AUTH_PASS=secret \
  -v ~/.cache/huggingface:/home/hfdownloader/.cache/huggingface \
  reuc/hfdownloader serve

# Copy models out over SSH
scp -P 2222 -r admin@localhost:.cache/huggingface/hub ./hub
```

The SSH login runs as the same user as the app, so copied files keep the right owner.
The container starts as root only to set up SSH, then drops to the `hfdownloader` user.
SSH host keys are generated on first start in `/etc/ssh/keys`; mount a volume there to keep them across container re-creation.

#### What's in the image

The image is built on [Wolfi](https://github.com/wolfi-dev) and has no package manager:

- **Build stage:** the distroless variant of [`wolfi-polyglot`](https://github.com/reuc/wolfi-polyglot), a Go toolchain with no shell, compiles a static `hfdownloader` for the target platform.
- **Runtime:** an empty (`scratch`) image with only the Wolfi packages the container uses: busybox (for the entrypoint script and SSH sessions), OpenSSH, su-exec, the CA bundle and tzdata. There are no setuid binaries.
- The app runs as `hfdownloader`, uid/gid **1000**. Give that id write access to mounted volumes, or run as another id with `--user` (SSH then stays off).

Both base images are pinned by digest in the [`Dockerfile`](Dockerfile).

To build against a local `wolfi-polyglot` instead of the published one, for example to test a change to it, override the build stage's image with a named build context. The name on the left must match the build stage's `FROM` reference exactly:

```bash
docker buildx build --load \
  --build-context docker.io/reuc/wolfi-polyglot:<tag>-distroless@sha256:<digest>=docker-image://wolfi-polyglot:<local-tag>-distroless \
  -t hfdownloader .
```

#### CVE gate and smoke test

Every pull request, every push to `main` and a monthly run (06:17 UTC on the 1st) build the image natively on amd64 and arm64 (`.github/workflows/docker-build-and-scan.yml`). Each build must pass:

- **`scripts/docker-smoke-test.sh`:** CLI and help, the image hardening, `serve` with the environment variables and basic auth, the drop to uid 1000, `--user 568:568`, and SSH/SFTP login with `SSH_ENABLE=true`.
- **`scripts/cve-gate.sh`:** a [grype](https://github.com/anchore/grype) scan that fails above **0 Critical, 2 High, 4 Medium or 9 Low** unique CVEs.

Each run's summary lists the findings, and the grype report is uploaded as an artifact. To make a failing gate block merges, require the **`docker-build-and-scan (all)`** check in the branch protection rule for `main`. For a reviewed false positive, add a [grype ignore rule](https://github.com/anchore/grype#specifying-matches-to-ignore) with a justification to `.grype.yaml`; don't raise the limits.

Run both locally (needs grype and jq):

```bash
docker build -t hfdownloader:local .
scripts/docker-smoke-test.sh hfdownloader:local
scripts/cve-gate.sh hfdownloader:local
```

#### Published images

`.github/workflows/docker-publish.yml` publishes **`docker.io/reuc/hfdownloader`** for `linux/amd64` and `linux/arm64`. It runs when a release is published, or on demand (**Actions → Docker Build & Publish → Run workflow**). Each architecture must pass the smoke test and the CVE gate first. Only the exact image that passed is pushed, and if either architecture fails, nothing is published.

| Published from | Tags |
|---|---|
| release `vX.Y.Z` | `X.Y.Z`, `X.Y`, `X`, `latest`, `sha-<commit>` (a pre-release gets only its version and `sha-<commit>`) |
| manual run | the `tag` input (default `latest`), `sha-<commit>` |

Each per-architecture image gets an SBOM attestation and a cosign signature. The multi-arch image gets SLSA build provenance and a cosign signature. For deployments, pin by digest; it's shown in each run's summary. Verify an image before using it:

```bash
IMAGE=docker.io/reuc/hfdownloader@sha256:<digest>
cosign verify "$IMAGE" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github.com/reuc/HuggingFaceModelDownloader/\.github/workflows/docker-publish\.yml@'
gh attestation verify "oci://$IMAGE" -R reuc/HuggingFaceModelDownloader
```

The workflow needs two repository settings: `DOCKER_USER` (the Docker Hub namespace, as a variable or a secret) and `DOCKER_TOKEN` (a Docker Hub access token with Read & Write scope).

#### TrueNAS SCALE

A ready-made app file is in [`deploy/truenas/hfdownloader.yaml`](deploy/truenas/hfdownloader.yaml).
In TrueNAS SCALE 24.10 or newer, open **Apps > Discover Apps > Install via YAML** and paste it in.
Edit the dataset paths and login first; the comments at the top of the file list each value to change.

---

## Private & Gated Models

For private repos or gated models (Llama, etc.):

```bash
# Set token via environment
export HF_TOKEN=hf_xxxxx
hfdownloader download meta-llama/Llama-2-7b

# Or via flag
hfdownloader download meta-llama/Llama-2-7b -t hf_xxxxx
```

For gated models, you must first accept the license on the model's HuggingFace page.

---

## China Mirror

Use the HuggingFace mirror for faster downloads in China:

```bash
hfdownloader download owner/repo --endpoint https://hf-mirror.com
```

Or set in config file:

```yaml
endpoint: https://hf-mirror.com
```

---

## CLI Reference

| Command | Description |
|---------|-------------|
| `download` | Download models or datasets (`hfdownloader download owner/name`; the subcommand is required) |
| `analyze` | Analyze repository before downloading |
| `serve` | Start web server with REST API |
| `list` | List all downloaded repos |
| `info` | Show details about a downloaded repo |
| `rebuild` | Regenerate friendly view from HF cache |
| `export` | Export a downloaded repo as plain files (no re-download) |
| `mirror` | Sync cache between locations |
| `proxy` | Test and show proxy configuration |
| `config` | Manage configuration |
| `version` | Show version info |

Full documentation: [docs/CLI.md](docs/CLI.md) • [docs/API.md](docs/API.md) • [docs/V3_FEATURES.md](docs/V3_FEATURES.md)

---

## What's New in v3.0

| Feature | Description |
|---------|-------------|
| **HF Cache Compatibility** | Downloads use standard HuggingFace cache structure by default (see [Storage Modes](#storage-modes)) |
| **`--local-dir` flag** | One-flag opt-in to flat files at a path of your choice — `huggingface-cli`-style |
| **Dual-Layer Storage** | Python-compatible cache + human-readable symlinks |
| **Smart Analyzer** | Auto-detect model types, GGUF quality ratings, RAM estimates |
| **Web UI v3** | Modern interface with real-time WebSocket progress |
| **Mirror Sync** | Push/pull cache between locations |
| **Full Proxy Support** | HTTP, SOCKS5, authentication, CIDR bypass |
| **Manifest Tracking** | `hfd.yaml` records what/when/how for every download |

Both storage modes (HF cache and flat-file `--local-dir` / `--legacy -o`)
are fully supported and permanent — neither is deprecated. See
[Storage Modes](#storage-modes) for when to pick which.

---

## Environment Variables

| Variable | Purpose |
|----------|---------|
| `HF_TOKEN` | HuggingFace access token |
| `HF_HOME` | Override `~/.cache/huggingface` |
| `HF_HUB_CACHE` | Override just the `hub/` directory |
| `HTTP_PROXY` | Proxy for HTTP requests |
| `HTTPS_PROXY` | Proxy for HTTPS requests |
| `NO_PROXY` | Comma-separated bypass list |

---

## License

[Apache 2.0](LICENSE) — use freely in personal and commercial projects.

---

<div align="center">

**[Full CLI Docs](docs/CLI.md)** • **[REST API](docs/API.md)** • **[V3 Features](docs/V3_FEATURES.md)** • **[Issues](https://github.com/reuc/HuggingFaceModelDownloader/issues)**

</div>
