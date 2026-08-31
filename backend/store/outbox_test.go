// File overview: Durable outbox isolation, optimistic Sent visibility, recovery, and promotion tests.

package store

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

type outboxTestFixture struct {
	db      *Store
	user    User
	account MailAccount
	smtp    SMTPAccount
	sent    Mailbox
	blob    BlobRecord
}

func newOutboxTestFixture(t *testing.T, suffix string) outboxTestFixture {
	t.Helper()
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "rolltop.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	user, err := db.CreateUser(ctx, suffix+"@example.test", suffix, "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	account, err := db.CreateMailAccount(ctx, MailAccount{
		UserID: user.ID, Email: user.Email, Host: "imap.example.test", Port: 993,
		Username: user.Email, EncryptedPassword: "encrypted", UseTLS: true, Mailbox: "INBOX",
	})
	if err != nil {
		t.Fatal(err)
	}
	smtpAccount, err := db.CreateSMTPAccount(ctx, SMTPAccount{
		UserID: user.ID, Label: "SMTP", Host: "smtp.example.test", Port: 587,
		Username: user.Email, EncryptedPassword: "encrypted", UseTLS: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	sent, err := db.GetOrCreateMailboxWithRole(ctx, user.ID, account.ID, "Sent", "sent")
	if err != nil {
		t.Fatal(err)
	}
	blob, err := db.CreateBlob(ctx, BlobRecord{
		UserID: user.ID, Kind: "outbox-message",
		Path: "users/outbox/" + suffix + ".eml", SHA256: "sha-" + suffix, Size: 128,
	})
	if err != nil {
		t.Fatal(err)
	}
	return outboxTestFixture{db: db, user: user, account: account, smtp: smtpAccount, sent: sent, blob: blob}
}

func (f outboxTestFixture) enqueue(t *testing.T, submission string) (OutboxJob, MessageRecord) {
	t.Helper()
	now := time.Now().UTC()
	job, msg, created, err := f.db.EnqueueOutboxMessage(context.Background(), OutboxEnqueue{
		UserID:          f.user.ID,
		SubmissionKey:   submission,
		SMTPAccountID:   f.smtp.ID,
		IMAPAccountID:   f.account.ID,
		SentMailboxID:   f.sent.ID,
		EnvelopeFrom:    f.user.Email,
		RecipientsJSON:  `["recipient@example.test"]`,
		MessageIDHeader: "<" + submission + "@example.test>",
		Blob:            f.blob,
		RawSHA256:       f.blob.SHA256,
		RawSize:         f.blob.Size,
		Message: CreateMessage{
			Subject: "Queued " + submission, FromAddr: f.user.Email,
			ToAddr: "recipient@example.test", Date: now, InternalDate: now,
			Size: f.blob.Size, BodyText: "queued body", IsRead: true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("first enqueue was reported as an existing job")
	}
	return job, msg
}

func TestOutboxEnqueueCreatesVisibleSentMessageWithoutAdvancingRemoteUID(t *testing.T) {
	f := newOutboxTestFixture(t, "visible")
	ctx := context.Background()
	job, msg := f.enqueue(t, "stable-submit")
	if msg.ID != job.OptimisticMessageID || msg.MailboxID != f.sent.ID || msg.AccountID != f.account.ID {
		t.Fatalf("optimistic Sent message = %+v, job = %+v", msg, job)
	}
	if msg.UID < outboxSyntheticUIDFloor {
		t.Fatalf("optimistic UID=%d, want synthetic range", msg.UID)
	}
	if next, err := f.db.NextUIDForMailbox(ctx, f.user.ID, f.sent.ID); err != nil || next != 1 {
		t.Fatalf("NextUIDForMailbox = %d, %v; want 1", next, err)
	}
	if uids, err := f.db.MessageUIDsForMailbox(ctx, f.user.ID, f.account.ID, f.sent.ID); err != nil || len(uids) != 0 {
		t.Fatalf("remote-derived UIDs = %v, %v; want none", uids, err)
	}
	deleted, err := f.db.DeleteMessagesMissingUIDs(ctx, f.user.ID, f.account.ID, f.sent.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 0 {
		t.Fatalf("remote reconciliation deleted optimistic Sent message: %+v", deleted)
	}
	if _, err := f.db.GetMessageForUser(ctx, f.user.ID, msg.ID); err != nil {
		t.Fatalf("optimistic Sent message disappeared: %v", err)
	}
	archive, err := f.db.GetOrCreateMailbox(ctx, f.user.ID, f.account.ID, "Archive")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.StageMessageTransfer(ctx, f.user.ID, msg.ID, archive.ID, "copy", ""); err == nil ||
		!strings.Contains(err.Error(), "still being delivered") {
		t.Fatalf("optimistic Sent transfer error=%v, want delivery-in-progress rejection", err)
	}

	sameJob, sameMessage, created, err := f.db.EnqueueOutboxMessage(ctx, OutboxEnqueue{
		UserID: f.user.ID, SubmissionKey: "stable-submit", SMTPAccountID: f.smtp.ID,
		IMAPAccountID: f.account.ID, SentMailboxID: f.sent.ID, EnvelopeFrom: f.user.Email,
		RecipientsJSON: `["ignored@example.test"]`, MessageIDHeader: "<ignored@example.test>",
		Blob: f.blob, RawSHA256: f.blob.SHA256, RawSize: f.blob.Size,
	})
	if err != nil {
		t.Fatal(err)
	}
	if created || sameJob.ID != job.ID || sameMessage.ID != msg.ID {
		t.Fatalf("idempotent enqueue created=%v job=%d message=%d; want %d/%d",
			created, sameJob.ID, sameMessage.ID, job.ID, msg.ID)
	}
}

func TestOutboxTenantIsolationAndExpiredInFlightRecovery(t *testing.T) {
	f := newOutboxTestFixture(t, "owner")
	ctx := context.Background()
	job, _ := f.enqueue(t, "interrupted")
	other, err := f.db.CreateUser(ctx, "other@example.test", "Other", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.GetOutboxJobForUser(ctx, other.ID, job.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant outbox read error=%v, want not found", err)
	}
	foreignSMTP, err := f.db.CreateSMTPAccount(ctx, SMTPAccount{
		UserID: other.ID, Label: "Other SMTP", Host: "smtp.other.test", Port: 587,
		Username: other.Email, EncryptedPassword: "encrypted", UseTLS: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, _, _, err := f.db.EnqueueOutboxMessage(ctx, OutboxEnqueue{
		UserID: f.user.ID, SubmissionKey: "foreign-smtp", SMTPAccountID: foreignSMTP.ID,
		IMAPAccountID: f.account.ID, SentMailboxID: f.sent.ID, EnvelopeFrom: f.user.Email,
		RecipientsJSON: `["recipient@example.test"]`, MessageIDHeader: "<foreign@example.test>",
		Blob: f.blob, RawSHA256: f.blob.SHA256, RawSize: f.blob.Size,
		Message: CreateMessage{Date: now, InternalDate: now, Size: f.blob.Size},
	}); err == nil {
		t.Fatal("cross-tenant SMTP account was accepted by outbox enqueue")
	}
	claimed, ok, err := f.db.ClaimNextOutboxJob(ctx, f.user.ID, "worker-one",
		time.Now().UTC(), -time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim = %+v, %v, %v", claimed, ok, err)
	}
	if err := f.db.MarkOutboxSMTPInFlight(ctx, f.user.ID, job.ID, "worker-one"); err != nil {
		t.Fatal(err)
	}
	recovered, err := f.db.RecoverExpiredOutboxJobs(ctx, f.user.ID, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if recovered != 1 {
		t.Fatalf("recovered=%d, want 1", recovered)
	}
	got, err := f.db.GetOutboxJobForUser(ctx, f.user.ID, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.DeliveryState != OutboxDeliveryUnknown || got.AttentionAt.IsZero() {
		t.Fatalf("recovered state=%q attention=%v, want delivery_unknown with attention", got.DeliveryState, got.AttentionAt)
	}
}

func TestPromoteOutboxMessagePreservesLocalMessageIdentity(t *testing.T) {
	f := newOutboxTestFixture(t, "promote")
	ctx := context.Background()
	job, optimistic := f.enqueue(t, "promote-submit")
	if err := f.db.UpdateMailboxRemoteStatus(ctx, f.user.ID, f.sent.ID, 0, 0, 40, 77); err != nil {
		t.Fatal(err)
	}
	displaced, err := f.db.PromoteOutboxMessage(ctx, f.user.ID, job.ID, 39, 77)
	if err != nil {
		t.Fatal(err)
	}
	if displaced != nil {
		t.Fatalf("unexpected displaced message: %+v", displaced)
	}
	promoted, err := f.db.GetMessageForUser(ctx, f.user.ID, optimistic.ID)
	if err != nil {
		t.Fatal(err)
	}
	if promoted.ID != optimistic.ID || promoted.UID != 39 {
		t.Fatalf("promoted message ID/UID=%d/%d, want %d/39", promoted.ID, promoted.UID, optimistic.ID)
	}
	if uids, err := f.db.MessageUIDsForMailbox(ctx, f.user.ID, f.account.ID, f.sent.ID); err != nil || !slices.Equal(uids, []uint32{39}) {
		t.Fatalf("promoted remote UIDs=%v, %v; want [39]", uids, err)
	}
	var outboxID int64
	if err := f.db.mustDataDB(ctx, f.user.ID).QueryRowContext(ctx,
		`SELECT outbox_job_id FROM messages WHERE user_id = ? AND id = ?`, f.user.ID, optimistic.ID).Scan(&outboxID); err != nil {
		t.Fatal(err)
	}
	if outboxID != 0 {
		t.Fatalf("promoted message outbox_job_id=%d, want 0", outboxID)
	}
	// A retry after promotion must be harmless and preserve the same URL.
	if displaced, err := f.db.PromoteOutboxMessage(ctx, f.user.ID, job.ID, 39, 77); err != nil || displaced != nil {
		t.Fatalf("idempotent promotion displaced=%+v error=%v", displaced, err)
	}
}

func TestOutboxRestartImmediatelyReleasesSafeFilingClaim(t *testing.T) {
	f := newOutboxTestFixture(t, "filing-restart")
	ctx := context.Background()
	job, _ := f.enqueue(t, "filing-restart-submit")
	claimed, ok, err := f.db.ClaimNextOutboxJob(ctx, f.user.ID, "old-worker",
		time.Now().UTC(), time.Hour)
	if err != nil || !ok {
		t.Fatalf("SMTP claim=%+v ok=%v err=%v", claimed, ok, err)
	}
	if err := f.db.MarkOutboxSMTPInFlight(ctx, f.user.ID, job.ID, "old-worker"); err != nil {
		t.Fatal(err)
	}
	if err := f.db.MarkOutboxSMTPAccepted(ctx, f.user.ID, job.ID, "old-worker"); err != nil {
		t.Fatal(err)
	}
	claimed, ok, err = f.db.ClaimNextOutboxJob(ctx, f.user.ID, "old-worker",
		time.Now().UTC(), time.Hour)
	if err != nil || !ok || claimed.DeliveryState != OutboxDeliveryAccepted {
		t.Fatalf("filing claim=%+v ok=%v err=%v", claimed, ok, err)
	}
	recovered, err := f.db.RecoverInterruptedOutboxJobs(ctx, f.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered != 1 {
		t.Fatalf("recovered=%d, want one safe filing claim", recovered)
	}
	reclaimed, ok, err := f.db.ClaimNextOutboxJob(ctx, f.user.ID, "new-worker",
		time.Now().UTC(), time.Hour)
	if err != nil || !ok || reclaimed.ID != job.ID {
		t.Fatalf("restart filing claim=%+v ok=%v err=%v", reclaimed, ok, err)
	}
}

func TestCompletedOutboxHistoryPruneKeepsPromotedSentMessage(t *testing.T) {
	f := newOutboxTestFixture(t, "history-prune")
	ctx := context.Background()
	job, optimistic := f.enqueue(t, "history-prune-submit")
	claimed, ok, err := f.db.ClaimNextOutboxJob(ctx, f.user.ID, "worker",
		time.Now().UTC(), time.Minute)
	if err != nil || !ok {
		t.Fatalf("SMTP claim=%+v ok=%v err=%v", claimed, ok, err)
	}
	if err := f.db.MarkOutboxSMTPInFlight(ctx, f.user.ID, job.ID, "worker"); err != nil {
		t.Fatal(err)
	}
	if err := f.db.MarkOutboxSMTPAccepted(ctx, f.user.ID, job.ID, "worker"); err != nil {
		t.Fatal(err)
	}
	claimed, ok, err = f.db.ClaimNextOutboxJob(ctx, f.user.ID, "worker",
		time.Now().UTC(), time.Minute)
	if err != nil || !ok {
		t.Fatalf("filing claim=%+v ok=%v err=%v", claimed, ok, err)
	}
	if err := f.db.UpdateMailboxRemoteStatus(ctx, f.user.ID, f.sent.ID, 0, 0, 52, 88); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.PromoteOutboxMessage(ctx, f.user.ID, job.ID, 51, 88); err != nil {
		t.Fatal(err)
	}
	if err := f.db.CompleteOutboxFiling(ctx, f.user.ID, job.ID, "worker", 51, 88); err != nil {
		t.Fatal(err)
	}
	blobs, err := f.db.PurgeCompletedOutboxHistory(ctx, f.user.ID, time.Now().UTC().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(blobs) != 1 || blobs[0].ID != f.blob.ID {
		t.Fatalf("history prune blobs=%+v", blobs)
	}
	if _, err := f.db.GetOutboxJobForUser(ctx, f.user.ID, job.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("pruned history read error=%v, want not found", err)
	}
	if _, err := f.db.GetMessageForUser(ctx, f.user.ID, optimistic.ID); err != nil {
		t.Fatalf("history prune removed promoted Sent message: %v", err)
	}
	if deleted, err := f.db.DeleteBlobIfUnreferencedForUser(ctx, f.user.ID, f.blob.ID); err != nil || deleted {
		t.Fatalf("promoted Sent blob deleted=%v error=%v", deleted, err)
	}
}

func TestOutboxAccountPurgesRemovePendingRowsAndReleaseBlob(t *testing.T) {
	for _, test := range []struct {
		name  string
		purge func(context.Context, outboxTestFixture) ([]BlobRecord, []int64, error)
	}{
		{
			name: "IMAP account",
			purge: func(ctx context.Context, f outboxTestFixture) ([]BlobRecord, []int64, error) {
				return f.db.PurgeOutboxJobsForIMAPAccount(ctx, f.user.ID, f.account.ID)
			},
		},
		{
			name: "SMTP account",
			purge: func(ctx context.Context, f outboxTestFixture) ([]BlobRecord, []int64, error) {
				return f.db.PurgeOutboxJobsForSMTPAccount(ctx, f.user.ID, f.smtp.ID)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newOutboxTestFixture(t, "purge-"+strings.ReplaceAll(test.name, " ", "-"))
			ctx := context.Background()
			job, optimistic := f.enqueue(t, "purge-submit")
			blobs, messageIDs, err := test.purge(ctx, f)
			if err != nil {
				t.Fatal(err)
			}
			if len(blobs) != 1 || blobs[0].ID != f.blob.ID ||
				!slices.Equal(messageIDs, []int64{optimistic.ID}) {
				t.Fatalf("purge blobs=%+v messageIDs=%v", blobs, messageIDs)
			}
			if _, err := f.db.GetOutboxJobForUser(ctx, f.user.ID, job.ID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("purged job read error=%v, want not found", err)
			}
			if _, err := f.db.GetMessageForUser(ctx, f.user.ID, optimistic.ID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("purged optimistic message read error=%v, want not found", err)
			}
			deleted, err := f.db.DeleteBlobIfUnreferencedForUser(ctx, f.user.ID, f.blob.ID)
			if err != nil || !deleted {
				t.Fatalf("released blob deleted=%v error=%v", deleted, err)
			}
		})
	}
}

func TestDeletingSMTPAccountKeepsAlreadyAcceptedSentFiling(t *testing.T) {
	f := newOutboxTestFixture(t, "smtp-accepted")
	ctx := context.Background()
	job, optimistic := f.enqueue(t, "accepted-submit")
	claimed, ok, err := f.db.ClaimNextOutboxJob(ctx, f.user.ID, "worker", time.Now().UTC(), time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim=%+v ok=%v err=%v", claimed, ok, err)
	}
	if err := f.db.MarkOutboxSMTPInFlight(ctx, f.user.ID, job.ID, "worker"); err != nil {
		t.Fatal(err)
	}
	if err := f.db.MarkOutboxSMTPAccepted(ctx, f.user.ID, job.ID, "worker"); err != nil {
		t.Fatal(err)
	}
	blobs, messageIDs, err := f.db.PurgeOutboxJobsForSMTPAccount(ctx, f.user.ID, f.smtp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(blobs) != 0 || len(messageIDs) != 0 {
		t.Fatalf("accepted SMTP purge blobs=%+v messageIDs=%v, want retained", blobs, messageIDs)
	}
	if err := f.db.DeleteSMTPAccountForUser(ctx, f.user.ID, f.smtp.ID); err != nil {
		t.Fatalf("delete SMTP account with accepted filing: %v", err)
	}
	retained, err := f.db.GetOutboxJobForUser(ctx, f.user.ID, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if retained.DeliveryState != OutboxDeliveryAccepted {
		t.Fatalf("retained delivery state=%q", retained.DeliveryState)
	}
	if _, err := f.db.GetMessageForUser(ctx, f.user.ID, optimistic.ID); err != nil {
		t.Fatalf("accepted optimistic Sent message was removed: %v", err)
	}
}
