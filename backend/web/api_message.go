// File overview: Message detail, attachment, remote image, unsubscribe, and metadata API handlers.

package web

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/mail"
	"strconv"
	"strings"
	"time"

	"rolltop/backend/mailparse"
	"rolltop/backend/plugins"
	"rolltop/backend/search"
	"rolltop/backend/store"
)

// apiMessagePath is the subrouter for /api/messages/:id. It keeps per-message
// actions in one place while each action still performs auth, CSRF, and user-scoped
// store checks independently.
func (s *Server) apiMessagePath(w http.ResponseWriter, r *http.Request, rest string) {
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || id <= 0 {
		http.NotFound(w, r)
		return
	}
	if len(parts) == 1 {
		s.apiMessage(w, r, id)
		return
	}
	if len(parts) == 2 && parts[1] == "prefetch" {
		s.apiMessagePrefetch(w, r, id)
		return
	}
	if len(parts) == 2 && parts[1] == "load-status" {
		s.apiMessageLoadStatus(w, r, id)
		return
	}
	if len(parts) == 2 && parts[1] == "original" {
		s.apiMessageOriginal(w, r, id)
		return
	}
	if len(parts) == 2 && parts[1] == "search-explanation" {
		s.apiMessageSearchExplanation(w, r, id)
		return
	}
	if len(parts) == 2 && parts[1] == "move" {
		s.apiMoveMessage(w, r, id)
		return
	}
	if len(parts) == 2 && parts[1] == "star" {
		s.apiSetMessageStarred(w, r, id)
		return
	}
	if len(parts) == 2 && parts[1] == "snooze" {
		s.apiMessageSnooze(w, r, id)
		return
	}
	if len(parts) == 2 && parts[1] == "unsubscribe" {
		if !s.pluginEnabled(r.Context(), plugins.OneClickUnsubscribe) {
			http.NotFound(w, r)
			return
		}
		s.apiOneClickUnsubscribe(w, r, id)
		return
	}
	if len(parts) == 3 && parts[1] == "contacts" && parts[2] == "add-sender" {
		s.apiAddSenderContact(w, r, id)
		return
	}
	if len(parts) == 3 && parts[1] == "images" && parts[2] == "trust" {
		if !s.pluginEnabled(r.Context(), plugins.TrustedImageSources) {
			http.NotFound(w, r)
			return
		}
		s.apiTrustImages(w, r, id)
		return
	}
	http.NotFound(w, r)
}

// apiMessagePrefetch hydrates locally cached display bodies without opening the
// conversation. Unlike apiMessage, it must never mutate read state or queue IMAP flags.
func (s *Server) apiMessagePrefetch(w http.ResponseWriter, r *http.Request, id int64) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	cu, ok := s.requireAPIAuth(w, r)
	if !ok {
		return
	}
	msg, err := s.store.GetMessageEnvelopeForUser(r.Context(), cu.User.ID, id)
	if store.IsNotFound(err) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, err)
		return
	}
	thread, err := s.store.ListThreadMessagesForUser(r.Context(), cu.User.ID, msg)
	if err != nil {
		s.serverError(w, err)
		return
	}
	for _, threadMsg := range thread {
		s.displayBodiesForMessage(r.Context(), cu.User.ID, threadMsg)
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, map[string]any{"ok": true, "message_id": id, "thread_messages": len(thread)})
}

// apiMessage returns the full thread around one message. threadViewsForMessage may
// hydrate pruned bodies from local blobs or IMAP before this method serializes the
// render-ready body docs for React.
func (s *Server) apiMessage(w http.ResponseWriter, r *http.Request, id int64) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	cu, ok := s.requireAPIAuth(w, r)
	if !ok {
		return
	}
	timing := newSearchTiming()
	stop := timing.measure(&timing.filter)
	msg, err := s.store.GetMessageEnvelopeForUser(r.Context(), cu.User.ID, id)
	stop()
	if store.IsNotFound(err) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, err)
		return
	}
	stop = timing.measure(&timing.hydrate)
	views, msg, err := s.threadViewsForMessageTimed(r.Context(), cu, msg, r.URL.Query().Get("images") == "1", r.URL.Query().Get("q"), timing)
	stop()
	if err != nil {
		s.serverError(w, err)
		return
	}
	stop = timing.measure(&timing.render)
	thread := s.apiThreadMessagesTimed(r.Context(), cu.User.ID, views, timing)
	stop()
	timing.seeds = len(views)
	// Identity choices may need a contended SQLite read while a mirror is
	// writing. They are only needed after the user presses Reply/Forward, where
	// /api/compose already loads the complete, security-aware choices. Keep that
	// optional compose work out of the message-open critical path.
	snoozedUntil := ""
	if snooze, snoozeErr := s.store.MessageSnoozeForUser(r.Context(), cu.User.ID, msg.ID); snoozeErr == nil {
		if snooze.SnoozedUntil.After(time.Now()) {
			snoozedUntil = timeString(snooze.SnoozedUntil)
		} else {
			acknowledged, ackErr := s.store.AcknowledgeDueSnoozeForUser(r.Context(), cu.User.ID, msg.ID, time.Now().UTC())
			if ackErr != nil {
				s.serverError(w, ackErr)
				return
			}
			if acknowledged {
				s.notifySnoozeStateChanged(cu.User.ID)
			}
		}
	} else if !errors.Is(snoozeErr, sql.ErrNoRows) {
		s.serverError(w, snoozeErr)
		return
	}
	writeMessageTimingHeaders(w, timing)
	writeJSONCached(w, r, map[string]any{
		"message":         apiMessageFromRecord(msg, msg.BodyText),
		"thread":          thread,
		"compose_from":    "",
		"from_identities": []apiComposeIdentity{},
		"mailbox_id":      msg.MailboxID,
		"raw_blob_url":    fmt.Sprintf("/blobs/%d", msg.BlobID),
		"conversation":    len(views),
		"snoozed_until":   snoozedUntil,
		"showing_images":  r.URL.Query().Get("images") == "1",
	})
}

type apiMessageOriginalSource struct {
	Filename string `json:"filename"`
	Source   string `json:"source"`
}

// apiMessageOriginal returns raw RFC822 source for one user-owned message. The
// browser requests this from the message action menu and security opener because raw
// messages can be large when they include attachments.
func (s *Server) apiMessageOriginal(w http.ResponseWriter, r *http.Request, id int64) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	cu, ok := s.requireAPIAuth(w, r)
	if !ok {
		return
	}
	msg, err := s.store.GetMessageForUser(r.Context(), cu.User.ID, id)
	if store.IsNotFound(err) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, err)
		return
	}
	raw, err := s.rawMessageBytes(r.Context(), cu.User.ID, msg)
	if err != nil {
		writeAPIError(w, http.StatusGone, "original message source is not available")
		return
	}
	writeJSON(w, apiMessageOriginalSource{
		Filename: rawMessageFilename(msg),
		Source:   strings.ToValidUTF8(string(raw), "\uFFFD"),
	})
}

func rawMessageFilename(msg store.MessageRecord) string {
	name := strings.TrimSpace(msg.Subject)
	if name == "" {
		name = fmt.Sprintf("message-%d", msg.ID)
	}
	name = strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|':
			return '-'
		default:
			return r
		}
	}, name)
	name = strings.TrimSpace(name)
	if name == "" {
		name = fmt.Sprintf("message-%d", msg.ID)
	}
	return name + ".eml"
}

type apiSearchFieldMatch struct {
	Field string   `json:"field"`
	Terms []string `json:"terms"`
}

type apiSearchTermContribution struct {
	Field         string  `json:"field"`
	Section       string  `json:"section"`
	Term          string  `json:"term"`
	QueryTerm     string  `json:"query_term"`
	Score         float64 `json:"score"`
	TermFrequency float64 `json:"term_frequency,omitempty"`
	FieldNorm     float64 `json:"field_norm,omitempty"`
	IDF           float64 `json:"idf,omitempty"`
	QueryWeight   float64 `json:"query_weight,omitempty"`
	Boost         float64 `json:"boost,omitempty"`
	QueryNorm     float64 `json:"query_norm,omitempty"`
}

type apiSearchBoost struct {
	Kind        string  `json:"kind"`
	Label       string  `json:"label"`
	Description string  `json:"description"`
	Value       string  `json:"value,omitempty"`
	Boost       float64 `json:"boost,omitempty"`
}

type apiScoreExplanation struct {
	Value    float64                `json:"value"`
	Message  string                 `json:"message"`
	Children []*apiScoreExplanation `json:"children,omitempty"`
}

// apiMessageSearchExplanation is intentionally on-demand. It re-runs the active
// search against one user-owned message with Bleve explanations enabled, then adds
// Rolltop-level labels for ranking boosts that are otherwise hard to interpret
// from the raw scorer tree.
func (s *Server) apiMessageSearchExplanation(w http.ResponseWriter, r *http.Request, id int64) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	cu, ok := s.requireAPIAuth(w, r)
	if !ok {
		return
	}
	if s.search == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "search is not configured")
		return
	}
	msg, err := s.store.GetMessageForUser(r.Context(), cu.User.ID, id)
	if store.IsNotFound(err) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, err)
		return
	}
	rawQuery := strings.TrimSpace(r.URL.Query().Get("q"))
	if rawQuery == "" {
		writeAPIError(w, http.StatusBadRequest, "search query is required")
		return
	}
	query, _ := stripStarSearchOperators(rawQuery)
	cleanQuery, mailboxFilter, err := s.searchMailboxFilter(r.Context(), cu.User.ID, query)
	if err != nil {
		s.serverError(w, err)
		return
	}
	query = strings.TrimSpace(cleanQuery)
	if query == "" {
		writeAPIError(w, http.StatusBadRequest, "search query has no explainable text")
		return
	}
	exactHitID, _ := strconv.ParseInt(strings.TrimSpace(r.URL.Query().Get("hit")), 10, 64)
	threadMessages, err := s.store.ListThreadMessagesForUser(r.Context(), cu.User.ID, msg)
	if err != nil {
		s.serverError(w, err)
		return
	}
	candidateIDs := make([]int64, 0, len(threadMessages)+2)
	candidateMessages := make([]store.MessageRecord, 0, len(threadMessages)+2)
	messagesByID := map[int64]store.MessageRecord{}
	seenCandidates := map[int64]bool{}
	appendCandidate := func(candidate store.MessageRecord) {
		if candidate.ID == 0 || seenCandidates[candidate.ID] || !mailboxFilter.matches(candidate) {
			return
		}
		seenCandidates[candidate.ID] = true
		candidateIDs = append(candidateIDs, candidate.ID)
		candidateMessages = append(candidateMessages, candidate)
		messagesByID[candidate.ID] = candidate
	}

	// Prefer explaining the message whose menu the user opened. The optional hit
	// parameter is only a fallback that ties a conversation view back to the exact
	// Bleve result row that opened it.
	appendCandidate(msg)
	if exactHitID > 0 && exactHitID != msg.ID {
		hitMsg, err := s.store.GetMessageForUser(r.Context(), cu.User.ID, exactHitID)
		if err != nil && !store.IsNotFound(err) {
			s.serverError(w, err)
			return
		}
		if err == nil {
			appendCandidate(hitMsg)
		}
	}
	for _, threadMsg := range threadMessages {
		appendCandidate(threadMsg)
	}
	if len(candidateIDs) == 0 {
		writeJSON(w, map[string]any{
			"matched": false,
			"query":   query,
			"reason":  "No message in this conversation is inside the search mailbox filter.",
		})
		return
	}
	if _, repairErr := s.ensureSearchDocuments(r.Context(), cu.User.ID, candidateMessages); repairErr != nil {
		s.serverError(w, repairErr)
		return
	}
	opts, _ := s.searchExplanationOptions(r.Context(), cu.User, msg)
	var result search.ExplanationResult
	var matched bool
	if seenCandidates[msg.ID] {
		result, matched, err = s.search.ExplainMessageWithOptions(r.Context(), cu.User.ID, msg.ID, query, opts)
		if err != nil {
			s.serverError(w, err)
			return
		}
	}
	if !matched && exactHitID > 0 && exactHitID != msg.ID && seenCandidates[exactHitID] {
		result, matched, err = s.search.ExplainMessageWithOptions(r.Context(), cu.User.ID, exactHitID, query, opts)
		if err != nil {
			s.serverError(w, err)
			return
		}
	}
	if !matched {
		result, matched, err = s.search.ExplainMessagesWithOptions(r.Context(), cu.User.ID, candidateIDs, query, opts)
		if err != nil {
			s.serverError(w, err)
			return
		}
	}
	if !matched {
		writeJSON(w, map[string]any{
			"matched":      false,
			"query":        query,
			"message_ids":  candidateIDs,
			"requested_id": msg.ID,
			"hit_id":       exactHitID,
			"reason":       "Bleve did not match any checked message for the current query. The local SQLite copy is present, but the query still did not match after a lightweight search-index repair.",
		})
		return
	}
	explainedMsg := messagesByID[result.ID]
	if explainedMsg.ID == 0 {
		explainedMsg = msg
	}
	opts, rankingBoosts := s.searchExplanationOptions(r.Context(), cu.User, explainedMsg)
	writeJSON(w, map[string]any{
		"matched":              true,
		"query":                query,
		"message_id":           result.ID,
		"requested_message_id": msg.ID,
		"score":                result.Score,
		"terms":                result.Terms,
		"query_terms":          result.QueryTerms,
		"fields":               result.Fields,
		"field_matches":        apiSearchFieldMatches(result.FieldMatches),
		"term_contributions":   apiSearchTermContributions(result.TermContributions),
		"boosts":               apiSearchBoosts(cu.User, explainedMsg, rankingBoosts, !searchQueryHasDateOperator(query)),
		"raw":                  apiScoreExplanationFromRaw(result.Raw, 0),
	})
}

func (s *Server) searchExplanationOptions(ctx context.Context, user store.User, msg store.MessageRecord) (search.SearchOptions, []apiSearchBoost) {
	opts := s.searchOptionsWithRankingBoosts(ctx, user)
	messageSender := store.SenderIdentity(msg.FromAddr)
	var boosts []apiSearchBoost
	if messageSender == "" {
		return opts, boosts
	}
	if opts.Behavior.SenderBoostScale > 0 {
		if stats, err := s.store.ListReadSenderStatsForUser(ctx, user.ID, 40); err == nil {
			for _, stat := range stats {
				if strings.EqualFold(stat.Sender, messageSender) {
					boosts = append(boosts, apiSearchBoost{
						Kind:        "sender",
						Label:       "Familiar sender",
						Description: fmt.Sprintf("%d of %d messages from this sender are read.", stat.ReadCount, stat.TotalCount),
						Value:       fmt.Sprintf("%s sender history", searchSenderHistoryWeightForUser(user)),
						Boost:       stat.Boost * opts.Behavior.SenderBoostScale,
					})
					break
				}
			}
		}
	}
	if opts.Behavior.ContactBoostScale > 0 {
		if contact, err := s.store.GetContactByEmailForUser(ctx, user.ID, messageSender); err == nil && !contact.IsMe {
			label := strings.TrimSpace(contact.DisplayName)
			if label == "" {
				label = messageSender
			}
			boosts = append(boosts, apiSearchBoost{
				Kind:        "contacts",
				Label:       "In contacts",
				Description: fmt.Sprintf("Sender is in your contacts as %s.", label),
				Value:       fmt.Sprintf("%s contact boost", searchContactBoostWeightForUser(user)),
				Boost:       opts.Behavior.ContactBoostScale,
			})
		}
	}
	return opts, boosts
}

func apiSearchFieldMatches(matches []search.FieldTermMatch) []apiSearchFieldMatch {
	out := make([]apiSearchFieldMatch, 0, len(matches))
	for _, match := range matches {
		out = append(out, apiSearchFieldMatch{Field: match.Field, Terms: match.Terms})
	}
	return out
}

func apiSearchTermContributions(contributions []search.TermContribution) []apiSearchTermContribution {
	out := make([]apiSearchTermContribution, 0, len(contributions))
	for _, contribution := range contributions {
		out = append(out, apiSearchTermContribution{
			Field:         contribution.Field,
			Section:       apiSearchFieldLabel(contribution.Field),
			Term:          contribution.Term,
			QueryTerm:     contribution.QueryTerm,
			Score:         contribution.Score,
			TermFrequency: contribution.TermFrequency,
			FieldNorm:     contribution.FieldNorm,
			IDF:           contribution.IDF,
			QueryWeight:   contribution.QueryWeight,
			Boost:         contribution.Boost,
			QueryNorm:     contribution.QueryNorm,
		})
	}
	return out
}

func apiSearchFieldLabel(field string) string {
	switch field {
	case "subject", "subject_compound":
		return "Subject"
	case "from", "from_compound", "from_domain":
		return "Sender"
	case "to":
		return "To"
	case "cc":
		return "Cc"
	case "body":
		return "Body"
	case "attachment_names":
		return "Attachment name"
	case "attachment_types":
		return "Attachment type"
	case "attachments":
		return "Attachment text"
	case "compound":
		return "Joined words"
	case "message_id":
		return "Message ID"
	default:
		return strings.ReplaceAll(field, "_", " ")
	}
}

func apiSearchBoosts(user store.User, msg store.MessageRecord, rankingBoosts []apiSearchBoost, includeRecency bool) []apiSearchBoost {
	var out []apiSearchBoost
	if includeRecency {
		if recency := apiRecencySearchBoost(user, msg); recency != nil {
			out = append(out, *recency)
		}
	}
	out = append(out, rankingBoosts...)
	return out
}

func searchQueryHasDateOperator(query string) bool {
	for _, token := range strings.Fields(strings.ToLower(query)) {
		token = strings.TrimPrefix(token, "-")
		if strings.HasPrefix(token, "after:") || strings.HasPrefix(token, "before:") || strings.HasPrefix(token, "year:") {
			return true
		}
	}
	return false
}

func apiRecencySearchBoost(user store.User, msg store.MessageRecord) *apiSearchBoost {
	bias := normalizedRecencyBiasForUser(user)
	if bias == "none" {
		return nil
	}
	messageTime := msg.Date
	if messageTime.IsZero() {
		messageTime = msg.InternalDate
	}
	if messageTime.IsZero() {
		return nil
	}
	age := time.Since(messageTime)
	if age < 0 {
		age = 0
	}
	for _, bucket := range recencyExplanationBuckets(bias) {
		if age <= bucket.age {
			label := "Recent mail"
			description := fmt.Sprintf("Message date is within %s; recency profile is %s. This nudge contributes to the final rank score but is not required for matching.", bucket.label, bias)
			value := fmt.Sprintf("%s freshness bucket", bucket.label)
			if bucket.boost < 0 {
				label = "Older mail"
				description = fmt.Sprintf("Message date falls in the %s bucket; recency profile is %s. This small penalty helps newer equally relevant mail stay ahead without blocking old exact matches.", bucket.label, bias)
				value = fmt.Sprintf("%s age bucket", bucket.label)
			}
			return &apiSearchBoost{
				Kind:        "recency",
				Label:       label,
				Description: description,
				Value:       value,
				Boost:       bucket.boost,
			}
		}
	}
	return nil
}

func normalizedRecencyBiasForUser(user store.User) string {
	if value := strings.ToLower(strings.TrimSpace(user.SearchRecencyBias)); value != "" {
		switch value {
		case "none", "light", "normal", "strong":
			return value
		}
	}
	switch strings.ToLower(strings.TrimSpace(user.SearchPreset)) {
	case "strict":
		return "light"
	default:
		return "normal"
	}
}

func formatSearchBoostNumber(value float64) string {
	if value == 0 {
		return "0"
	}
	return strconv.FormatFloat(value, 'f', -1, 64)
}

func recencyExplanationBuckets(bias string) []struct {
	age   time.Duration
	label string
	boost float64
} {
	buckets := search.RecencyRankBuckets(bias)
	out := make([]struct {
		age   time.Duration
		label string
		boost float64
	}, 0, len(buckets))
	for _, bucket := range buckets {
		out = append(out, struct {
			age   time.Duration
			label string
			boost float64
		}{age: bucket.Age, label: bucket.Label, boost: bucket.Boost})
	}
	return out
}

func apiScoreExplanationFromRaw(raw *search.ScoreExplanation, depth int) *apiScoreExplanation {
	if raw == nil || depth > 5 {
		return nil
	}
	message := strings.TrimSpace(raw.Message)
	if len([]rune(message)) > 220 {
		message = string([]rune(message)[:220]) + "..."
	}
	out := &apiScoreExplanation{Value: raw.Value, Message: message}
	const maxChildren = 8
	limit := len(raw.Children)
	if limit > maxChildren {
		limit = maxChildren
	}
	for i := 0; i < limit; i++ {
		if child := apiScoreExplanationFromRaw(raw.Children[i], depth+1); child != nil {
			out.Children = append(out.Children, child)
		}
	}
	if len(raw.Children) > maxChildren {
		out.Children = append(out.Children, &apiScoreExplanation{Message: fmt.Sprintf("%d more scorer nodes omitted", len(raw.Children)-maxChildren)})
	}
	return out
}

// apiMessageLoadStatus is a cheap preflight used by ThreadView to decide whether
// to show the "fetching from IMAP/local blob" dialog before the heavier message
// request finishes.
func (s *Server) apiMessageLoadStatus(w http.ResponseWriter, r *http.Request, id int64) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	cu, ok := s.requireAPIAuth(w, r)
	if !ok {
		return
	}
	msg, err := s.store.GetMessageForUser(r.Context(), cu.User.ID, id)
	if store.IsNotFound(err) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, err)
		return
	}
	threadMessages, err := s.store.ListThreadMessagesForUser(r.Context(), cu.User.ID, msg)
	if err != nil {
		s.serverError(w, err)
		return
	}
	imapFetchCount := 0
	localBlobCount := 0
	indexedCount := 0
	unavailableCount := 0
	for _, threadMsg := range threadMessages {
		switch s.messageDisplayLoadSource(cu.User.ID, threadMsg) {
		case "imap":
			imapFetchCount++
		case "local_blob":
			localBlobCount++
		case "indexed":
			indexedCount++
		default:
			unavailableCount++
		}
	}
	source := "indexed"
	switch {
	case imapFetchCount > 0:
		source = "imap"
	case localBlobCount == len(threadMessages) && localBlobCount > 0:
		source = "local_blob"
	case localBlobCount > 0:
		source = "local"
	case unavailableCount > 0:
		source = "preview"
	}
	writeJSONCached(w, r, map[string]any{
		"conversation":      len(threadMessages),
		"imap_fetch_count":  imapFetchCount,
		"local_blob_count":  localBlobCount,
		"indexed_count":     indexedCount,
		"unavailable_count": unavailableCount,
		"source":            source,
	})
}

// messageDisplayLoadSource classifies the cheapest available source for one
// message body: already indexed HTML, existing blob, IMAP fetch, or preview only.
func (s *Server) messageDisplayLoadSource(userID int64, msg store.MessageRecord) string {
	if strings.TrimSpace(msg.BodyHTML) != "" {
		return "indexed"
	}
	if strings.TrimSpace(msg.BlobPath) != "" && s.blobs != nil {
		f, err := s.blobs.OpenUserBlob(userID, msg.BlobPath)
		if err == nil {
			_ = f.Close()
			return "local_blob"
		}
	}
	if s.syncer != nil {
		return "imap"
	}
	return "preview"
}

// apiMoveMessage moves one message through the syncer and then schedules mailbox
// refreshes so local state catches up with the remote IMAP server.
func (s *Server) apiMoveMessage(w http.ResponseWriter, r *http.Request, id int64) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	cu, ok := s.requireAPIAuth(w, r)
	if !ok {
		return
	}
	if !s.verifyCSRF(w, r) {
		return
	}
	var in struct {
		MailboxID int64 `json:"mailbox_id"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if s.syncer == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "IMAP sync is not configured")
		return
	}
	dest, err := s.store.GetMailboxForUser(r.Context(), cu.User.ID, in.MailboxID)
	if store.IsNotFound(err) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, err)
		return
	}
	refreshMailboxes, err := s.moveRefreshMailboxNames(r.Context(), cu.User.ID, []int64{id}, dest)
	if err != nil {
		s.serverError(w, err)
		return
	}
	finishForeground := func() {}
	if s.syncRunner != nil {
		finishForeground, err = s.syncRunner.BeginForegroundOperation(r.Context(), cu.User.ID)
		if err != nil {
			writeAPIError(w, http.StatusServiceUnavailable, "could not schedule message move")
			return
		}
	}
	defer finishForeground()
	if err := s.syncer.MoveMessage(r.Context(), cu.User.ID, id, in.MailboxID); err != nil {
		if store.IsNotFound(err) {
			http.NotFound(w, r)
			return
		}
		writeAPIError(w, http.StatusBadGateway, "could not move message")
		return
	}
	s.startMoveRefresh(cu.User.ID, dest.AccountID, refreshMailboxes)
	finishForeground()
	writeJSON(w, map[string]any{"ok": true, "mailbox": dest.Name})
}

func (s *Server) moveRefreshMailboxNames(ctx context.Context, userID int64, messageIDs []int64, dest store.Mailbox) ([]string, error) {
	seen := map[string]bool{}
	names := make([]string, 0, 2)
	add := func(name string) {
		name = strings.TrimSpace(name)
		key := strings.ToLower(name)
		if name == "" || seen[key] {
			return
		}
		seen[key] = true
		names = append(names, name)
	}
	messages, err := s.store.ListMessagesByIDsForUser(ctx, userID, messageIDs)
	if err != nil {
		return nil, err
	}
	mailboxes := map[int64]store.Mailbox{}
	for _, msg := range messages {
		if msg.MailboxID == 0 {
			continue
		}
		mb, ok := mailboxes[msg.MailboxID]
		if !ok {
			var err error
			mb, err = s.store.GetMailboxForUser(ctx, userID, msg.MailboxID)
			if store.IsNotFound(err) {
				continue
			}
			if err != nil {
				return nil, err
			}
			mailboxes[msg.MailboxID] = mb
		}
		add(mb.Name)
	}
	add(dest.Name)
	return names, nil
}

func (s *Server) startMoveRefresh(userID, accountID int64, mailboxes []string) {
	if s.syncRunner == nil {
		return
	}
	// Moves call this while holding the tenant's foreground reservation. The
	// queue path records a deferred refresh without waiting on that same token;
	// release schedules it before another foreground writer can take a turn.
	s.syncRunner.QueueAccountMailboxes(userID, accountID, mailboxes)
}

// apiBulkMoveMessages does small batches inline and large batches as background
// syncer jobs so drag/drop remains responsive for bulk selections.
func (s *Server) apiBulkMoveMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	cu, ok := s.requireAPIAuth(w, r)
	if !ok {
		return
	}
	if !s.verifyCSRF(w, r) {
		return
	}
	if s.syncer == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "IMAP sync is not configured")
		return
	}
	var in struct {
		MessageIDs []int64 `json:"message_ids"`
		MailboxID  int64   `json:"mailbox_id"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if len(in.MessageIDs) == 0 || len(in.MessageIDs) > 1000 || in.MailboxID <= 0 {
		writeAPIError(w, http.StatusBadRequest, "select messages and a destination folder")
		return
	}
	dest, err := s.store.GetMailboxForUser(r.Context(), cu.User.ID, in.MailboxID)
	if store.IsNotFound(err) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, err)
		return
	}
	refreshMailboxes, err := s.moveRefreshMailboxNames(r.Context(), cu.User.ID, in.MessageIDs, dest)
	if err != nil {
		s.serverError(w, err)
		return
	}
	if len(in.MessageIDs) > 5 {
		finishForeground := func() {}
		if s.syncRunner != nil {
			finishForeground, err = s.syncRunner.BeginForegroundOperation(r.Context(), cu.User.ID)
			if err != nil {
				writeAPIError(w, http.StatusServiceUnavailable, "could not schedule bulk move")
				return
			}
		}
		run, err := s.syncer.StartMoveMessages(r.Context(), cu.User.ID, in.MessageIDs, in.MailboxID, func() {
			s.startMoveRefresh(cu.User.ID, dest.AccountID, refreshMailboxes)
			finishForeground()
		})
		if err != nil {
			finishForeground()
			if store.IsNotFound(err) {
				http.NotFound(w, r)
				return
			}
			writeAPIError(w, http.StatusBadGateway, "could not start bulk move")
			return
		}
		writeJSON(w, map[string]any{"ok": true, "queued": true, "run_id": run.ID, "mailbox": dest.Name})
		return
	}
	finishForeground := func() {}
	if s.syncRunner != nil {
		finishForeground, err = s.syncRunner.BeginForegroundOperation(r.Context(), cu.User.ID)
		if err != nil {
			writeAPIError(w, http.StatusServiceUnavailable, "could not schedule bulk move")
			return
		}
	}
	defer finishForeground()
	moved, err := s.syncer.MoveMessages(r.Context(), cu.User.ID, in.MessageIDs, in.MailboxID)
	if err != nil {
		if store.IsNotFound(err) {
			http.NotFound(w, r)
			return
		}
		writeAPIError(w, http.StatusBadGateway, "could not move messages")
		return
	}
	s.startMoveRefresh(cu.User.ID, dest.AccountID, refreshMailboxes)
	finishForeground()
	writeJSON(w, map[string]any{"ok": true, "queued": false, "moved": moved, "mailbox": dest.Name})
}

// apiBulkCopyMessages copies messages by appending their raw RFC822 bodies to a
// destination IMAP mailbox. Unlike move, copy may target another account because
// the source message remains untouched and the destination receives a new UID.
func (s *Server) apiBulkCopyMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	cu, ok := s.requireAPIAuth(w, r)
	if !ok {
		return
	}
	if !s.verifyCSRF(w, r) {
		return
	}
	if s.syncer == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "IMAP sync is not configured")
		return
	}
	var in struct {
		MessageIDs []int64 `json:"message_ids"`
		MailboxID  int64   `json:"mailbox_id"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if len(in.MessageIDs) == 0 || len(in.MessageIDs) > 1000 || in.MailboxID <= 0 {
		writeAPIError(w, http.StatusBadRequest, "select messages and a destination folder")
		return
	}
	dest, err := s.store.GetMailboxForUser(r.Context(), cu.User.ID, in.MailboxID)
	if store.IsNotFound(err) {
		writeAPIError(w, http.StatusBadRequest, "destination folder is no longer available")
		return
	}
	if err != nil {
		s.serverError(w, err)
		return
	}
	refreshDest := func() {
		if s.syncRunner != nil {
			s.syncRunner.QueueAccountMailboxes(cu.User.ID, dest.AccountID, []string{dest.Name})
		}
	}
	if len(in.MessageIDs) > 5 {
		finishForeground := func() {}
		if s.syncRunner != nil {
			finishForeground, err = s.syncRunner.BeginForegroundOperation(r.Context(), cu.User.ID)
			if err != nil {
				writeAPIError(w, http.StatusServiceUnavailable, "could not schedule bulk copy")
				return
			}
		}
		run, err := s.syncer.StartCopyMessages(r.Context(), cu.User.ID, in.MessageIDs, in.MailboxID, func() {
			defer finishForeground()
			refreshDest()
		})
		if err != nil {
			finishForeground()
			if store.IsNotFound(err) {
				writeAPIError(w, http.StatusBadRequest, "copy source or destination is no longer available")
				return
			}
			writeAPIError(w, http.StatusBadGateway, "could not start bulk copy")
			return
		}
		writeJSON(w, map[string]any{"ok": true, "queued": true, "run_id": run.ID, "mailbox": dest.Name})
		return
	}
	finishForeground := func() {}
	if s.syncRunner != nil {
		finishForeground, err = s.syncRunner.BeginForegroundOperation(r.Context(), cu.User.ID)
		if err != nil {
			writeAPIError(w, http.StatusServiceUnavailable, "could not schedule bulk copy")
			return
		}
	}
	defer finishForeground()
	copied, err := s.syncer.CopyMessages(r.Context(), cu.User.ID, in.MessageIDs, in.MailboxID)
	if err != nil {
		if store.IsNotFound(err) {
			writeAPIError(w, http.StatusBadRequest, "copy source or destination is no longer available")
			return
		}
		writeAPIError(w, http.StatusBadGateway, "could not copy messages")
		return
	}
	refreshDest()
	finishForeground()
	writeJSON(w, map[string]any{"ok": true, "queued": false, "copied": copied, "mailbox": dest.Name})
}

func (s *Server) apiSetMessageStarred(w http.ResponseWriter, r *http.Request, id int64) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	cu, ok := s.requireAPIAuth(w, r)
	if !ok {
		return
	}
	if !s.verifyCSRF(w, r) {
		return
	}
	if s.syncer == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "IMAP sync is not configured")
		return
	}
	var in struct {
		Starred bool `json:"starred"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	msg, err := s.syncer.SetStarredForMessage(r.Context(), cu.User.ID, id, in.Starred)
	if err != nil {
		if store.IsNotFound(err) {
			http.NotFound(w, r)
			return
		}
		s.serverError(w, err)
		return
	}
	go func(userID, messageID int64) {
		ctx := s.backgroundContext()
		if err := s.syncer.SyncStarStateForMessage(ctx, userID, messageID); err != nil && ctx.Err() == nil {
			log.Printf("sync starred flag user_id=%d message_id=%d: %v", userID, messageID, err)
		}
		s.notifyUserChanged(userID)
	}(cu.User.ID, msg.ID)
	s.notifyUserChanged(cu.User.ID)
	writeJSON(w, map[string]any{"ok": true, "message": apiMessageFromRecord(msg, msg.BodyText)})
}

func (s *Server) apiOneClickUnsubscribe(w http.ResponseWriter, r *http.Request, id int64) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	cu, ok := s.requireAPIAuth(w, r)
	if !ok {
		return
	}
	if !s.verifyCSRF(w, r) {
		return
	}
	msg, err := s.store.GetMessageForUser(r.Context(), cu.User.ID, id)
	if store.IsNotFound(err) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, err)
		return
	}
	target, ok := s.oneClickUnsubscribeTarget(r.Context(), cu.User.ID, msg)
	if !ok {
		writeAPIError(w, http.StatusBadRequest, "This message does not advertise RFC 8058 one-click unsubscribe.")
		return
	}
	userDB, err := s.store.UserDB(r.Context(), cu.User.ID)
	if err != nil {
		s.serverError(w, err)
		return
	}
	hook, ok := oneClickUnsubscribeHook()
	if !ok {
		http.NotFound(w, r)
		return
	}
	if previous, prevErr := hook.LatestOneClickSend(r.Context(), userDB, cu.User.ID, msg.ID, target.String(), time.Now().Add(-oneClickUnsubscribeRecentWindow)); prevErr == nil {
		writeJSON(w, map[string]any{"ok": true, "already_sent": true, "sent_at": timeString(previous.SentAt)})
		return
	}
	if err := s.performOneClickUnsubscribe(r.Context(), target); err != nil {
		if errors.Is(err, errOneClickUnavailable) {
			writeAPIError(w, http.StatusBadRequest, "This message does not advertise RFC 8058 one-click unsubscribe.")
			return
		}
		writeAPIError(w, http.StatusBadGateway, "Unsubscribe request failed.")
		return
	}
	sentAt := time.Now()
	if err := hook.RecordOneClickSend(r.Context(), userDB, cu.User.ID, msg.ID, msg.FromAddr, target.String(), sentAt); err != nil {
		s.serverError(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "already_sent": false, "sent_at": timeString(sentAt)})
}

func (s *Server) apiTrustImages(w http.ResponseWriter, r *http.Request, id int64) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	cu, ok := s.requireAPIAuth(w, r)
	if !ok {
		return
	}
	if !s.verifyCSRF(w, r) {
		return
	}
	msg, err := s.store.GetMessageForUser(r.Context(), cu.User.ID, id)
	if store.IsNotFound(err) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, err)
		return
	}
	userDB, err := s.store.UserDB(r.Context(), cu.User.ID)
	if err != nil {
		s.serverError(w, err)
		return
	}
	hook, ok := trustedImageSourcesHook()
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := hook.TrustImageSender(r.Context(), userDB, cu.User.ID, msg.FromAddr); err != nil {
		s.serverError(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}
func (s *Server) threadViewsForMessage(ctx context.Context, cu currentUser, msg store.MessageRecord, showImages bool, query string) ([]threadMessageView, store.MessageRecord, error) {
	return s.threadViewsForMessageTimed(ctx, cu, msg, showImages, query, nil)
}

func (s *Server) threadViewsForMessageTimed(ctx context.Context, cu currentUser, msg store.MessageRecord, showImages bool, query string, timing *searchTiming) ([]threadMessageView, store.MessageRecord, error) {
	stop := func() {}
	if timing != nil {
		stop = timing.measure(&timing.thread)
	}
	threadMessages, err := s.store.ListThreadMessagesForUser(ctx, cu.User.ID, msg)
	stop()
	if err != nil {
		return nil, msg, err
	}
	threadViews := make([]threadMessageView, 0, len(threadMessages))
	previousBodies := make([]string, 0, len(threadMessages))
	if timing != nil {
		stop = timing.measure(&timing.hydrateSetup)
	}
	own := s.ownAddresses(ctx, cu.User)
	remoteImageBlockingEnabled := s.pluginEnabled(ctx, plugins.RemoteImageBlocklist)
	trustedImageSourcesEnabled := s.pluginEnabled(ctx, plugins.TrustedImageSources)
	oneClickUnsubscribeEnabled := s.pluginEnabled(ctx, plugins.OneClickUnsubscribe)
	var trustedDB *sql.DB
	if trustedImageSourcesEnabled {
		var trustedDBErr error
		trustedDB, trustedDBErr = s.store.UserDB(ctx, cu.User.ID)
		if trustedDBErr != nil {
			stop()
			return nil, msg, trustedDBErr
		}
	}
	var imageBlockRules []string
	if remoteImageBlockingEnabled {
		if hook, ok := remoteImageBlocklistHook(); ok {
			imageBlockRules, err = hook.ListRemoteImagePatterns(ctx, s.store.DB())
			if err != nil {
				stop()
				return nil, msg, err
			}
		}
	}
	stop()
	if timing != nil {
		stop = timing.measure(&timing.match)
	}
	matchDetails := s.threadSearchMatchDetails(ctx, cu.User, threadMessages, query)
	stop()
	unreadIDs := make([]int64, 0, len(threadMessages))
	for _, threadMsg := range threadMessages {
		if !threadMsg.IsRead {
			unreadIDs = append(unreadIDs, threadMsg.ID)
		}
	}
	markedRead := false
	defer func() {
		if markedRead {
			s.notifyUserChanged(cu.User.ID)
		}
	}()
	if len(unreadIDs) > 0 {
		if timing != nil {
			stop = timing.measure(&timing.readState)
		}
		if err := s.store.MarkMessagesReadForUser(ctx, cu.User.ID, unreadIDs, true, true); err == nil {
			markedRead = true
			if s.syncer != nil {
				s.queueReadStatePush(cu.User.ID, unreadIDs)
			}
		}
		stop()
	}
	messageIDs := make([]int64, 0, len(threadMessages))
	for _, threadMsg := range threadMessages {
		messageIDs = append(messageIDs, threadMsg.ID)
	}
	attachmentsByMessage := map[int64][]store.Attachment{}
	if len(messageIDs) > 0 {
		if timing != nil {
			stop = timing.measure(&timing.attachments)
		}
		var err error
		attachmentsByMessage, err = s.store.ListAttachmentsForMessages(ctx, cu.User.ID, messageIDs)
		stop()
		if err != nil {
			return nil, msg, err
		}
	}
	headerCache := map[int64]mail.Header{}
	headerFor := func(threadMsg store.MessageRecord) mail.Header {
		if header, ok := headerCache[threadMsg.ID]; ok {
			return header
		}
		header := s.rawMessageHeader(ctx, cu.User.ID, threadMsg)
		headerCache[threadMsg.ID] = header
		return header
	}
	for idx, threadMsg := range threadMessages {
		if !threadMsg.IsRead {
			threadMsg.IsRead = true
			threadMsg.ReadSyncPending = true
		}
		if timing != nil {
			stop = timing.measure(&timing.security)
		}
		threadMsg, err = s.ensureMessageSecurityState(ctx, cu.User.ID, threadMsg)
		stop()
		if err != nil {
			return nil, msg, err
		}
		if timing != nil {
			stop = timing.measure(&timing.attachments)
		}
		allAttachments := attachmentsByMessage[threadMsg.ID]
		expanded := idx == len(threadMessages)-1 || threadMsg.ID == msg.ID || len(threadMessages) == 1
		allAttachments, err = s.ensureMessageAttachments(ctx, cu.User.ID, threadMsg, allAttachments, expanded)
		stop()
		if err != nil {
			return nil, msg, err
		}
		attachments := visibleAttachments(allAttachments)
		if timing != nil {
			stop = timing.measure(&timing.body)
		}
		sourceHTML, sourceText, previewOnly := s.displayBodiesForMessage(ctx, cu.User.ID, threadMsg)
		stop()
		displayMsg := threadMsg
		displayMsg.BodyHTML = sourceHTML
		displayMsg.BodyText = sourceText
		displayHTML, displayText, hiddenQuoted := clippedEmailBody(sourceHTML, sourceText, previousBodies)
		remoteImages := remoteImageBlockingEnabled && hasRemoteImages(sourceHTML)
		imagesAllowed := showImages || !remoteImageBlockingEnabled
		if remoteImageBlockingEnabled && trustedImageSourcesEnabled && remoteImages && !imagesAllowed {
			hook, ok := trustedImageSourcesHook()
			if ok {
				if trusted, trustErr := hook.IsImageSenderTrusted(ctx, trustedDB, cu.User.ID, threadMsg.FromAddr); trustErr == nil && trusted {
					imagesAllowed = true
				}
			}
		}
		if timing != nil {
			stop = timing.measure(&timing.unsubscribe)
		}
		var oneClickUnsub bool
		oneClickSentAt := time.Time{}
		if oneClickUnsubscribeEnabled {
			oneClickTarget, ok := oneClickUnsubscribeTargetFromHeader(headerFor(threadMsg))
			oneClickUnsub = ok
			if oneClickUnsub {
				oneClickSentAt = s.recentOneClickUnsubscribeSentAt(ctx, cu.User.ID, threadMsg, oneClickTarget.String())
			}
		}
		stop()
		attachmentMatches, attachmentContentMatched, attachmentMatchTerms := attachmentSearchMatches(attachments, matchDetails[threadMsg.ID], query)
		if timing != nil {
			stop = timing.measure(&timing.headers)
		}
		header := headerFor(threadMsg)
		headerDetails := messageHeaderDetailsFromHeader(header, threadMsg)
		securityIndicators := messageSecurityIndicatorsFor(header, displayMsg, sourceHTML)
		stop()
		threadViews = append(threadViews, threadMessageView{
			Message:                  displayMsg,
			Attachments:              attachments,
			InlineAttachments:        allAttachments,
			HeaderDetails:            headerDetails,
			SecurityIndicators:       securityIndicators,
			OneClickUnsub:            oneClickUnsub,
			OneClickSentAt:           oneClickSentAt,
			AttachmentMatches:        attachmentMatches,
			AttachmentContentMatched: attachmentContentMatched,
			AttachmentMatchTerms:     attachmentMatchTerms,
			SenderName:               senderDisplayName(displayMsg.FromAddr),
			SenderEmail:              senderEmail(displayMsg.FromAddr),
			SenderInitial:            senderInitial(displayMsg.FromAddr),
			RecipientLine:            recipientLine(displayMsg),
			Snippet:                  threadSnippet(displayText, sourceText),
			DisplayBodyHTML:          displayHTML,
			DisplayBodyText:          displayText,
			HasHiddenQuoted:          hiddenQuoted,
			HasDisplayBody:           strings.TrimSpace(displayHTML) != "" || strings.TrimSpace(displayText) != "",
			BodyPreviewOnly:          previewOnly,
			HasRemoteImages:          remoteImages,
			ImagesAllowed:            imagesAllowed,
			ImageBlockRules:          imageBlockRules,
			Expanded:                 expanded,
			CanReplyAll:              canReplyAll(threadMsg, threadMessages, own),
		})
		previousBodies = append(previousBodies, sourceText)
		if threadMsg.ID == msg.ID {
			msg = displayMsg
		}
	}
	return threadViews, msg, nil
}

func (s *Server) ensureMessageSecurityState(ctx context.Context, userID int64, msg store.MessageRecord) (store.MessageRecord, error) {
	hasSecurityProvider, err := s.hasMessageSecurityProvider(ctx)
	if err != nil {
		return msg, err
	}
	if !hasSecurityProvider {
		return msg, nil
	}
	var raw []byte
	if strings.TrimSpace(msg.BlobPath) != "" || strings.TrimSpace(msg.BodyText+msg.BodyHTML) == "" {
		if data, err := s.rawMessageBytes(ctx, userID, msg); err == nil {
			raw = data
		}
	}
	state, handled, err := s.detectMessageSecurity(ctx, userID, raw, plugins.MessageBody{Purpose: "repair", Text: msg.BodyText, HTML: msg.BodyHTML})
	if err != nil {
		return msg, err
	}
	if !handled || (!state.Encrypted && !state.Signed) {
		return msg, nil
	}
	if state.Encrypted == msg.IsEncrypted && state.Signed == msg.IsSigned {
		return msg, nil
	}
	if err := s.store.UpdateMessageSecurityState(ctx, userID, msg.ID, state.Encrypted, state.Signed); err != nil {
		return msg, err
	}
	msg.IsEncrypted = state.Encrypted
	msg.IsSigned = state.Signed
	return msg, nil
}

const maxSyncAttachmentRepairBytes int64 = 256 * 1024

func (s *Server) ensureMessageAttachments(ctx context.Context, userID int64, msg store.MessageRecord, current []store.Attachment, allowSyncRepair bool) ([]store.Attachment, error) {
	if len(current) > 0 || !msg.HasAttachments {
		return current, nil
	}
	if !allowSyncRepair || msg.Size <= 0 || msg.Size > maxSyncAttachmentRepairBytes {
		s.repairMessageAttachmentsAsync(userID, msg)
		return current, nil
	}
	repaired, err := s.repairMessageAttachments(ctx, userID, msg)
	if err != nil {
		return nil, err
	}
	if len(repaired) == 0 {
		return current, nil
	}
	return repaired, nil
}

func (s *Server) repairMessageAttachments(ctx context.Context, userID int64, msg store.MessageRecord) ([]store.Attachment, error) {
	raw, err := s.rawMessageBytes(ctx, userID, msg)
	if err != nil {
		return nil, nil
	}
	parsed, err := mailparse.Parse(raw)
	if err != nil || len(parsed.Files) == 0 {
		return nil, nil
	}
	if err := s.store.DeleteAttachmentsForMessage(ctx, userID, msg.ID); err != nil {
		return nil, err
	}
	for _, file := range parsed.Files {
		if _, err := s.store.CreateAttachment(ctx, store.Attachment{
			UserID:      userID,
			MessageID:   msg.ID,
			BlobID:      msg.BlobID,
			Filename:    file.Filename,
			ContentType: file.ContentType,
			ContentID:   file.ContentID,
			IsInline:    file.IsInline,
			Size:        int64(len(file.Data)),
			BlobPath:    "",
		}); err != nil {
			return nil, err
		}
	}
	return s.store.ListAttachmentsForMessage(ctx, userID, msg.ID)
}

func (s *Server) repairMessageAttachmentsAsync(userID int64, msg store.MessageRecord) {
	if s == nil || s.store == nil || userID <= 0 || msg.ID <= 0 || !msg.HasAttachments {
		return
	}
	s.attachmentRepairMu.Lock()
	if s.attachmentRepairRunning == nil {
		s.attachmentRepairRunning = map[int64]map[int64]bool{}
	}
	runningByUser := s.attachmentRepairRunning[userID]
	if runningByUser == nil {
		runningByUser = map[int64]bool{}
		s.attachmentRepairRunning[userID] = runningByUser
	}
	if runningByUser[msg.ID] {
		s.attachmentRepairMu.Unlock()
		return
	}
	runningByUser[msg.ID] = true
	s.attachmentRepairMu.Unlock()

	go func() {
		defer func() {
			s.attachmentRepairMu.Lock()
			if runningByUser := s.attachmentRepairRunning[userID]; runningByUser != nil {
				delete(runningByUser, msg.ID)
				if len(runningByUser) == 0 {
					delete(s.attachmentRepairRunning, userID)
				}
			}
			s.attachmentRepairMu.Unlock()
		}()
		repairCtx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		attachments, err := s.repairMessageAttachments(repairCtx, userID, msg)
		if err != nil {
			log.Printf("repair attachments user_id=%d message_id=%d: %v", userID, msg.ID, err)
			return
		}
		if len(attachments) > 0 {
			s.notifyUserChanged(userID)
		}
	}()
}

type threadSearchMatch struct {
	Terms  []string
	Fields []string
}

func (s *Server) threadSearchMatchDetails(ctx context.Context, user store.User, messages []store.MessageRecord, query string) map[int64]threadSearchMatch {
	userID := user.ID
	out := map[int64]threadSearchMatch{}
	query, _ = stripStarSearchOperators(strings.TrimSpace(query))
	cleanQuery, _, err := s.searchMailboxFilter(ctx, userID, query)
	if err != nil {
		return out
	}
	query = cleanQuery
	if query == "" || s.search == nil {
		return out
	}
	opts := searchOptionsForUser(user)
	for _, msg := range messages {
		hit, ok, err := s.search.MatchMessageWithOptions(ctx, userID, msg.ID, query, opts)
		if err != nil || !ok {
			continue
		}
		out[msg.ID] = threadSearchMatch{Terms: hit.Terms, Fields: hit.Fields}
	}
	return out
}

func attachmentSearchMatches(attachments []store.Attachment, match threadSearchMatch, query string) ([]string, bool, []string) {
	if !searchFieldsInclude(match.Fields, "attachment_names", "attachments", "attachment_types") {
		return nil, false, nil
	}
	needles := mergeSnippetTerms(match.Terms, searchSnippetTerms(query))
	var matches []string
	if searchFieldsInclude(match.Fields, "attachment_names") {
		for _, att := range attachments {
			name := attachmentDisplayName(att)
			if name != "" && attachmentNameMatches(name, needles) {
				matches = append(matches, name)
			}
		}
	}
	contentMatched := searchFieldsInclude(match.Fields, "attachments") && len(matches) == 0
	if len(matches) == 0 && !contentMatched {
		needles = nil
	}
	return uniqueStrings(matches, len(matches)), contentMatched, needles
}

func visibleAttachments(attachments []store.Attachment) []store.Attachment {
	out := make([]store.Attachment, 0, len(attachments))
	for _, att := range attachments {
		if isDisplayAttachment(att) {
			out = append(out, att)
		}
	}
	return out
}

func isDisplayAttachment(att store.Attachment) bool {
	if att.IsInline {
		return false
	}
	filename := strings.ToLower(strings.TrimSpace(att.Filename))
	contentID := strings.TrimSpace(att.ContentID)
	contentType := strings.ToLower(strings.TrimSpace(att.ContentType))
	if contentID != "" && strings.HasPrefix(contentType, "image/") {
		return false
	}
	if strings.HasPrefix(contentType, "image/") && strings.HasPrefix(filename, "outlook-") && att.Size <= 256*1024 {
		return false
	}
	return true
}

func attachmentDisplayName(att store.Attachment) string {
	name := strings.TrimSpace(att.Filename)
	if name == "" {
		name = strings.TrimSpace(att.ContentType)
	}
	if name == "" {
		name = "Attachment"
	}
	return name
}

func stringInSliceFold(value string, values []string) bool {
	value = strings.TrimSpace(value)
	for _, candidate := range values {
		if strings.EqualFold(value, strings.TrimSpace(candidate)) {
			return true
		}
	}
	return false
}
