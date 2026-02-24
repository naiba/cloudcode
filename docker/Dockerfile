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
    tree \
    unzip \
    vim-tiny \
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

# Create workspace directory
RUN mkdir -p /workspace /root/.opencode /root/.config/opencode /root/.local/share/opencode /root/.agents/skills

# Copy entrypoint script
COPY entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh

WORKDIR /workspace

EXPOSE 4096

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
