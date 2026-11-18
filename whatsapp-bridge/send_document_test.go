package main

import (
	"bytes"
	"testing"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
)

func TestBuildDocumentMessage_PreservesFilename(t *testing.T) {
	cases := []struct {
		name      string
		mediaPath string
		want      string
	}{
		{"plain filename", "report.pdf", "report.pdf"},
		{"nested path", "/tmp/reports/quarterly/report.pdf", "report.pdf"},
		{"spaces and unicode", "/tmp/shared reports/Résumé 東京 2026.pdf", "Résumé 東京 2026.pdf"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := buildDocumentMessage(tc.mediaPath, "application/pdf", "Report", whatsmeow.UploadResponse{})
			if doc == nil {
				t.Fatal("buildDocumentMessage returned nil")
			}
			if got := doc.GetTitle(); got != tc.want {
				t.Errorf("Title = %q, want %q", got, tc.want)
			}
			if got := doc.GetFileName(); got != tc.want {
				t.Errorf("FileName = %q, want %q", got, tc.want)
			}

			// These are the metadata values sendWhatsAppMessage passes to the local store.
			mediaType, filename, _, _, _, _, _ := extractMediaInfo(&waProto.Message{DocumentMessage: doc})
			if mediaType != "document" {
				t.Errorf("stored media type = %q, want document", mediaType)
			}
			if filename != tc.want {
				t.Errorf("stored filename = %q, want %q", filename, tc.want)
			}
		})
	}
}

func TestBuildDocumentMessage_PreservesUploadMetadata(t *testing.T) {
	resp := whatsmeow.UploadResponse{
		URL:           "https://example.invalid/document.enc?token=synthetic",
		DirectPath:    "/synthetic/document.enc?token=synthetic",
		MediaKey:      bytes.Repeat([]byte{0x11}, 32),
		FileEncSHA256: bytes.Repeat([]byte{0x22}, 32),
		FileSHA256:    bytes.Repeat([]byte{0x33}, 32),
		FileLength:    12345,
	}
	doc := buildDocumentMessage("report.pdf", "application/pdf", "Quarterly report — ready to share", resp)
	if doc == nil {
		t.Fatal("buildDocumentMessage returned nil")
	}
	for _, tc := range []struct {
		name string
		got  string
		want string
	}{
		{"Caption", doc.GetCaption(), "Quarterly report — ready to share"},
		{"Mimetype", doc.GetMimetype(), "application/pdf"},
		{"URL", doc.GetURL(), resp.URL},
		{"DirectPath", doc.GetDirectPath(), resp.DirectPath},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
	for _, tc := range []struct {
		name string
		got  []byte
		want []byte
	}{
		{"MediaKey", doc.GetMediaKey(), resp.MediaKey},
		{"FileEncSHA256", doc.GetFileEncSHA256(), resp.FileEncSHA256},
		{"FileSHA256", doc.GetFileSHA256(), resp.FileSHA256},
	} {
		if !bytes.Equal(tc.got, tc.want) {
			t.Errorf("%s = %x, want %x", tc.name, tc.got, tc.want)
		}
	}
	if got := doc.GetFileLength(); got != resp.FileLength {
		t.Errorf("FileLength = %d, want %d", got, resp.FileLength)
	}
}
