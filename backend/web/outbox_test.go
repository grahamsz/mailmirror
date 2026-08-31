// File overview: Compose queue and native outbox worker integration tests.

package web

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"rolltop/backend/smtpclient"
	"rolltop/backend/store"
	"rolltop/backend/syncer"
)

type testOutboxSender struct {
	count int
	raw   []byte
	err   error
}

func (s *testOutboxSender) Send(_ context.Context, _ store.MailAccount, msg smtpclient.Message) ([]byte, error) {
	raw, _, err := smtpclient.BuildRaw(msg)
	return raw, err
}

func (s *testOutboxSender) SendRawReader(_ context.Context, _ store.MailAccount, _ []string, raw io.Reader) error {
	s.count++
	s.raw, _ = io.ReadAll(raw)
	return s.err
}

type testOutboxAppendFetcher struct {
	*captureAppendFetcher
	boundary      syncer.MailboxAppendBoundary
	existingMatch *syncer.FetchedMessage
}

func (f *testOutboxAppendFetcher) SnapshotMailboxAppendBoundary(context.Context, store.MailAccount, string) (syncer.MailboxAppendBoundary, error) {
	return f.boundary, nil
}

func (f *testOutboxAppendFetcher) SnapshotExactMessageMatches(context.Context, store.MailAccount, string, string, []byte, uint32) (syncer.ExactMessageMatchSnapshot, error) {
	snapshot := syncer.ExactMessageMatchSnapshot{
		UIDValidity: f.boundary.UIDValidity,
		UIDNext:     f.boundary.UIDNext,
	}
	if f.existingMatch != nil {
		snapshot.CandidateUIDs = []uint32{f.existingMatch.UID}
		snapshot.MatchingUIDs = []uint32{f.existingMatch.UID}
	}
	return snapshot, nil
}

func (f *testOutboxAppendFetcher) FetchMessage(_ context.Context, _ store.MailAccount, mailbox string, uid uint32) (syncer.FetchedMessage, error) {
	if f.existingMatch != nil && f.existingMatch.UID == uid {
		result := *f.existingMatch
		result.Mailbox = mailbox
		return result, nil
	}
	return syncer.FetchedMessage{}, store.ErrNotFound
}

func TestQueueComposeReturnsBeforeSMTPAndCreatesSentMessage(t *testing.T) {
	ctx := context.Background()
	server, user, fromID, oldSender, _ := setupAutocryptComposeTest(t, ctx, false)
	job, optimistic, err := server.queueCompose(ctx, currentUser{User: user}, composeForm{
		SubmissionKey:  "browser-stable-request",
		To:             "recipient@example.test",
		Subject:        "Visible before SMTP",
		Body:           "queued body",
		FromIdentityID: fromID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if oldSender.count != 0 {
		t.Fatalf("queueCompose performed synchronous SMTP sends=%d", oldSender.count)
	}
	if job.DeliveryState != store.OutboxDeliveryQueued || optimistic.ID != job.OptimisticMessageID {
		t.Fatalf("queued job=%+v optimistic=%+v", job, optimistic)
	}
	messages, err := server.store.ListMessagesForMailbox(ctx, user.ID, job.SentMailboxID, 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].ID != optimistic.ID || messages[0].Subject != "Visible before SMTP" {
		t.Fatalf("Sent mailbox messages=%+v, want optimistic message %d", messages, optimistic.ID)
	}
	states, err := server.store.OutboxMessageStatesForUser(ctx, user.ID, []int64{optimistic.ID})
	if err != nil {
		t.Fatal(err)
	}
	if states[optimistic.ID].DeliveryState != store.OutboxDeliveryQueued {
		t.Fatalf("optimistic state=%+v", states[optimistic.ID])
	}
	other, err := server.store.CreateUser(ctx, "outbox-other@example.test", "Other", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/outbox", nil)
	request = request.WithContext(context.WithValue(request.Context(), userContextKey, currentUser{User: other}))
	server.apiOutbox(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("foreign outbox status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var foreignResponse struct {
		Jobs []apiOutboxJob `json:"jobs"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &foreignResponse); err != nil {
		t.Fatal(err)
	}
	if len(foreignResponse.Jobs) != 0 {
		t.Fatalf("foreign user saw outbox jobs=%+v", foreignResponse.Jobs)
	}

	againJob, againMessage, err := server.queueCompose(ctx, currentUser{User: user}, composeForm{
		SubmissionKey:  "browser-stable-request",
		To:             "different@example.test",
		Subject:        "Must not duplicate",
		Body:           "different",
		FromIdentityID: fromID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if againJob.ID != job.ID || againMessage.ID != optimistic.ID {
		t.Fatalf("idempotent queue returned job/message=%d/%d, want %d/%d",
			againJob.ID, againMessage.ID, job.ID, optimistic.ID)
	}
}

func TestComposeAPIAcknowledgesDurableQueueWithAcceptedStatus(t *testing.T) {
	ctx := context.Background()
	server, user, fromID, sender, _ := setupAutocryptComposeTest(t, ctx, false)
	body := strings.NewReader(`{
		"submission_key":"api-stable-request",
		"to":"recipient@example.test",
		"subject":"Queued by API",
		"body":"body",
		"from_identity_id":` + strconv.FormatInt(fromID, 10) + `
	}`)
	request := httptest.NewRequest(http.MethodPost, "/api/compose", body)
	request.Header.Set("Content-Type", "application/json")
	const csrfBase = "outbox-compose-csrf"
	request.AddCookie(&http.Cookie{Name: csrfCookie, Value: csrfBase})
	request.Header.Set("X-CSRF-Token", server.csrfForBase(csrfBase))
	request = request.WithContext(context.WithValue(request.Context(), userContextKey, currentUser{User: user}))
	recorder := httptest.NewRecorder()
	server.apiCompose(recorder, request)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("compose status=%d body=%s, want 202", recorder.Code, recorder.Body.String())
	}
	if sender.count != 0 {
		t.Fatalf("compose API waited for SMTP sends=%d", sender.count)
	}
	var response struct {
		Queued    bool  `json:"queued"`
		SendID    int64 `json:"send_id"`
		MessageID int64 `json:"message_id"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.Queued || response.SendID <= 0 || response.MessageID <= 0 {
		t.Fatalf("compose queue response=%+v", response)
	}
	message, err := server.store.GetMessageForUser(ctx, user.ID, response.MessageID)
	if err != nil {
		t.Fatal(err)
	}
	if message.Subject != "Queued by API" {
		t.Fatalf("optimistic Sent subject=%q", message.Subject)
	}
}

func TestOutboxWorkerDeliversThenPromotesSameSentMessage(t *testing.T) {
	ctx := context.Background()
	server, user, fromID, _, _ := setupAutocryptComposeTest(t, ctx, false)
	sender := &testOutboxSender{}
	server.sender = sender
	fetcher := &testOutboxAppendFetcher{
		captureAppendFetcher: &captureAppendFetcher{nextUID: 71, uidValidity: 909},
		boundary:             syncer.MailboxAppendBoundary{UIDValidity: 909, UIDNext: 71},
	}
	server.syncer.Fetcher = fetcher
	server.syncer.Store = server.store
	server.syncer.Blobs = server.blobs
	server.outboxWorkerID = "test-worker"
	job, optimistic, err := server.queueCompose(ctx, currentUser{User: user}, composeForm{
		SubmissionKey:  "worker-success",
		To:             "recipient@example.test",
		Subject:        "Background delivery",
		Body:           "body",
		FromIdentityID: fromID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := server.store.UpdateMailboxRemoteStatus(ctx, user.ID, job.SentMailboxID, 0, 0, 71, 909); err != nil {
		t.Fatal(err)
	}

	claimed, ok, err := server.store.ClaimNextOutboxJob(ctx, user.ID, server.outboxWorkerID, time.Now().UTC(), time.Minute)
	if err != nil || !ok {
		t.Fatalf("SMTP claim=%+v ok=%v err=%v", claimed, ok, err)
	}
	server.processClaimedOutboxJob(ctx, claimed)
	afterSMTP, err := server.store.GetOutboxJobForUser(ctx, user.ID, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sender.count != 1 || afterSMTP.DeliveryState != store.OutboxDeliveryAccepted ||
		afterSMTP.FilingState != store.OutboxFilingPending {
		t.Fatalf("after SMTP sender.count=%d job=%+v", sender.count, afterSMTP)
	}
	if !strings.Contains(string(sender.raw), "Subject: Background delivery") {
		t.Fatalf("streamed SMTP payload=%q", sender.raw)
	}

	claimed, ok, err = server.store.ClaimNextOutboxJob(ctx, user.ID, server.outboxWorkerID, time.Now().UTC(), time.Minute)
	if err != nil || !ok {
		t.Fatalf("filing claim=%+v ok=%v err=%v", claimed, ok, err)
	}
	server.processClaimedOutboxJob(ctx, claimed)
	complete, err := server.store.GetOutboxJobForUser(ctx, user.ID, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if complete.DeliveryState != store.OutboxDeliveryAccepted || complete.FilingState != store.OutboxFilingComplete {
		t.Fatalf("completed job=%+v", complete)
	}
	promoted, err := server.store.GetMessageForUser(ctx, user.ID, optimistic.ID)
	if err != nil {
		t.Fatal(err)
	}
	if promoted.ID != optimistic.ID || promoted.UID != 71 {
		t.Fatalf("promoted Sent identity=%d UID=%d, want %d/71", promoted.ID, promoted.UID, optimistic.ID)
	}
	states, err := server.store.OutboxMessageStatesForUser(ctx, user.ID, []int64{optimistic.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := states[optimistic.ID]; exists {
		t.Fatalf("completed Sent message retained a transient outbox annotation: %+v", states[optimistic.ID])
	}
}

func TestOutboxWorkerDoesNotRetryUncertainSMTPDelivery(t *testing.T) {
	ctx := context.Background()
	server, user, fromID, _, _ := setupAutocryptComposeTest(t, ctx, false)
	sender := &testOutboxSender{err: &smtpclient.DeliveryError{
		Phase: smtpclient.DeliveryPhaseAccept, Outcome: smtpclient.DeliveryUnknown,
		Err: context.DeadlineExceeded,
	}}
	server.sender = sender
	server.outboxWorkerID = "test-worker"
	job, _, err := server.queueCompose(ctx, currentUser{User: user}, composeForm{
		SubmissionKey:  "worker-uncertain",
		To:             "recipient@example.test",
		Subject:        "Maybe sent",
		Body:           "body",
		FromIdentityID: fromID,
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := server.store.ClaimNextOutboxJob(ctx, user.ID, server.outboxWorkerID, time.Now().UTC(), time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim=%+v ok=%v err=%v", claimed, ok, err)
	}
	server.processClaimedOutboxJob(ctx, claimed)
	got, err := server.store.GetOutboxJobForUser(ctx, user.ID, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.DeliveryState != store.OutboxDeliveryUnknown || got.AttentionAt.IsZero() {
		t.Fatalf("uncertain job=%+v", got)
	}
	if _, claimable, err := server.store.ClaimNextOutboxJob(ctx, user.ID, "other-worker",
		time.Now().UTC().Add(24*time.Hour), time.Minute); err != nil || claimable {
		t.Fatalf("uncertain delivery claimable=%v err=%v, want false", claimable, err)
	}
}

func TestOutboxWorkerUsesMatchingSentCopyWithoutAppendingDuplicate(t *testing.T) {
	ctx := context.Background()
	server, user, fromID, _, _ := setupAutocryptComposeTest(t, ctx, false)
	sender := &testOutboxSender{}
	appendCount := 0
	fetcher := &testOutboxAppendFetcher{
		captureAppendFetcher: &captureAppendFetcher{
			nextUID: 71, uidValidity: 909,
			onAppend: func() { appendCount++ },
		},
		boundary: syncer.MailboxAppendBoundary{UIDValidity: 909, UIDNext: 71},
		existingMatch: &syncer.FetchedMessage{
			UID: 70, UIDValidity: 909, Flags: []string{`\Seen`},
		},
	}
	server.sender = sender
	server.syncer.Fetcher = fetcher
	server.syncer.Store = server.store
	server.syncer.Blobs = server.blobs
	server.outboxWorkerID = "test-worker"
	job, optimistic, err := server.queueCompose(ctx, currentUser{User: user}, composeForm{
		SubmissionKey:  "server-saved-copy",
		To:             "recipient@example.test",
		Subject:        "Already saved by SMTP provider",
		Body:           "body",
		FromIdentityID: fromID,
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := server.store.ClaimNextOutboxJob(ctx, user.ID, server.outboxWorkerID,
		time.Now().UTC(), time.Minute)
	if err != nil || !ok {
		t.Fatalf("SMTP claim=%+v ok=%v err=%v", claimed, ok, err)
	}
	server.processClaimedOutboxJob(ctx, claimed)
	claimed, ok, err = server.store.ClaimNextOutboxJob(ctx, user.ID, server.outboxWorkerID,
		time.Now().UTC(), time.Minute)
	if err != nil || !ok {
		t.Fatalf("filing claim=%+v ok=%v err=%v", claimed, ok, err)
	}
	server.processClaimedOutboxJob(ctx, claimed)
	if appendCount != 0 {
		t.Fatalf("Sent filing appended %d duplicate copies, want 0", appendCount)
	}
	complete, err := server.store.GetOutboxJobForUser(ctx, user.ID, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	promoted, err := server.store.GetMessageForUser(ctx, user.ID, optimistic.ID)
	if err != nil {
		t.Fatal(err)
	}
	if complete.FilingState != store.OutboxFilingComplete || promoted.UID != 70 {
		t.Fatalf("existing Sent copy filing job=%+v message UID=%d", complete, promoted.UID)
	}
}
