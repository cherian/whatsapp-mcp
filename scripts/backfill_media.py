#!/usr/bin/env python3
"""One-shot backfill of still-downloadable WhatsApp media.

Background: media download never worked at all until the 2026-09-15 fix
(a URL-construction bug -- see whatsapp-bridge/main.go's directPathFromURL
and its commit message for the full history). 3,626 media messages
existed with zero files ever archived.
WhatsApp's CDN URLs also genuinely expire (~25-32 days, empirically
fuzzy -- some media has been observed expired as early as 25 days old),
so this is a race against an actual clock: every day this doesn't run,
more media crosses into "permanently gone."

Design choices (see the plan file's "What the two reviews changed"
section for the full LLM-council + Codex review that produced these):

- Calls the bridge's own `POST /api/download` rather than reimplementing
  WhatsApp's media decryption in Python. The isolation argument for a
  fully independent worker (own HTTP fetch, own decrypt) applies to the
  ONGOING eager-archiving hook that will run inside message ingestion
  (Phase 6, not built here) -- a hung fetch there can stall the whatsmeow
  event loop and drop live messages. This script is a manual,
  rate-limited, one-shot pass; going through the endpoint reuses code
  that's already fixed, tested, and proven (via this same review) to
  stream to disk without buffering large files in RAM.
- Owns its OWN sqlite state db (media-backfill-state.db), separate from
  the bridge's messages.db, so this script's bookkeeping can never
  contend with the bridge's live writer (BEGIN DEFERRED + no WAL on the
  device db already makes that a real risk elsewhere in this project --
  see run-bridge.sh's is_paired() fix history).
- Terminal failures (expired/not_media/incomplete) are recorded and
  never retried -- without this, a plain re-run would burn the rate-limit
  budget re-discovering the same ~2,100+ dead items every time.
- Oldest-first among eligible rows: the oldest have the least time left
  before they cross whatever the real expiry boundary turns out to be
  for that particular piece of media.
- Rate-limited (default 1 request / 2s, serial): this hits the same CDN
  and the same account session real WhatsApp clients use. A burst of
  parallel fetches from what looks like a companion device is exactly
  the shape that gets a device flagged or logged out -- and a forced
  logout ends this whole project's data capture, not just the backfill.

Usage:
    python3 backfill_media.py --dry-run          # accounting report only
    python3 backfill_media.py                    # run the real backfill
    python3 backfill_media.py --limit 50         # cap this run's attempts
    python3 backfill_media.py --window-days 32   # eligibility window
"""
from __future__ import annotations

import argparse
import json
import sqlite3
import sys
import time
import urllib.error
import urllib.request
from pathlib import Path

MESSAGES_DB = Path.home() / "Developer/whatsapp-mcp/whatsapp-bridge/store/messages.db"
STATE_DB = Path.home() / ".local/share/whatsapp-bridge/media-backfill-state.db"
API_URL = "http://localhost:8080/api/download"

DEFAULT_WINDOW_DAYS = 32  # generous: real observed expiry has been as early as ~25 days
DEFAULT_RATE_LIMIT_SECONDS = 2.0
MEDIA_TYPES = ("image", "video", "audio", "document")

TERMINAL_CODES = {"not_media", "expired", "incomplete"}  # never retried
TRANSIENT_CODES = {"offline", "internal"}  # retried on a later run


def _connect_messages_ro() -> sqlite3.Connection:
    conn = sqlite3.connect(f"file:{MESSAGES_DB}?mode=ro", uri=True, timeout=5)
    conn.execute("PRAGMA query_only = ON")
    return conn


def _connect_state() -> sqlite3.Connection:
    STATE_DB.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    conn = sqlite3.connect(STATE_DB)
    conn.execute(
        """CREATE TABLE IF NOT EXISTS backfill_state (
            chat_jid TEXT NOT NULL,
            message_id TEXT NOT NULL,
            status TEXT NOT NULL,      -- 'archived' | 'terminal' | 'transient_failed'
            code TEXT,                 -- the bridge's classifyMediaCode value
            message TEXT,
            attempts INTEGER NOT NULL DEFAULT 0,
            last_attempt_ts INTEGER,
            PRIMARY KEY (chat_jid, message_id)
        )"""
    )
    conn.commit()
    return conn


def eligible_candidates(msg_conn: sqlite3.Connection, window_days: int) -> list[tuple[str, str, str, int]]:
    """Returns (id, chat_jid, media_type, file_length) for every message
    with media, a real URL, and a timestamp inside the eligibility window
    -- oldest first. Every media type, no size cap (per the 2026-09-15
    decision to include the 105 large files / 2.91GB that a 5MB cap had
    been excluding -- see the plan file)."""
    placeholders = ",".join("?" for _ in MEDIA_TYPES)
    rows = msg_conn.execute(
        f"""SELECT id, chat_jid, media_type, file_length
            FROM messages
            WHERE media_type IN ({placeholders})
              AND url IS NOT NULL AND url != ''
              AND file_length > 0
              AND timestamp >= datetime('now', ?)
            ORDER BY timestamp ASC""",
        (*MEDIA_TYPES, f"-{window_days} days"),
    ).fetchall()
    return rows


def already_resolved(state_conn: sqlite3.Connection, chat_jid: str, message_id: str) -> tuple[str, str] | None:
    row = state_conn.execute(
        "SELECT status, code FROM backfill_state WHERE chat_jid = ? AND message_id = ?",
        (chat_jid, message_id),
    ).fetchone()
    return tuple(row) if row else None


def record(state_conn: sqlite3.Connection, chat_jid: str, message_id: str, status: str, code: str, message: str) -> None:
    state_conn.execute(
        """INSERT INTO backfill_state (chat_jid, message_id, status, code, message, attempts, last_attempt_ts)
           VALUES (?, ?, ?, ?, ?, 1, ?)
           ON CONFLICT(chat_jid, message_id) DO UPDATE SET
             status=excluded.status, code=excluded.code, message=excluded.message,
             attempts=backfill_state.attempts + 1, last_attempt_ts=excluded.last_attempt_ts""",
        (chat_jid, message_id, status, code, message, int(time.time())),
    )
    state_conn.commit()


def call_download_api(message_id: str, chat_jid: str, timeout: float = 660.0) -> dict:
    """Calls the bridge's own /api/download -- see module docstring for
    why this script doesn't reimplement decryption. 660s timeout: a bit
    longer than the bridge's own 10-minute (600s) per-download deadline,
    so the bridge's own timeout fires first and we get its real error."""
    payload = json.dumps({"message_id": message_id, "chat_jid": chat_jid}).encode()
    req = urllib.request.Request(
        API_URL, data=payload, headers={"Content-Type": "application/json"}, method="POST"
    )
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return json.loads(resp.read())
    except urllib.error.HTTPError as e:
        try:
            return json.loads(e.read())
        except (json.JSONDecodeError, ValueError):
            return {"success": False, "code": "internal", "message": f"HTTP {e.code}"}
    except urllib.error.URLError as e:
        return {"success": False, "code": "offline", "message": str(e)}


def dry_run_report(msg_conn: sqlite3.Connection, state_conn: sqlite3.Connection, window_days: int) -> None:
    """Mutually exclusive category counts summing to every media row in
    messages.db -- Codex-flagged (2026-09-15 review): earlier population
    figures (1,471 vs 1,353) didn't reconcile, traced to a since-dropped
    size cap silently excluding items. This report exists so that never
    happens silently again."""
    placeholders = ",".join("?" for _ in MEDIA_TYPES)
    total = msg_conn.execute(
        f"SELECT COUNT(*) FROM messages WHERE media_type IN ({placeholders})", MEDIA_TYPES
    ).fetchone()[0]

    incomplete = msg_conn.execute(
        f"""SELECT COUNT(*) FROM messages
            WHERE media_type IN ({placeholders})
              AND (url IS NULL OR url = '' OR file_length IS NULL OR file_length <= 0)""",
        MEDIA_TYPES,
    ).fetchone()[0]

    candidates = eligible_candidates(msg_conn, window_days)
    in_window_ids = {(c, m) for m, c, *_ in candidates}

    already_archived = 0
    already_terminal = 0
    already_transient = 0
    fresh_candidates = 0
    total_bytes = 0
    for message_id, chat_jid, _media_type, file_length in candidates:
        resolved = already_resolved(state_conn, chat_jid, message_id)
        if resolved is None:
            fresh_candidates += 1
            total_bytes += file_length
        elif resolved[0] == "archived":
            already_archived += 1
        elif resolved[0] == "terminal":
            already_terminal += 1
        else:
            already_transient += 1

    outside_window = total - incomplete - len(in_window_ids)

    print("=== Backfill dry-run accounting ===")
    print(f"  total media rows in messages.db      : {total}")
    print(f"  incomplete crypto metadata (unusable) : {incomplete}")
    print(f"  outside the {window_days}-day eligibility window : {outside_window}")
    print(f"  in window, already archived on disk   : {already_archived}")
    print(f"  in window, already known terminal      : {already_terminal}")
    print(f"  in window, transient-failed (will retry): {already_transient}")
    print(f"  in window, NEW candidates this run     : {fresh_candidates}  ({total_bytes/1024/1024:.1f} MB)")
    reconciled = incomplete + outside_window + already_archived + already_terminal + already_transient + fresh_candidates
    print(f"  --- sums to {reconciled} of {total} total ---")
    if reconciled != total:
        print(f"  !! MISMATCH: {total - reconciled} rows unaccounted for -- do not run the real backfill until this is 0", file=sys.stderr)
        sys.exit(1)


def run_backfill(msg_conn: sqlite3.Connection, state_conn: sqlite3.Connection, window_days: int,
                  rate_limit_seconds: float, limit: int | None) -> None:
    candidates = eligible_candidates(msg_conn, window_days)
    attempted = 0
    archived = 0
    terminal = 0
    transient = 0

    for message_id, chat_jid, media_type, file_length in candidates:
        if limit is not None and attempted >= limit:
            print(f"Reached --limit {limit}, stopping.")
            break

        resolved = already_resolved(state_conn, chat_jid, message_id)
        if resolved is not None and resolved[0] in ("archived", "terminal"):
            continue  # already resolved, permanently -- never re-attempt

        attempted += 1
        resp = call_download_api(message_id, chat_jid)
        code = resp.get("code", "internal")
        message = resp.get("message", "")

        if resp.get("success"):
            record(state_conn, chat_jid, message_id, "archived", code, message)
            archived += 1
            print(f"[{attempted}] OK      {media_type:9s} {file_length/1024:.0f}KB  {chat_jid}/{message_id}")
        elif code in TERMINAL_CODES:
            record(state_conn, chat_jid, message_id, "terminal", code, message)
            terminal += 1
            print(f"[{attempted}] {code.upper():9s} {chat_jid}/{message_id}: {message}")
        else:
            record(state_conn, chat_jid, message_id, "transient_failed", code, message)
            transient += 1
            print(f"[{attempted}] RETRY   {code}: {chat_jid}/{message_id}: {message}", file=sys.stderr)
            if code == "offline":
                print("Bridge is offline -- stopping this run early (will resume on next invocation).", file=sys.stderr)
                break

        time.sleep(rate_limit_seconds)

    print()
    print(f"=== Run summary: {attempted} attempted, {archived} archived, {terminal} newly terminal, {transient} transient (will retry) ===")


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--dry-run", action="store_true", help="Print the accounting report only; download nothing.")
    ap.add_argument("--window-days", type=int, default=DEFAULT_WINDOW_DAYS, help=f"Eligibility window (default {DEFAULT_WINDOW_DAYS}).")
    ap.add_argument("--rate-limit-seconds", type=float, default=DEFAULT_RATE_LIMIT_SECONDS, help=f"Delay between requests (default {DEFAULT_RATE_LIMIT_SECONDS}).")
    ap.add_argument("--limit", type=int, default=None, help="Cap the number of NEW attempts this run.")
    args = ap.parse_args()

    if not MESSAGES_DB.exists():
        print(f"messages.db not found at {MESSAGES_DB}", file=sys.stderr)
        return 1

    msg_conn = _connect_messages_ro()
    state_conn = _connect_state()

    if args.dry_run:
        dry_run_report(msg_conn, state_conn, args.window_days)
    else:
        run_backfill(msg_conn, state_conn, args.window_days, args.rate_limit_seconds, args.limit)
    return 0


if __name__ == "__main__":
    sys.exit(main())
