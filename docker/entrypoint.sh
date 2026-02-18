#!/bin/bash
set -e

echo "=== CloudCode Instance Starting ==="

echo "[1/3] Updating OpenCode..."
bun update -g opencode-ai@latest 2>/dev/null || echo "Warning: OpenCode update failed, using existing version"
echo "  OpenCode version: $(opencode --version 2>/dev/null || echo 'unknown')"

echo "[2/3] Updating Oh My OpenCode..."
bun update -g oh-my-opencode@latest 2>/dev/null || echo "Warning: oh-my-opencode update failed, using existing version"

if [ -n "$GH_TOKEN" ]; then
    echo "[*] GitHub CLI authenticated via GH_TOKEN"
fi

# Config files are bind-mounted by the management platform:
#   /root/.config/opencode/      ← opencode.json, oh-my-opencode.json, skills/, commands/, etc.
#   /root/.local/share/opencode/ ← auth.json
#   /root/.opencode/             ← package.json

if [ -f /root/.config/opencode/opencode.json ]; then
    echo "[*] Global opencode config detected"
fi
if [ -f /root/.local/share/opencode/auth.json ]; then
    echo "[*] Global auth config detected"
fi
if [ -f /root/.config/opencode/oh-my-opencode.json ]; then
    echo "[*] Global oh-my-opencode config detected"
fi

PORT="${OPENCODE_PORT:-4096}"

echo "[3/3] Starting OpenCode Web UI on port ${PORT}..."
echo "=== Ready ==="

exec opencode web --port "${PORT}" --hostname 0.0.0.0
