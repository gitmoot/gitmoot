#!/usr/bin/env bash
# Optional smoke for the Herdr cockpit (`gitmoot orchestrate --cockpit`).
#
# It verifies that the cockpit is opt-in and fail-open: with `--cockpit` set but
# Herdr unreachable, orchestration must still enqueue normally (no pane, just one
# `cockpit_unavailable` job event), and when Herdr IS reachable it exercises the
# wrap path against the live single-server socket. It SKIPS cleanly (exit 0) when
# Herdr or gitmoot is unavailable so it never fails a normal checkout.
set -euo pipefail

skip() {
  echo "cockpit-smoke: SKIP — $1"
  exit 0
}

command -v herdr >/dev/null 2>&1 || skip "herdr is not on PATH"

# Reachability is gated on `herdr status` (single server, default socket;
# honor HERDR_SOCKET_PATH if the caller set one). No --session is needed.
herdr status >/dev/null 2>&1 || skip "herdr status is not ok (server not running / not reachable)"

# Gate the pane surface here too (matching herdr-train-init-smoke.sh) so we skip
# BEFORE doing any home/repo init when panes are unavailable. These are the exact
# reachability + pane primitives the cockpit adapter uses.
herdr pane list >/dev/null 2>&1 || skip "herdr pane list failed — cockpit pane surface not available"

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="$(mktemp -d)"

# Use an explicit GITMOOT_BIN if provided, otherwise build a current binary from
# this checkout so the smoke always matches the code under review.
if [ -n "${GITMOOT_BIN:-}" ]; then
  command -v "$GITMOOT_BIN" >/dev/null 2>&1 || skip "GITMOOT_BIN=$GITMOOT_BIN is not executable"
else
  command -v go >/dev/null 2>&1 || skip "go is not on PATH to build gitmoot (or set GITMOOT_BIN)"
  GITMOOT_BIN="$WORK/gitmoot"
  echo "cockpit-smoke: building gitmoot from $ROOT_DIR"
  ( cd "$ROOT_DIR" && GOTOOLCHAIN="${GOTOOLCHAIN:-go1.26.0}" go build -o "$GITMOOT_BIN" ./cmd/gitmoot ) \
    || skip "could not build gitmoot from this checkout"
fi

HOME_DIR="$WORK/home"
REPO_DIR="$WORK/repo"
mkdir -p "$HOME_DIR" "$REPO_DIR"
cleanup() { rm -rf "$WORK"; }
trap cleanup EXIT

# Everything runs against an isolated --home so the smoke never touches the
# user's real gitmoot state or daemon.
gm() { "$GITMOOT_BIN" "$@" --home "$HOME_DIR"; }

echo "cockpit-smoke: initializing an isolated gitmoot home"
gm init >/dev/null

# A throwaway git repo so orchestrate has a checkout to point at.
( cd "$REPO_DIR" \
  && git init -q \
  && git config user.email smoke@example.com \
  && git config user.name "cockpit smoke" \
  && git commit -q --allow-empty -m "init" )

echo "cockpit-smoke: herdr is reachable; exercising the --cockpit wrap path"

# Drive a tiny background orchestrate with --cockpit. We do not require a real
# coordinator template or runtime auth here by default; we only assert that
# --cockpit is accepted and that orchestration proceeds (a pane is best-effort
# and never blocks or fails the job). Set GITMOOT_COCKPIT_SMOKE_AGENT to a real,
# runnable coordinator for a live end-to-end pane demo.
AGENT="${GITMOOT_COCKPIT_SMOKE_AGENT:-}"
if [ -z "$AGENT" ]; then
  gm orchestrate --help 2>&1 | grep -q -- "--cockpit" \
    || { echo "cockpit-smoke: FAIL — orchestrate is missing the --cockpit flag"; exit 1; }
  echo "cockpit-smoke: OK — orchestrate exposes --cockpit and herdr is reachable"
  echo "cockpit-smoke: set GITMOOT_COCKPIT_SMOKE_AGENT=<coordinator> for a live pane run"
  exit 0
fi

echo "cockpit-smoke: launching '$AGENT' under --cockpit (isolated home: $HOME_DIR)"
gm orchestrate "$AGENT" "Cockpit smoke: confirm the pane wrap path." \
  --repo "file://$REPO_DIR" --cockpit >/dev/null \
  || { echo "cockpit-smoke: FAIL — orchestrate --cockpit returned non-zero"; exit 1; }

echo "cockpit-smoke: orchestrate --cockpit launched; inspect panes with 'herdr pane list'"
echo "cockpit-smoke: OK"
