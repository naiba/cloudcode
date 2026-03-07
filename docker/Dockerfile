FROM ubuntu:24.04

ARG TARGETARCH

# Allow timezone to be baked in at build time (e.g. --build-arg TZ=America/New_York).
# Defaults to UTC. Mirrors pattern from Anthropic's official Claude Code devcontainer.
ARG TZ=UTC
ENV TZ="$TZ"

# Avoid interactive prompts during package installation
ENV DEBIAN_FRONTEND=noninteractive

# Disable Bun's runtime transpiler cache — not useful in ephemeral containers
# (pattern from upstream opencode official Docker image)
ENV BUN_RUNTIME_TRANSPILER_CACHE_PATH=0

# Increase Node.js heap to avoid OOM on large codebases.
# Mirrors the NODE_OPTIONS setting in Anthropic's Claude Code devcontainer.
ENV NODE_OPTIONS="--max-old-space-size=4096"

# ── Layer 1: Base system packages (most stable, cached aggressively) ──────────
# ripgrep (rg)  — required by opencode's built-in search/grep tool
# fd-find       — fast file discovery (aliased as 'fd' below)
# fzf           — fuzzy file/history search used by oh-my-opencode shell integration
# zoxide        — smarter cd replacement used by oh-my-opencode
# zsh           — richer interactive shell (Anthropic devcontainer default)
# gnupg2        — GPG for signed commits and key management
# iproute2      — provides 'ip' command for network debugging inside containers
# dnsutils      — provides 'dig'/'nslookup' for DNS troubleshooting
# nano          — approachable editor alongside vim-tiny (from Anthropic devcontainer)
# sqlite3       — CLI for debugging CloudCode instance databases
# python3 / python3-pip / python3-venv — Python 3 (Ubuntu 24.04 ships 3.12);
#   pip and venv are the baseline; uv replaces pip for day-to-day use (see Layer 2a)
# python3-dev   — headers needed for compiling native Python extensions (e.g. numpy, psutil)
RUN for i in 1 2 3 4 5; do \
    apt-get update && \
    apt-get install -y --no-install-recommends \
        build-essential \
        ca-certificates \
        curl \
        dnsutils \
        fd-find \
        fzf \
        git \
        gnupg2 \
        iproute2 \
        jq \
        less \
        make \
        nano \
        openssh-client \
        procps \
        python3 \
        python3-dev \
        python3-pip \
        python3-venv \
        ripgrep \
        sqlite3 \
        tmux \
        tree \
        unzip \
        vim-tiny \
        wget \
        zoxide \
        zsh \
    && break || (apt-get install -y --fix-broken 2>/dev/null; sleep 10); \
    done \
    && rm -rf /var/lib/apt/lists/* \
    && ln -sf /usr/bin/python3 /usr/local/bin/python

# Symlink fd-find to 'fd' (Ubuntu package installs it as 'fdfind')
RUN ln -sf /usr/bin/fdfind /usr/local/bin/fd

# ── Layer 2a: uv — fast Python package and project manager ───────────────────
# uv replaces pip/virtualenv/pyenv for day-to-day Python work:
#   - 10–100x faster than pip for installs
#   - manages virtual environments, Python versions, and lock files
#   - `uv run`, `uv pip`, `uv venv`, `uv sync` all work out of the box
#   - opencode's bash tool benefits from uv being available for Python projects
ENV UV_CACHE_DIR=/root/.cache/uv
RUN curl -LsSf https://astral.sh/uv/install.sh | sh
# uv installs to ~/.local/bin by default
ENV PATH="/root/.local/bin:${PATH}"

# ── Layer 3: git-delta ────────────────────────────────────────────────────────
# delta is a syntax-highlighting pager for git diffs — opencode's bash tool and
# any git operations produce far more readable output with it installed.
# Sourced from Anthropic's Claude Code devcontainer (pinned version).
ARG GIT_DELTA_VERSION=0.18.2
RUN ARCH=$(dpkg --print-architecture) && \
    wget -q "https://github.com/dandavison/delta/releases/download/${GIT_DELTA_VERSION}/git-delta_${GIT_DELTA_VERSION}_${ARCH}.deb" && \
    dpkg -i "git-delta_${GIT_DELTA_VERSION}_${ARCH}.deb" && \
    rm "git-delta_${GIT_DELTA_VERSION}_${ARCH}.deb"

# ── Layer 4: GitHub CLI ───────────────────────────────────────────────────────
RUN curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
    | dd of=/usr/share/keyrings/githubcli-archive-keyring.gpg \
    && chmod go+r /usr/share/keyrings/githubcli-archive-keyring.gpg \
    && echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
    | tee /etc/apt/sources.list.d/github-cli.list > /dev/null \
    && for i in 1 2 3; do apt-get update && apt-get install -y gh && break || sleep 10; done \
    && rm -rf /var/lib/apt/lists/*

# ── Layer 5: Cloudflare Tunnel ────────────────────────────────────────────────
# cloudflared enables exposing local services to the internet via Cloudflare
RUN curl -fsSL https://pkg.cloudflare.com/cloudflare-main.gpg \
    | tee /usr/share/keyrings/cloudflare-main.gpg > /dev/null \
    && echo 'deb [signed-by=/usr/share/keyrings/cloudflare-main.gpg] https://pkg.cloudflare.com/cloudflared any main' \
    | tee /etc/apt/sources.list.d/cloudflared.list > /dev/null \
    && for i in 1 2 3; do apt-get update && apt-get install -y cloudflared && break || sleep 10; done \
    && rm -rf /var/lib/apt/lists/*

# ── Layer 6: Go ───────────────────────────────────────────────────────────────
# Pinned to latest Go 1.24 stable release
ENV GO_VERSION=1.24.2
RUN curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${TARGETARCH}.tar.gz" \
    | tar -C /usr/local -xzf -
ENV PATH="/usr/local/go/bin:/root/go/bin:${PATH}"

# ── Layer 7: Node.js LTS (v22) ────────────────────────────────────────────────
RUN curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
    && for i in 1 2 3; do apt-get install -y nodejs && break || sleep 10; done \
    && rm -rf /var/lib/apt/lists/*

# ── Layer 8: Bun ──────────────────────────────────────────────────────────────
# Bun is the primary JS runtime/package manager used by opencode and plugins
RUN curl -fsSL https://bun.sh/install | bash
ENV PATH="/root/.bun/bin:${PATH}"

# ── Layer 9: OpenCode and ecosystem packages ──────────────────────────────────
# Install all global bun packages in one layer to minimise image layers:
#   opencode-ai          — the core AI coding agent (web mode used for CloudCode)
#   oh-my-opencode       — plugin extending opencode with skills, tools, themes
#   @fission-ai/openspec — spec-driven development plugin for AI coding assistants
#
# oh-my-opencode-linux-arm64 is an optional arm64-specific binary helper;
# failure is silenced because it only applies on arm64 hosts.
RUN bun install -g \
        opencode-ai@latest \
        oh-my-opencode@latest \
        @fission-ai/openspec@latest \
    && bun install -g oh-my-opencode-linux-arm64@latest 2>/dev/null || true

# ── Layer 10: Playwright Chromium ─────────────────────────────────────────────
# Pre-install Chromium for browser automation skills.
# Split into two steps: binary download first (reliable network), then apt deps.
RUN npx playwright install chromium
RUN for i in 1 2 3 4 5; do npx playwright install-deps chromium && break || sleep 15; done
# Symlink to well-known paths so tools can find chromium without PLAYWRIGHT_BROWSERS_PATH.
# 'find' handles both chrome-linux/ (arm64) and chrome-linux64/ (amd64) directory names.
RUN CHROME_BIN=$(find /root/.cache/ms-playwright/chromium-* -name chrome -type f | head -1) \
    && ln -sf "$CHROME_BIN" /usr/bin/chromium-browser \
    && ln -sf "$CHROME_BIN" /usr/bin/chrome

# ── Layer 11: Directory structure ─────────────────────────────────────────────
# Create directories expected by opencode and CloudCode bind mounts.
# Per AGENTS.md config layout:
#   /root/.config/opencode/          ← opencode.json, commands/, agents/, plugins/
#   /root/.local/share/opencode/     ← session data (per-instance volume)
#   /root/.opencode/                 ← package.json for dot-opencode
#   /root/.agents/skills             ← skills.sh managed skills (shared bind mount)
RUN mkdir -p \
    /workspace \
    /root/.opencode \
    /root/.config/opencode \
    /root/.local/share/opencode \
    /root/.agents/skills

# ── Layer 12: Entrypoint ──────────────────────────────────────────────────────
COPY entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh

WORKDIR /workspace

EXPOSE 4096

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
