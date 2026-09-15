#!/usr/bin/env bash
# Wrapper around ./whatsapp-bridge for launchd (and manual pairing).
#
# Two modes:
#   ./run-bridge.sh          Normal daemon start. Refuses to run the binary
#                             at all if the store isn't paired (no device
#                             row) or has been logged out (LOGGED_OUT
#                             sentinel present) -- exits 0 either way, so
#                             launchd's KeepAlive (SuccessfulExit: false)
#                             does not restart-loop an unpaired machine.
#   ./run-bridge.sh --pair   One-time interactive pairing: archives any
#                             existing (invalid) session, runs the bridge in
#                             the foreground so a QR code prints, and clears
#                             the LOGGED_OUT sentinel once pairing succeeds.
#
# See RECOVERY.md for the operational story this exists to support.

set -euo pipefail
umask 077

cd "$(dirname "${BASH_SOURCE[0]}")"

STORE_DIR="${WA_STORE_DIR:-store}"
DEVICE_DB="$STORE_DIR/whatsapp.db"
LOGGED_OUT_SENTINEL="$STORE_DIR/LOGGED_OUT"
MESSAGES_DB="$STORE_DIR/messages.db"

# Tighten permissions on anything already there. Directories need +x to be
# traversable; files (the SQLite stores, downloaded media) do not.
if [ -d "$STORE_DIR" ]; then
  find "$STORE_DIR" -type d -exec chmod 700 {} \;
  find "$STORE_DIR" -type f -exec chmod 600 {} \;
fi

# Record an event to bridge_events if messages.db exists to record it into.
# Best-effort: the bridge itself is the source of truth for this table, this
# is only for the "unpaired"/"started" transitions that happen before the Go
# binary ever runs.
record_event() {
  local kind="$1"
  if [ -f "$MESSAGES_DB" ] && command -v sqlite3 >/dev/null 2>&1; then
    sqlite3 "$MESSAGES_DB" \
      "INSERT INTO bridge_events (ts, kind, message_count) VALUES (strftime('%s','now'), '$kind', (SELECT COUNT(*) FROM messages));" \
      2>/dev/null || true
  fi
  echo "BRIDGE_EVENT $(date -u +%Y-%m-%dT%H:%M:%SZ) $kind"
}

PAIR_CHECK_ERRORED=0

is_paired() {
  [ -f "$DEVICE_DB" ] || return 1
  [ ! -f "$LOGGED_OUT_SENTINEL" ] || return 1
  local count
  if ! count=$(sqlite3 "$DEVICE_DB" "SELECT COUNT(*) FROM whatsmeow_device;" 2>/dev/null); then
    # Bug fixed 2026-09-15: the old `|| echo 0` fallback made a QUERY
    # ERROR (e.g. a transient exclusive lock from a concurrent writer --
    # this DB is journal_mode=delete, not WAL) indistinguishable from a
    # genuine zero-row table. main() then recorded "unpaired" into
    # bridge_events -- a trial-fatal event for a downstream uptime
    # verdict, even though nothing about pairing had actually changed.
    # Signal "indeterminate" distinctly so the caller retries instead of
    # persisting false evidence.
    PAIR_CHECK_ERRORED=1
    return 1
  fi
  [ "${count:-0}" -gt 0 ]
}

do_pair() {
  echo "=== WhatsApp bridge: pairing ==="

  if [ -f "$DEVICE_DB" ] || [ -f "$LOGGED_OUT_SENTINEL" ]; then
    local archive_dir="$STORE_DIR/archive-$(date -u +%Y%m%dT%H%M%SZ)"
    mkdir -p "$archive_dir"
    echo "Archiving existing session to $archive_dir/ before re-pairing."
    for f in "$STORE_DIR"/whatsapp.db "$STORE_DIR"/whatsapp.db-wal "$STORE_DIR"/whatsapp.db-shm; do
      [ -f "$f" ] && mv "$f" "$archive_dir/"
    done
    rm -f "$LOGGED_OUT_SENTINEL"
  fi

  mkdir -p "$STORE_DIR"
  chmod 700 "$STORE_DIR"

  echo "Starting the bridge in the foreground. Scan the QR code with"
  echo "WhatsApp on your phone: Settings > Linked Devices > Link a Device."
  echo
  echo "After it connects, wait for \"History sync complete\" and roughly"
  echo "60 seconds of quiet (history sync continues asynchronously after"
  echo "the connect message) before Ctrl-C."
  echo

  WA_STORE_DIR="$STORE_DIR" ./whatsapp-bridge
  # Reaching here means the bridge exited (e.g. Ctrl-C = SIGINT). Whether
  # pairing actually succeeded is for the operator to have observed above;
  # this script does not second-guess it.
}

main() {
  if [ "${1:-}" = "--pair" ]; then
    do_pair
    exit 0
  fi

  if [ -f "$LOGGED_OUT_SENTINEL" ]; then
    record_event "unpaired"
    echo "store/LOGGED_OUT sentinel present -- refusing to start." >&2
    echo "Run: $0 --pair" >&2
    exit 0
  fi

  if ! is_paired; then
    if [ "$PAIR_CHECK_ERRORED" = "1" ]; then
      # Query error, not a real pairing state -- do NOT record "unpaired"
      # (that would be false evidence). Exit non-zero so launchd's
      # KeepAlive={SuccessfulExit:false} retries in ~30s, when the
      # transient lock has likely cleared.
      echo "Could not query $DEVICE_DB (transient error, not a pairing decision) -- retrying via launchd." >&2
      exit 1
    fi
    record_event "unpaired"
    echo "No paired device found in $DEVICE_DB -- refusing to start." >&2
    echo "Run: $0 --pair" >&2
    exit 0
  fi

  exec env WA_STORE_DIR="$STORE_DIR" ./whatsapp-bridge
}

main "$@"
