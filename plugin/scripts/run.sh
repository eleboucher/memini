#!/bin/sh
# Resolve node when version managers (mise, fnm, nvm, asdf, volta) aren't on
# the default /bin/sh PATH — the environment Claude Code hooks run under.
set -e

find_node() {
  command -v node >/dev/null 2>&1 && return 0

  # mise
  if command -v mise >/dev/null 2>&1; then
    eval "$(mise env 2>/dev/null)" && command -v node >/dev/null 2>&1 && return 0
  fi
  for d in "$HOME/.local/bin" "$HOME/.local/share/mise/bin"; do
    if [ -x "$d/mise" ]; then
      eval "$("$d/mise" env 2>/dev/null)" && command -v node >/dev/null 2>&1 && return 0
    fi
  done

  # fnm
  if [ -d "$HOME/.local/share/fnm" ]; then
    eval "$(HOME/.local/share/fnm/fnm env 2>/dev/null)" && command -v node >/dev/null 2>&1 && return 0
  fi
  if [ -d "$HOME/Library/Application Support/fnm" ]; then
    eval "$("$HOME/Library/Application Support/fnm/fnm" env 2>/dev/null)" && command -v node >/dev/null 2>&1 && return 0
  fi

  # nvm
  if [ -s "$HOME/.nvm/nvm.sh" ]; then
    . "$HOME/.nvm/nvm.sh" && command -v node >/dev/null 2>&1 && return 0
  fi

  # volta
  if [ -d "$HOME/.volta" ]; then
    PATH="$HOME/.volta/bin:$PATH" && command -v node >/dev/null 2>&1 && return 0
  fi

  # asdf
  if [ -d "$HOME/.asdf" ]; then
    . "$HOME/.asdf/asdf.sh" 2>/dev/null && command -v node >/dev/null 2>&1 && return 0
  fi

  # Homebrew (macOS)
  for d in /opt/homebrew/bin /usr/local/bin; do
    if [ -x "$d/node" ]; then
      PATH="$d:$PATH" && return 0
    fi
  done

  return 1
}

if ! find_node; then
  echo "memini: node not found — install Node.js or check your version manager setup" >&2
  exit 1
fi

exec node "$@"
