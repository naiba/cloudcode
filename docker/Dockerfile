FROM ubuntu:24.04

ARG TARGETARCH

# Avoid interactive prompts during package installation
ENV DEBIAN_FRONTEND=noninteractive

# Install base system dependencies and dev tools (stable layer, cached aggressively)
RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential \
    ca-certificates \
    curl \
    git \
    jq \
    less \
    make \
    openssh-client \
    procps \
    python3-pip \
    tree \
    unzip \
    vim-tiny \
    tini \
    tmux \
    wget

# Install GitHub CLI
RUN curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
    | dd of=/usr/share/keyrings/githubcli-archive-keyring.gpg \
    && chmod go+r /usr/share/keyrings/githubcli-archive-keyring.gpg \
    && echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
    | tee /etc/apt/sources.list.d/github-cli.list > /dev/null \
    && apt-get update \
    && apt-get install -y gh


# Install cloudflared (Cloudflare Tunnel, for exposing local services to the internet)
RUN curl -fsSL https://pkg.cloudflare.com/cloudflare-main.gpg \
    | tee /usr/share/keyrings/cloudflare-main.gpg > /dev/null \
    && echo 'deb [signed-by=/usr/share/keyrings/cloudflare-main.gpg] https://pkg.cloudflare.com/cloudflared any main' \
    | tee /etc/apt/sources.list.d/cloudflared.list > /dev/null \
    && apt-get update \
    && apt-get install -y cloudflared

# Install Go
ENV GO_VERSION=1.23.6
RUN curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${TARGETARCH}.tar.gz" \
    | tar -C /usr/local -xzf -
ENV PATH="/usr/local/go/bin:/root/go/bin:${PATH}"

# Install Node.js (LTS) via NodeSource
RUN curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
    && apt-get install -y nodejs

# Install Bun
RUN curl -fsSL https://bun.sh/install | bash
ENV PATH="/root/.bun/bin:${PATH}"

# Install OpenCode
RUN bun install -g opencode-ai@latest

# Install Oh My OpenCode (as opencode plugin)
RUN bun install -g oh-my-opencode@latest
RUN bun install -g oh-my-opencode-linux-arm64@latest || true

# Install OpenSpec (spec-driven development for AI coding assistants)
RUN bun install -g @fission-ai/openspec@latest

# Install Pinchtab (browser control for AI agents)
RUN bun install -g pinchtab@latest

# Chromium + system deps (libatk, libnss3, libgbm, etc.) — required by pinchtab browser server
RUN npx playwright install --with-deps chromium
# chromium-browser: --no-sandbox wrapper (root in container requires it)
RUN CHROME_BIN=$(find /root/.cache/ms-playwright/chromium-* -name chrome -type f | head -1) \
    && ln -sf "$CHROME_BIN" /usr/bin/cloudcode-browser \
    && printf '#!/bin/sh\nexec cloudcode-browser --no-sandbox "$@"\n' > /usr/bin/chromium-browser \
    && chmod +x /usr/bin/chromium-browser

# Create workspace directory
RUN mkdir -p /workspace /root/.opencode /root/.config/opencode /root/.local/share/opencode /root/.agents/skills

# Copy entrypoint script
COPY entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh

WORKDIR /workspace

EXPOSE 4096

# tini 作为 PID 1：正确转发信号给子进程组，并回收孤儿/僵尸进程。
# 没有它，opencode 作为 PID 1 不会 wait() 孤儿进程（如 chromium、git），导致僵尸堆积。
ENTRYPOINT ["tini", "--", "/usr/local/bin/entrypoint.sh"]
