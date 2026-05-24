#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GO_BIN="${GO_BIN:-go}"
CREATED_WORK_DIR=""
if [[ -n "${GITMOOT_PLUGIN_SMOKE_DIR:-}" ]]; then
  WORK_DIR="$GITMOOT_PLUGIN_SMOKE_DIR"
else
  WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/gitmoot-plugin-smoke.XXXXXX")"
  CREATED_WORK_DIR=1
fi
KEEP_WORK_DIR="${GITMOOT_PLUGIN_SMOKE_KEEP:-}"

cleanup() {
  if [[ -n "$CREATED_WORK_DIR" && -z "$KEEP_WORK_DIR" && -d "$WORK_DIR" ]]; then
    rm -rf "$WORK_DIR"
  fi
}
trap cleanup EXIT

mkdir -p "$WORK_DIR"

BIN="${GITMOOT_BIN:-$WORK_DIR/gitmoot}"
SMOKE_USER_HOME="$WORK_DIR/user-home"
RUNTIME_HOME="$WORK_DIR/runtime-home"
CODEX_OUT="$WORK_DIR/codex-plugin"
CLAUDE_OUT="$WORK_DIR/claude-plugin"

rm -rf "$SMOKE_USER_HOME" "$RUNTIME_HOME" "$CODEX_OUT" "$CLAUDE_OUT"
mkdir -p "$SMOKE_USER_HOME" "$RUNTIME_HOME"

if [[ -z "${GITMOOT_BIN:-}" ]]; then
  (cd "$ROOT_DIR" && "$GO_BIN" build -o "$BIN" ./cmd/gitmoot)
fi

"$BIN" init --home "$SMOKE_USER_HOME"
"$BIN" plugin build codex --out "$CODEX_OUT"
"$BIN" plugin build claude --out "$CLAUDE_OUT"

HAS_CLAUDE=""
HAS_CODEX=""
if command -v claude >/dev/null 2>&1; then
  HAS_CLAUDE=1
  claude plugin validate "$CLAUDE_OUT"
else
  echo "skip: claude CLI not found; package validation skipped"
fi

if command -v codex >/dev/null 2>&1; then
  HAS_CODEX=1
  HOME="$RUNTIME_HOME" codex plugin marketplace list >/dev/null
  HOME="$RUNTIME_HOME" codex plugin list >/dev/null
else
  echo "skip: codex CLI not found; non-mutating codex plugin checks skipped"
fi

"$BIN" plugin build codex --home "$SMOKE_USER_HOME"
"$BIN" plugin build claude --home "$SMOKE_USER_HOME"

if command -v codex >/dev/null 2>&1; then
  "$BIN" plugin doctor codex --home "$SMOKE_USER_HOME"
else
  "$BIN" plugin doctor codex --home "$SMOKE_USER_HOME" || true
fi

if command -v claude >/dev/null 2>&1; then
  "$BIN" plugin doctor claude --home "$SMOKE_USER_HOME"
else
  "$BIN" plugin doctor claude --home "$SMOKE_USER_HOME" || true
fi

HOME="$RUNTIME_HOME" "$BIN" plugin install codex --home "$SMOKE_USER_HOME" --force
HOME="$RUNTIME_HOME" "$BIN" plugin install claude --home "$SMOKE_USER_HOME" --scope user --force
if [[ -n "$HAS_CODEX$HAS_CLAUDE" ]]; then
  "$BIN" plugin doctor --home "$SMOKE_USER_HOME"
else
  "$BIN" plugin doctor --home "$SMOKE_USER_HOME" || true
fi

test -f "$SMOKE_USER_HOME/.gitmoot/plugins/build/codex/gitmoot/skills/gitmoot/SKILL.md"
test -f "$SMOKE_USER_HOME/.gitmoot/plugins/build/claude/gitmoot/skills/gitmoot/SKILL.md"
if [[ -n "$HAS_CODEX" ]]; then
  test -f "$RUNTIME_HOME/.codex/config.toml"
fi
if [[ -n "$HAS_CLAUDE" ]]; then
  test -f "$RUNTIME_HOME/.claude/plugins/installed_plugins.json"
fi

echo "plugin smoke passed"
echo "work dir: $WORK_DIR"
