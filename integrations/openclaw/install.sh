#!/bin/sh
# Install the memini memory-slot extension into OpenClaw.
#
#   curl -fsSL https://raw.githubusercontent.com/eleboucher/memini/main/integrations/openclaw/install.sh | sh
#
# Overrides (env): MEMINI_REPO_RAW, MEMINI_REF, OPENCLAW_HOME, DEST
set -eu

REPO="${MEMINI_REPO_RAW:-https://raw.githubusercontent.com/eleboucher/memini}"
REF="${MEMINI_REF:-main}"
OPENCLAW_HOME="${OPENCLAW_HOME:-$HOME/.openclaw}"
DEST="${DEST:-$OPENCLAW_HOME/extensions/memini}"
BASE="$REPO/$REF/integrations/openclaw/plugin"

echo "Installing memini OpenClaw extension -> $DEST"
mkdir -p "$DEST"
for f in plugin.mjs openclaw.plugin.json package.json plugin.yaml; do
  curl -fsSL "$BASE/$f" -o "$DEST/$f"
  echo "  fetched $f"
done

cat <<EOF

Done. Claim the memory slot in $OPENCLAW_HOME/openclaw.json:

  {
    "plugins": {
      "slots": { "memory": "memini" },
      "entries": {
        "memini": {
          "enabled": true,
          "config": { "base_url": "http://localhost:8080", "namespace": "openclaw" }
        }
      }
    }
  }

If memini needs auth, set MEMINI_API_KEY in the gateway env, then restart OpenClaw.
EOF
