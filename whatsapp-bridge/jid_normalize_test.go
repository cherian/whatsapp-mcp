package main

import (
	"context"
	"testing"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// newTestClient returns a *whatsmeow.Client backed by an in-memory SQLite
// store, so resolvePN's client.Store.LIDs lookups exercise the real
// whatsmeow LID-mapping code path instead of a hand-rolled fake.
func newTestClient(t *testing.T) *whatsmeow.Client {
	t.Helper()
	ctx := context.Background()
	log := waLog.Stdout("test", "ERROR", false)
	container, err := sqlstore.New(ctx, "sqlite3", "file:jidnorm?mode=memory&cache=shared&_foreign_keys=on", log)
	if err != nil {
		t.Fatalf("failed to open in-memory store: %v", err)
	}
	device := container.NewDevice()
	// device.LIDs is nil until the device is registered. PutDevice panics on
	// a device that hasn't completed real pairing (it dereferences
	// device.Account, only set post-handshake), so wire up just the LID
	// store directly — the same *CachedLIDMap PutDevice would have attached.
	device.LIDs = container.LIDMap
	client := whatsmeow.NewClient(device, log)
	if client == nil {
		t.Fatal("NewClient returned nil")
	}
	return client
}

func mustJID(t *testing.T, s string) types.JID {
	t.Helper()
	jid, err := types.ParseJID(s)
	if err != nil {
		t.Fatalf("ParseJID(%q): %v", s, err)
	}
	return jid
}

func TestResolvePN_AlreadyPN_PassesThrough(t *testing.T) {
	client := newTestClient(t)
	log := waLog.Stdout("test", "ERROR", false)
	jid := mustJID(t, "15551234567@s.whatsapp.net")

	got := resolvePN(context.Background(), client, jid, types.EmptyJID, log, "test")

	if got != jid {
		t.Errorf("resolvePN(PN, ...) = %v, want unchanged %v", got, jid)
	}
}

func TestResolvePN_Group_PassesThrough(t *testing.T) {
	client := newTestClient(t)
	log := waLog.Stdout("test", "ERROR", false)
	jid := mustJID(t, "120363000000000000@g.us")

	got := resolvePN(context.Background(), client, jid, types.EmptyJID, log, "test")

	if got != jid {
		t.Errorf("resolvePN(group, ...) = %v, want unchanged %v", got, jid)
	}
}

func TestResolvePN_LIDWithAlt_UsesAltWithoutLookup(t *testing.T) {
	// client is nil here on purpose: if resolvePN tried a store lookup
	// instead of using the free alt, this would panic on client.Store.
	log := waLog.Stdout("test", "ERROR", false)
	lid := mustJID(t, "100000000000001@lid")
	alt := mustJID(t, "15551234567@s.whatsapp.net")

	got := resolvePN(context.Background(), nil, lid, alt, log, "test")

	if got != alt {
		t.Errorf("resolvePN(LID, alt=%v) = %v, want alt", alt, got)
	}
}

func TestResolvePN_LIDMappedInStore_ResolvesViaLookup(t *testing.T) {
	client := newTestClient(t)
	log := waLog.Stdout("test", "ERROR", false)
	lid := mustJID(t, "100000000000001@lid")
	pn := mustJID(t, "15559876543@s.whatsapp.net")

	if err := client.Store.LIDs.PutLIDMapping(context.Background(), lid, pn); err != nil {
		t.Fatalf("PutLIDMapping: %v", err)
	}

	got := resolvePN(context.Background(), client, lid, types.EmptyJID, log, "test")

	if got.User != pn.User || got.Server != pn.Server {
		t.Errorf("resolvePN(LID, mapped in store) = %v, want %v", got, pn)
	}
}

func TestResolvePN_LIDUnmapped_FallsBackToLID(t *testing.T) {
	client := newTestClient(t)
	log := waLog.Stdout("test", "ERROR", false)
	lid := mustJID(t, "999999999999999@lid")

	got := resolvePN(context.Background(), client, lid, types.EmptyJID, log, "test")

	if got != lid {
		t.Errorf("resolvePN(LID, unmapped) = %v, want the LID unchanged (%v)", got, lid)
	}
}

func TestResolvePN_LIDWithADSuffix_NormalizesUser(t *testing.T) {
	// AD (device-specific) JIDs carry a ":<device>" suffix on the User part.
	// ParseJID keeps that on jid.User; PutLIDMapping stores/returns the PN
	// without a device suffix, which is what chat-synopsis and chat-send
	// both expect for chat_jid.
	client := newTestClient(t)
	log := waLog.Stdout("test", "ERROR", false)
	lid := mustJID(t, "100000000000001:19@lid")
	pn := mustJID(t, "15551234567@s.whatsapp.net")

	// PutLIDMapping keys on the LID; use the non-AD form as whatsmeow does
	// internally for its own live lookups (ToNonAD strips the device suffix).
	if err := client.Store.LIDs.PutLIDMapping(context.Background(), lid.ToNonAD(), pn); err != nil {
		t.Fatalf("PutLIDMapping: %v", err)
	}

	got := resolvePN(context.Background(), client, lid.ToNonAD(), types.EmptyJID, log, "test")

	if got.User != pn.User || got.Server != pn.Server {
		t.Errorf("resolvePN(LID with AD suffix, mapped) = %v, want %v", got, pn)
	}
	if got.User == lid.User {
		t.Errorf("resolvePN did not normalize away the LID form: got %v", got)
	}
}

func TestResolvePNString_EmptyAndUnparseable(t *testing.T) {
	client := newTestClient(t)
	log := waLog.Stdout("test", "ERROR", false)

	if got := resolvePNString(context.Background(), client, "", log, "test"); got != "" {
		t.Errorf("resolvePNString(\"\") = %q, want \"\"", got)
	}

	// Not a valid JID shape at all; must pass through unchanged rather than
	// silently dropping/mangling the value.
	garbage := "not-a-jid"
	if got := resolvePNString(context.Background(), client, garbage, log, "test"); got != garbage {
		t.Errorf("resolvePNString(garbage) = %q, want unchanged %q", got, garbage)
	}
}
