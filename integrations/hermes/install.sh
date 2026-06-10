#!/bin/sh
# Install the memini memory provider plugin into a Hermes Agent.
#
#   curl -fsSL https://raw.githubusercontent.com/eleboucher/memini/main/integrations/hermes/install.sh | sh
#
# Overrides (env): MEMINI_REPO_RAW, MEMINI_REF, HERMES_HOME, DEST
set -eu

REPO="${MEMINI_REPO_RAW:-https://raw.githubusercontent.com/eleboucher/memini}"
REF="${MEMINI_REF:-main}"
HERMES_HOME="${HERMES_HOME:-$HOME/.hermes}"
DEST="${DEST:-$HERMES_HOME/plugins/memini}"
BASE="$REPO/$REF/integrations/hermes/plugin/memini"

echo "Installing memini Hermes plugin -> $DEST"
mkdir -p "$DEST"
for f in __init__.py plugin.yaml; do
  curl -fsSL "$BASE/$f" -o "$DEST/$f"
  echo "  fetched $f"
done

cat <<EOF

Done. Activate it in $HERMES_HOME/config.yaml:

  plugins:
    enabled:
      - memini

Then point it at your memini with MEMINI_URL (default http://localhost:8080),
optionally MEMINI_NAMESPACE / MEMINI_API_KEY, and restart Hermes.

For Kubernetes (bjw-s app-template) deployments, see kubernetes.md instead.
EOF
