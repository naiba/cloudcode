#!/bin/bash
# Stricter error handling: exit on error (-e), undefined variables (-u),
# and pipeline failures (-o pipefail). Mirrors Anthropic's Claude Code devcontainer.
set -euo pipefail

echo "=== CloudCode Instance Starting ==="

echo "[1/5] Updating OpenCode..."
bun update -g opencode-ai@latest 2>/dev/null || echo "Warning: OpenCode update failed, using existing version"
echo "  OpenCode version: $(opencode --version 2>/dev/null || echo 'unknown')"

echo "[2/5] Updating Oh My OpenCode..."
bun update -g oh-my-opencode@latest 2>/dev/null || echo "Warning: oh-my-opencode update failed, using existing version"
bun update -g oh-my-opencode-linux-arm64@latest 2>/dev/null || true

echo "[3/5] Updating OpenSpec..."
bun update -g @fission-ai/openspec@latest 2>/dev/null || echo "Warning: OpenSpec update failed, using existing version"

echo "[4/5] Updating skills.sh skills..."
# .skill-lock.json is bind-mounted at /root/.agents/ so updates are shared across instances
bunx skills update -g -y 2>/dev/null || echo "Warning: skills update failed or no skills installed"
if [ -n "${GH_TOKEN:-}" ]; then
    echo "[*] GitHub CLI authenticated via GH_TOKEN"
fi

# Config files are bind-mounted by the management platform:
#   /root/.config/opencode/           ← opencode.json, oh-my-opencode.json, commands/, agents/, plugins/
#   /root/.local/share/opencode/      ← session data (per-instance)
#   /root/.local/share/opencode/auth.json ← auth tokens (global, shared across all instances)
#   /root/.opencode/                  ← package.json

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

# Run user-defined startup script if present (bind-mounted from platform settings).
if [ -f /root/.config/cloudcode/startup.sh ]; then
    echo "[*] Running startup script..."
    bash /root/.config/cloudcode/startup.sh
    echo "[*] Startup script complete"
fi

echo "[5/5] Starting OpenCode Web UI on port ${PORT}..."
echo "=== Ready ==="

exec opencode web --port "${PORT}" --hostname 0.0.0.0
