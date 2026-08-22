package syncer

import (
	"context"
	"strings"
	"testing"
	"time"

	"rolltop/backend/blob"
)

// TestStoreFetchedMessageOversizedStoresHeaderOnlyPlaceholder covers the
// metadata-only ingestion path for messages above the mirror limit: the row
// stays listable from headers, no raw blob file is written, and the body
// preview explains why the content is absent.
func TestStoreFetchedMessageOversizedStoresHeaderOnlyPlaceholder(t *testing.T) {
	fixture := newMoveTestFixture(t)
	ctx := context.Background()
	fixture.service.Blobs = blob.New(t.TempDir())

	raw := []byte("From: giant@example.test\r\n" +
		"To: owner@example.test\r\n" +
		"Subject: Enormous attachment mail\r\n" +
		"Date: Tue, 14 Jul 2026 12:00:00 +0000\r\n" +
		"Message-ID: <enormous@example.test>\r\n\r\n")
	item := FetchedMessage{
		Mailbox:      fixture.source.Name,
		UID:          75,
		UIDValidity:  uint32(fixture.source.UIDValidity),
		InternalDate: time.Date(2026, 7, 14, 12, 1, 0, 0, time.UTC),
		Size:         900 * 1024 * 1024,
		Raw:          raw,
		Oversized:    true,
	}

	msg, _, pendingIndex, err := fixture.service.storeFetchedMessage(ctx, fixture.userID,
		fixture.account, fixture.source, item, true)
	if err != nil {
		t.Fatal(err)
	}
	if pendingIndex != nil {
		t.Fatal("oversized message should not prepare a full search document from headers alone")
	}
	if msg.Subject != "Enormous attachment mail" {
		t.Fatalf("subject = %q, want header subject", msg.Subject)
	}
	if msg.HasAttachments {
		t.Fatal("oversized placeholder must not claim attachments")
	}
	if !strings.Contains(msg.BodyText, "too large") {
		t.Fatalf("body preview = %q, want oversized note", msg.BodyText)
	}
	if msg.BlobPath != "" {
		t.Fatalf("blob path = %q, want empty for header-only record", msg.BlobPath)
	}
	blobRec, err := fixture.store.GetBlobForUser(ctx, fixture.userID, msg.BlobID)
	if err != nil {
		t.Fatal(err)
	}
	if blobRec.Kind != "message-remote" || blobRec.Size != 0 {
		t.Fatalf("oversized blob kind=%q size=%d, want message-remote/0", blobRec.Kind, blobRec.Size)
	}
}
