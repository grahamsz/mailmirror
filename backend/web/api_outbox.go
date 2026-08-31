// File overview: Tenant-scoped outbox listing and explicit retry/cancel/acknowledge actions.

package web

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"

	"rolltop/backend/store"
)

type apiOutboxJob struct {
	ID                  int64  `json:"id"`
	MessageID           int64  `json:"message_id"`
	Subject             string `json:"subject"`
	DeliveryState       string `json:"delivery_state"`
	FilingState         string `json:"filing_state"`
	AttemptCount        int    `json:"attempt_count"`
	FilingAttemptCount  int    `json:"filing_attempt_count"`
	NextAttemptAt       string `json:"next_attempt_at"`
	LastError           string `json:"last_error"`
	NeedsAttention      bool   `json:"needs_attention"`
	CanCancel           bool   `json:"can_cancel"`
	CanRetry            bool   `json:"can_retry"`
	RetryMayDuplicate   bool   `json:"retry_may_duplicate"`
	SMTPAcceptedAt      string `json:"smtp_accepted_at"`
	CompletedAt         string `json:"completed_at"`
	CreatedAt           string `json:"created_at"`
	UpdatedAt           string `json:"updated_at"`
	RawSize             int64  `json:"raw_size"`
	AppendedUID         uint32 `json:"appended_uid"`
	AppendedUIDValidity uint32 `json:"appended_uid_validity"`
}

func (s *Server) apiOutbox(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	cu, ok := s.requireAPIAuth(w, r)
	if !ok {
		return
	}
	jobs, err := s.store.ListOutboxJobsForUser(r.Context(), cu.User.ID, 100)
	if err != nil {
		s.serverError(w, err)
		return
	}
	messageIDs := make([]int64, 0, len(jobs))
	for _, job := range jobs {
		if job.OptimisticMessageID > 0 {
			messageIDs = append(messageIDs, job.OptimisticMessageID)
		}
	}
	messages, err := s.store.ListMessagesByIDsForUser(r.Context(), cu.User.ID, messageIDs)
	if err != nil {
		s.serverError(w, err)
		return
	}
	subjects := make(map[int64]string, len(messages))
	for _, message := range messages {
		subjects[message.ID] = message.Subject
	}
	out := make([]apiOutboxJob, 0, len(jobs))
	for _, job := range jobs {
		out = append(out, apiOutboxJobFrom(job, subjects[job.OptimisticMessageID]))
	}
	summary, err := s.store.OutboxSummaryForUser(r.Context(), cu.User.ID)
	if err != nil {
		s.serverError(w, err)
		return
	}
	writeJSONCached(w, r, map[string]any{
		"jobs":    out,
		"summary": apiOutboxSummary(summary),
	})
}

func (s *Server) apiOutboxPath(w http.ResponseWriter, r *http.Request, rest string) {
	cu, ok := s.requireAPIAuth(w, r)
	if !ok {
		return
	}
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) != 2 || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || id <= 0 {
		http.NotFound(w, r)
		return
	}
	if !s.verifyCSRF(w, r) {
		return
	}
	job, err := s.store.GetOutboxJobForUser(r.Context(), cu.User.ID, id)
	if store.IsNotFound(err) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, err)
		return
	}
	switch parts[1] {
	case "acknowledge":
		if err := s.store.AcknowledgeOutboxJob(r.Context(), cu.User.ID, id); err != nil {
			s.serverError(w, err)
			return
		}
	case "cancel":
		messageID, blobRecord, err := s.store.CancelOutboxJob(r.Context(), cu.User.ID, id)
		if err != nil {
			writeAPIError(w, http.StatusConflict, err.Error())
			return
		}
		if messageID > 0 && s.search != nil {
			_ = s.search.DeleteMessage(r.Context(), cu.User.ID, messageID)
		}
		deleted, cleanupErr := s.store.DeleteBlobIfUnreferencedForUser(r.Context(), cu.User.ID, blobRecord.ID)
		if cleanupErr != nil {
			log.Printf("cleanup canceled outbox blob user_id=%d outbox_id=%d: %v", cu.User.ID, id, cleanupErr)
		}
		if deleted && s.blobs != nil {
			_ = s.blobs.DeleteUserBlob(cu.User.ID, blobRecord.Path)
		}
	case "retry":
		if job.DeliveryState == store.OutboxDeliveryAccepted &&
			job.FilingState == store.OutboxFilingAttention {
			err = s.store.RequeueOutboxFiling(r.Context(), cu.User.ID, id)
		} else {
			var body struct {
				RetryAnyway bool `json:"retry_anyway"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if job.DeliveryState == store.OutboxDeliveryUnknown && !body.RetryAnyway {
				writeAPIError(w, http.StatusConflict,
					"Delivery may already have succeeded. Confirm retry anyway to accept the duplicate risk.")
				return
			}
			err = s.store.RequeueOutboxJob(r.Context(), cu.User.ID, id, body.RetryAnyway)
		}
		if err != nil {
			writeAPIError(w, http.StatusConflict, "This message is not waiting for a retry.")
			return
		}
	default:
		http.NotFound(w, r)
		return
	}
	s.notifyOutboxChanged(cu.User.ID)
	s.wakeOutboxWorker()
	writeJSON(w, map[string]any{"ok": true})
}

func apiOutboxJobFrom(job store.OutboxJob, subject string) apiOutboxJob {
	attention := !job.AttentionAt.IsZero() &&
		(job.AcknowledgedAt.IsZero() || job.AttentionAt.After(job.AcknowledgedAt))
	canCancel := job.DeliveryState == store.OutboxDeliveryQueued ||
		job.DeliveryState == store.OutboxDeliveryRetryWait ||
		job.DeliveryState == store.OutboxDeliveryFailed
	canRetry := job.DeliveryState == store.OutboxDeliveryFailed ||
		job.DeliveryState == store.OutboxDeliveryUnknown ||
		(job.DeliveryState == store.OutboxDeliveryAccepted &&
			job.FilingState == store.OutboxFilingAttention)
	return apiOutboxJob{
		ID:                  job.ID,
		MessageID:           job.OptimisticMessageID,
		Subject:             subject,
		DeliveryState:       job.DeliveryState,
		FilingState:         job.FilingState,
		AttemptCount:        job.AttemptCount,
		FilingAttemptCount:  job.FilingAttemptCount,
		NextAttemptAt:       timeString(job.NextAttemptAt),
		LastError:           job.LastError,
		NeedsAttention:      attention,
		CanCancel:           canCancel,
		CanRetry:            canRetry,
		RetryMayDuplicate:   job.DeliveryState == store.OutboxDeliveryUnknown,
		SMTPAcceptedAt:      timeString(job.SMTPAcceptedAt),
		CompletedAt:         timeString(job.CompletedAt),
		CreatedAt:           timeString(job.CreatedAt),
		UpdatedAt:           timeString(job.UpdatedAt),
		RawSize:             job.RawSize,
		AppendedUID:         job.AppendedUID,
		AppendedUIDValidity: job.AppendedUIDValidity,
	}
}

func apiOutboxSummary(summary store.OutboxSummary) map[string]any {
	return map[string]any{
		"active":          summary.Active,
		"needs_attention": summary.NeedsAttention,
		"latest_id":       summary.LatestID,
	}
}
