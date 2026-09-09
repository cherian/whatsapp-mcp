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
