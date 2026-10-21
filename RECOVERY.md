# Recovery: WhatsApp bridge

Read this before pairing, and keep it findable — the day you need it is the
day you've forgotten how this works.

## Is it alive?

```bash
launchctl print gui/$(id -u)/com.example.whatsapp-bridge | grep state
lsof -nP -iTCP:8080 -sTCP:LISTEN
sqlite3 -readonly ~/Developer/whatsapp-mcp/whatsapp-bridge/store/messages.db \
  "SELECT ts, kind, message_count FROM bridge_events ORDER BY seq DESC LIMIT 5;"
```

The last `bridge_events` row tells you what actually happened, independent
of whether launchd thinks the process is running:

- `connected` / `heartbeat` recently (within ~2 min) — it's fine.
- `disconnected` with no following `connected` — network hiccup; launchd
  should already be retrying (`ThrottleInterval` 30s).
- `logged_out` — the linked device was removed (you unlinked it on the
  phone, or WhatsApp force-logged it out). See "Re-pairing" below.
- `unpaired` — the wrapper found no valid session and refused to start the
  binary at all. Same fix as `logged_out`.
- Nothing newer than `started` from days ago, and the process isn't in
  `ps` — it crashed and launchd gave up (unlikely with `KeepAlive`, but
  check `~/.local/share/whatsapp-bridge/bridge.log` for a panic).

## Re-pairing (after logout or on first setup)

```bash
launchctl bootout gui/$(id -u)/com.example.whatsapp-bridge 2>/dev/null
cd ~/Developer/whatsapp-mcp/whatsapp-bridge
./run-bridge.sh --pair
```

This archives the old `store/whatsapp.db*` (if present) to
`store/archive-<timestamp>/`, then runs the bridge in the foreground and
prints a QR code. Scan it from WhatsApp on your phone: **Settings → Linked
Devices → Link a Device**. Wait for "History sync complete" plus about a
minute of quiet before Ctrl-C — the history sync that follows a successful
pair is asynchronous and still running after the "connected" message
prints.

Then bring the daemon back:

```bash
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.example.whatsapp-bridge.plist
```

**What re-pairing does to history**: it's a fresh linked-device session.
You get a new pairing-time history sync (typically the last several weeks
to months, WhatsApp's call, not something we control), but the gap between
your old session's last message and the new pairing is genuinely gone —
there's no server-side history API to backfill it. The old rows in
`messages.db` are untouched; only newly-fetched messages get added.

## What re-pairing does NOT require

You do not need to touch `chat-synopsis`, its config, or its database. The
bridge and `chat-synopsis` are decoupled through `messages.db` — as long as
the JID shapes stay the same (`<number>@s.whatsapp.net` for direct chats,
`<id>@g.us` for groups), the CLI keeps working against whatever's in there.

## The credential

`store/whatsapp.db` is the linked-device session key. Treat it like a
password:

- It's `chmod 600`'d by the wrapper on every start.
- If you lose the laptop, unlink the device from your phone immediately
  (Settings → Linked Devices) — that's the actual revocation; deleting the
  local file does nothing to a copy that already left the machine.
- Before any future `go get go.mau.fi/whatsmeow@...` bump, back this file
  up (bridge stopped) — a newer whatsmeow version may migrate the schema
  forward in a way the current binary can't read if you need to roll back.

## The one-time-alarm setup (optional, not part of this repo)

Worth building your own version of: a small script, run periodically by
launchd/cron, that reads `bridge_events` (read-only) and alerts you (e.g.
via Telegram/email) if the bridge has gone quiet, logged out, or stopped
ingesting messages. Without it, you will not otherwise notice a dead bridge
until a downstream consumer of `messages.db` comes back suspiciously
empty.

## If the bridge won't build after a dependency bump

whatsmeow's API has moved before between versions this bridge was written
against (context.Context added to several methods; LID-vs-phone-number
addressing introduced). Expect compile breaks after `go get
go.mau.fi/whatsmeow@latest`; check `go doc go.mau.fi/whatsmeow.<Symbol>`
for the current signature rather than guessing. Do not pair on a version
that only *compiles* — WhatsApp actively rejects companion clients running
stale library versions with an outright connection failure, so the real
test is a successful QR pairing, not just `go build`.

## Day-30 cutover (WhatsApp into the main chat-synopsis DB)

The day-30 launch job (`whatsapp-phase2-launch.py`, run daily by its own LaunchAgent) only
computes the trial
verdict and nags. The cutover itself is run by hand from a terminal, and it
does one thing: it merges the WhatsApp trial DB
(`~/.local/share/chat-synopsis-whatsapp/cache.db.nosync`) into the main DB
(`~/.local/share/telegram-synopsis/cache.db.nosync`). The CLI and the `/chat`
skill were already updated outside this job; the cutover does not touch them.

    /usr/bin/python3 ~/.claude/scheduled-scripts/whatsapp-phase2-launch.py --cutover

It runs a dry merge, shows the per-table counts, and waits for you to type
`LAUNCH`. Then it backs up the main DB, merges with the pinned Phase 2
binary, writes the `whatsapp-merge-completed` marker (which lets
`--channel whatsapp` use the main DB), and smoke-tests the live CLI. If the
smoke fails, it restores the backup on its own.

Phases you can see in `~/.local/share/whatsapp-bridge/phase2-launch-state.json`:

| Phase | Meaning | What to do |
|---|---|---|
| `verdict_ready` | Trial passed, cutover not run yet | Run `--cutover` |
| `launching` | A cutover stopped mid-way | Run `--cutover`, type `RESUME` |
| `launched` | Merged and smoke-tested | Edit the `/chat` skill first (below), then run `/chat --channel whatsapp <name>`; that acknowledges it and the job disables itself |
| `launch_failed` | The merge did not commit; nothing changed | Fix the cause, run `--cutover` again |
| `rollback_in_progress` | A restore stopped mid-way | Run `--cutover`, type `RESUME` |
| `restore_done` | The merge was rolled back cleanly | Fix the cause, run `--cutover` again |
| `rollback_failed` | The restore refused or failed | Read the notification, then use `--restore` |

The `/chat` skill decides which DB WhatsApp uses. Its WhatsApp section
(`claude-code-skill/chat/SKILL.md` in the chat-synopsis checkout) sets
`CHAT_DATA_DIR=~/.local/share/chat-synopsis-whatsapp` for the isolated trial.
After `launched`, remove that line so `/chat` uses the main DB. Until you do,
`/chat` keeps writing to the frozen trial archive and never acknowledges the
launch. After `restore_done` (or any restore), the line must be there again,
because WhatsApp is back on the isolated trial DB.

Manual restore of the pre-merge backup (needs a terminal and a typed
`RESTORE`):

    /usr/bin/python3 ~/.claude/scheduled-scripts/whatsapp-phase2-launch.py --restore

The restore refuses when the main DB's non-WhatsApp data changed after the
merge (for example, a Telegram pin or synopsis written later), because
restoring would delete that data. Reconcile by hand in that case. After
`launched`, treat the merge as one-way: the backup
(`~/.local/share/whatsapp-bridge/backups/cache.db.nosync.pre-merge-<op_id>`)
is kept indefinitely, but a restore drops everything written since. Never
delete the state file to re-run the merge.
