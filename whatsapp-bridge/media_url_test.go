package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"go.mau.fi/whatsmeow"
)

// Fixtures below are SYNTHETIC, structurally identical to the two real URL
// shapes observed in production (2,029 "/v/t62..." rows, 1,594
// "/o1/v/t24..." rows, both 100% carrying a query string). They were
// originally real production URLs; those were replaced because the "oh="/
// "oe=" query parameters are a genuine signature and expiry -- not a
// disposable nonce -- so a real one must never be committed to a public
// repo, live or not.
const (
	realVStyleURL  = "https://mmg.whatsapp.net/v/t62.7117-24/100000000_1000000000000000_1000000000000000000_n.enc?ccb=11-4&oh=01_SyntheticFixtureOhTokenNotARealSignatureAAAA&oe=00000000&_nc_sid=000000&mms3=true"
	realO1StyleURL = "https://mmg.whatsapp.net/o1/v/t24/f2/m235/BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB?ccb=9-4&oh=01_SyntheticFixtureOhTokenNotARealSignatureBBBB&oe=00000000&_nc_sid=000000&mms3=true"
)

// This is the test that would have caught the original bug: run it against
// the OLD extractDirectPathFromURL (query-stripped) and it fails, because
// the old function discarded everything after "?".
func TestDirectPathFromURL_PreservesQueryByteForByte(t *testing.T) {
	cases := []struct {
		name string
		url  string
		want string
	}{
		{
			name: "v-style (synthetic, real production shape)",
			url:  realVStyleURL,
			want: "/v/t62.7117-24/100000000_1000000000000000_1000000000000000000_n.enc?ccb=11-4&oh=01_SyntheticFixtureOhTokenNotARealSignatureAAAA&oe=00000000&_nc_sid=000000&mms3=true",
		},
		{
			name: "o1-style (synthetic, real production shape)",
			url:  realO1StyleURL,
			want: "/o1/v/t24/f2/m235/BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB?ccb=9-4&oh=01_SyntheticFixtureOhTokenNotARealSignatureBBBB&oe=00000000&_nc_sid=000000&mms3=true",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := directPathFromURL(tc.url)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("directPathFromURL(%q)\n  got:  %q\n  want: %q", tc.url, got, tc.want)
			}
			if got[0] != '/' {
				t.Errorf("direct path must start with '/', got %q", got)
			}
		})
	}
}

func TestDirectPathFromURL_MissingQueryFailsClosed(t *testing.T) {
	// No such row exists in production today (100% of stored URLs carry a
	// query), but a future WhatsApp URL format change must fail loudly
	// here rather than issue a request guaranteed to 403 (the exact bug
	// this whole fix exists to correct, just triggered a different way).
	_, err := directPathFromURL("https://mmg.whatsapp.net/v/t62.7117-24/foo.enc")
	if !errors.Is(err, ErrMediaURLMissingQuery) {
		t.Errorf("expected ErrMediaURLMissingQuery, got %v", err)
	}
}

func TestDirectPathFromURL_RejectsNonHTTPS(t *testing.T) {
	_, err := directPathFromURL("http://mmg.whatsapp.net/v/t62.7117-24/foo.enc?oh=x")
	if err == nil {
		t.Error("expected an error for a non-https URL, got nil")
	}
}

func TestDirectPathFromURL_RejectsUnparseable(t *testing.T) {
	_, err := directPathFromURL("://not a url at all")
	if err == nil {
		t.Error("expected an error for an unparseable URL, got nil")
	}
}

func TestDirectPathFromURL_RejectsEmptyHost(t *testing.T) {
	_, err := directPathFromURL("https:///v/t62.7117-24/foo.enc?oh=x")
	if err == nil {
		t.Error("expected an error for a URL with no host, got nil")
	}
}

func TestDirectPathFromURL_DoesNotReencodeOrReorderQuery(t *testing.T) {
	// Regression guard against the "obvious" alternative fix that is
	// itself wrong: u.String(), u.RequestURI(), or u.Query().Encode()
	// can all re-escape path segments or reorder/re-encode query
	// parameters, which can invalidate the CDN's signature just as
	// thoroughly as stripping the query outright. RawQuery must survive
	// character-for-character, in its original order.
	url := "https://mmg.whatsapp.net/v/t62.7117-24/foo.enc?ccb=11-4&oh=ZZZ&oe=6AC8BF66&_nc_sid=5e03e0&mms3=true"
	got, err := directPathFromURL(url)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "/v/t62.7117-24/foo.enc?ccb=11-4&oh=ZZZ&oe=6AC8BF66&_nc_sid=5e03e0&mms3=true"
	if got != want {
		t.Errorf("query was altered:\n  got:  %q\n  want: %q", got, want)
	}
}

func TestArchiveFilename_UniqueByMessageIDNotFilename(t *testing.T) {
	// This is the test that would have caught the original 80%-collision
	// bug: two messages with the IDENTICAL display filename (exactly what
	// time.Now()-based naming produced for hundreds of messages landing
	// in the same second) must still produce different on-disk paths.
	a, err := archiveFilename("3EB0C767D097E9ECFA5C", "image_20260909_082556.jpg")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	b, err := archiveFilename("3EB0C767D097E9ECFA5D", "image_20260909_082556.jpg")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a == b {
		t.Errorf("two different message IDs produced the same filename: %q", a)
	}
}

func TestArchiveFilename_RejectsEmptyMessageID(t *testing.T) {
	if _, err := archiveFilename("", "photo.jpg"); err == nil {
		t.Error("expected an error for an empty message ID, got nil")
	}
}

func TestArchiveFilename_TraversalAttemptsAreNeutralized(t *testing.T) {
	cases := []string{"../../etc/passwd", "..", ".", "", "a/b", `a\b`, "a\x00b"}
	for _, displayName := range cases {
		got, err := archiveFilename("3EB0C767D097E9ECFA5C", displayName)
		if err != nil {
			t.Fatalf("archiveFilename(%q) unexpected error: %v", displayName, err)
		}
		if strings.ContainsAny(got, "/\\") {
			t.Errorf("archiveFilename(%q) = %q still contains a path separator", displayName, got)
		}
		if strings.Contains(got, "..") {
			t.Errorf("archiveFilename(%q) = %q still contains '..'", displayName, got)
		}
	}
}

func TestArchiveFilename_UnsafeMessageIDIsHashedNotTrusted(t *testing.T) {
	got, err := archiveFilename("../../etc/passwd", "photo.jpg")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.ContainsAny(got, "/\\") || strings.Contains(got, "..") {
		t.Errorf("unsafe message ID leaked into filename: %q", got)
	}
}

func TestClassifyMediaCode(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil is ok", nil, "ok"},
		{"not media", ErrNotMediaMessage, "not_media"},
		{"not media wrapped", fmt.Errorf("wrap: %w", ErrNotMediaMessage), "not_media"},
		{"incomplete", ErrMediaIncomplete, "incomplete"},
		{"no url present", whatsmeow.ErrNoURLPresent, "incomplete"},
		{"expired 403", whatsmeow.ErrMediaDownloadFailedWith403, "expired"},
		{"expired 403 wrapped", fmt.Errorf("failed to download media: %w", whatsmeow.ErrMediaDownloadFailedWith403), "expired"},
		{"expired 404", whatsmeow.ErrMediaDownloadFailedWith404, "expired"},
		{"expired 410", whatsmeow.ErrMediaDownloadFailedWith410, "expired"},
		{"unknown error is internal", errors.New("something else broke"), "internal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyMediaCode(tc.err); got != tc.want {
				t.Errorf("classifyMediaCode(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}
