// File overview: Bleve search service. It owns per-user indexes, document mapping, query construction, result ranking, and highlighting metadata.

package search

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/mapping"
	blevesearch "github.com/blevesearch/bleve/v2/search"
	blevequery "github.com/blevesearch/bleve/v2/search/query"

	"rolltop/backend/store"
)

// Service owns Bleve indexes and query construction for either combined-test or per-user production mode.
type Service struct {
	index              bleve.Index
	root               string
	perUser            bool
	mu                 sync.Mutex
	indexes            map[int64]bleve.Index
	writers            map[int64]*writerLock
	writeCoordinator   *bleveWriteCoordinator
	closing            bool
	writes             sync.WaitGroup
	closeDone          chan struct{}
	closeErr           error
	closeWriterTimeout time.Duration
	writerWaitLogAfter time.Duration
	activeStallAfter   time.Duration
	activeStallHandler func(int64)
	activeStallOnce    sync.Once
	bleveErrorLog      func(string, ...any)
}

// writerLock is a context-friendly FIFO gate. Unlike polling sync.Mutex.TryLock,
// a blocked operation joins an explicit waiter queue, so later Bleve commits
// cannot repeatedly barge ahead of mailbox maintenance.
type writerLock struct {
	mu      sync.Mutex
	held    bool
	waiters []*writerWaiter

	active      bleveErrorContext
	activeSince time.Time
}

type writerWaiter struct {
	ready   chan struct{}
	granted bool
}

func newWriterLock() *writerLock {
	return &writerLock{}
}

func (l *writerLock) Lock() {
	waiter, acquired := l.acquireOrQueue()
	if !acquired {
		<-waiter.ready
	}
}

func (l *writerLock) TryLock() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.held || len(l.waiters) > 0 {
		return false
	}
	l.held = true
	return true
}

func (l *writerLock) Unlock() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.held {
		panic("search: unlock of unlocked writer")
	}
	if len(l.waiters) == 0 {
		l.held = false
		return
	}
	waiter := l.waiters[0]
	l.waiters = l.waiters[1:]
	waiter.granted = true
	close(waiter.ready)
}

func (l *writerLock) acquireOrQueue() (*writerWaiter, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.held && len(l.waiters) == 0 {
		l.held = true
		return nil, true
	}
	waiter := &writerWaiter{ready: make(chan struct{})}
	l.waiters = append(l.waiters, waiter)
	return waiter, false
}

func (l *writerLock) cancelWaiter(waiter *writerWaiter) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if waiter == nil || waiter.granted {
		return false
	}
	for index, queued := range l.waiters {
		if queued != waiter {
			continue
		}
		copy(l.waiters[index:], l.waiters[index+1:])
		l.waiters = l.waiters[:len(l.waiters)-1]
		return true
	}
	return false
}

func (l *writerLock) queuedWaiters() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.waiters)
}

func (l *writerLock) setActive(details bleveErrorContext) {
	l.mu.Lock()
	l.active = details
	l.activeSince = time.Now()
	l.mu.Unlock()
}

func (l *writerLock) clearActive() {
	l.mu.Lock()
	l.active = bleveErrorContext{}
	l.activeSince = time.Time{}
	l.mu.Unlock()
}

func (l *writerLock) activeSnapshot() (bleveErrorContext, time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.active, l.activeSince
}

type bleveErrorContext struct {
	Operation       string
	UserID          int64
	AccountID       int64
	MailboxID       int64
	Documents       int
	BatchBytes      uint64
	FirstDocumentID int64
	LastDocumentID  int64
	DocumentIDs     []int64
}

var errSearchServiceClosing = errors.New("search service is closing")

const (
	searchCloseWriterTimeout = 2 * time.Second
	activeWriterStallTimeout = 2 * time.Minute
	maxStallDocumentIDs      = 8
	maxBleveStackBytes       = 8 * 1024
)

// reportBleveError writes operational index failures through Go's standard
// logger, which Docker captures from stderr. Callers provide IDs and counts only:
// query text and indexed message content must never enter these diagnostics.
func (s *Service) reportBleveError(details bleveErrorContext, err error) {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errSearchServiceClosing) {
		return
	}
	logger := log.Printf
	if s != nil && s.bleveErrorLog != nil {
		logger = s.bleveErrorLog
	}
	logger("bleve error operation=%q user_id=%d account_id=%d mailbox_id=%d documents=%d batch_bytes=%d first_document_id=%d last_document_id=%d document_ids=%v error_type=%T error=%v",
		details.Operation, details.UserID, details.AccountID, details.MailboxID, details.Documents, details.BatchBytes,
		details.FirstDocumentID, details.LastDocumentID, details.DocumentIDs, err, err)
}

// AttachmentDoc is transient attachment text passed to IndexMessage after raw message parsing.
type AttachmentDoc struct {
	Filename    string
	ContentType string
	Text        string
}

// MessageIndexDocument is the complete search payload for one stored message.
// Sync batches these values after SQLite/blob writes so Bleve pays one commit
// cost for many IMAP messages instead of one commit per message.
type MessageIndexDocument struct {
	Message     store.MessageRecord
	Attachments []AttachmentDoc
}

const maxCompoundFieldBytes = 128 * 1024
const minSplitFragmentLength = 4

// SearchOptions carries ranking hints supplied by callers outside the raw query text.
type SearchOptions struct {
	SenderBoosts  []SenderBoost
	ContactBoosts []SenderBoost
	Behavior      SearchBehavior
}

// SearchBehavior holds query-time ranking controls from the authenticated user's
// profile. These knobs change only Bleve query construction, so saving them does
// not require reindexing existing mail. SenderBoostSet and CompactSplittingSet
// let zero-value SearchOptions{} preserve the established defaults.
type SearchBehavior struct {
	Preset              string
	RecencyBias         string
	Fuzzy               string
	SenderBoost         bool
	SenderBoostSet      bool
	SenderBoostScale    float64
	ContactBoostScale   float64
	AttachmentWeight    string
	CompactSplitting    bool
	CompactSplittingSet bool
}

type normalizedSearchBehavior struct {
	Preset            string
	RecencyBias       string
	Fuzzy             string
	SenderBoost       bool
	SenderBoostScale  float64
	ContactBoostScale float64
	AttachmentWeight  string
	CompactSplitting  bool
}

// RecencyRankBucket describes the query-time freshness nudge added to the
// Bleve text score. The values are deliberately in the same order of magnitude
// as normal text-match scores; old exact matches can still win, but a current
// direct hit should not be buried under years-old bulk mail.
type RecencyRankBucket struct {
	Age   time.Duration
	Label string
	Boost float64
}

// Hit is a search result with the terms/fields Bleve reported for highlighting.
type Hit struct {
	ID         int64
	Score      float64
	Terms      []string
	Fields     []string
	QueryTerms []string
}

// FieldTermMatch groups the concrete indexed terms Bleve reported by indexed field.
type FieldTermMatch struct {
	Field string
	Terms []string
}

// TermContribution is a flattened, human-readable scorer summary extracted from
// the Bleve explanation tree. QueryTerm keeps the full field-qualified Bleve term
// such as "body:housing" so result rows and explanation panels can show
// the exact indexed field/term pair that contributed to ranking.
type TermContribution struct {
	Field         string
	Term          string
	QueryTerm     string
	Score         float64
	TermFrequency float64
	FieldNorm     float64
	IDF           float64
	QueryWeight   float64
	Boost         float64
	QueryNorm     float64
}

// ScoreExplanation is Bleve's scorer tree for one hit. It is intentionally
// exposed only through on-demand explain endpoints because it can be verbose.
type ScoreExplanation = blevesearch.Explanation

// ExplanationResult is a single-document search explanation. Locations drive
// human-readable match labels while Raw preserves Bleve's scorer tree for debug UI.
type ExplanationResult struct {
	ID                int64
	Score             float64
	Terms             []string
	Fields            []string
	QueryTerms        []string
	FieldMatches      []FieldTermMatch
	TermContributions []TermContribution
	Raw               *ScoreExplanation
}

// SenderBoost increases rank for senders the user historically reads.
type SenderBoost struct {
	Sender string
	Boost  float64
}

func (b SearchBehavior) normalized() normalizedSearchBehavior {
	preset := normalizeChoice(b.Preset, "balanced", "strict", "balanced", "forgiving")
	out := normalizedSearchBehavior{Preset: preset, SenderBoost: true, SenderBoostScale: 1, CompactSplitting: true}
	switch preset {
	case "strict":
		out.RecencyBias = "light"
		out.Fuzzy = "off"
		out.AttachmentWeight = "normal"
		out.CompactSplitting = false
	case "forgiving":
		out.RecencyBias = "normal"
		out.Fuzzy = "forgiving"
		out.AttachmentWeight = "strong"
	default:
		out.RecencyBias = "normal"
		out.Fuzzy = "balanced"
		out.AttachmentWeight = "normal"
	}
	if strings.TrimSpace(b.RecencyBias) != "" {
		out.RecencyBias = normalizeChoice(b.RecencyBias, out.RecencyBias, "none", "light", "normal", "strong")
	}
	if strings.TrimSpace(b.Fuzzy) != "" {
		out.Fuzzy = normalizeChoice(b.Fuzzy, out.Fuzzy, "off", "balanced", "forgiving")
	}
	if strings.TrimSpace(b.AttachmentWeight) != "" {
		out.AttachmentWeight = normalizeChoice(b.AttachmentWeight, out.AttachmentWeight, "off", "light", "normal", "strong")
	}
	if b.SenderBoostSet {
		out.SenderBoost = b.SenderBoost
	}
	if b.SenderBoostScale > 0 {
		out.SenderBoostScale = b.SenderBoostScale
	}
	if !out.SenderBoost {
		out.SenderBoostScale = 0
	}
	if b.ContactBoostScale > 0 {
		out.ContactBoostScale = b.ContactBoostScale
	}
	if b.CompactSplittingSet {
		out.CompactSplitting = b.CompactSplitting
	}
	return out
}

func normalizeChoice(value, fallback string, allowed ...string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	for _, item := range allowed {
		if value == item {
			return value
		}
	}
	return fallback
}

func (b normalizedSearchBehavior) fuzzyEnabled() bool {
	return b.Fuzzy != "off"
}

func (b normalizedSearchBehavior) attachmentBoostScale() float64 {
	switch b.AttachmentWeight {
	case "off":
		return 0
	case "light":
		return 0.4
	case "strong":
		return 1.7
	default:
		return 1
	}
}

func RecencyRankBuckets(bias string) []RecencyRankBucket {
	switch normalizeChoice(bias, "normal", "none", "light", "normal", "strong") {
	case "none":
		return nil
	case "light":
		return recencyBucketsWithAgePenalties([]RecencyRankBucket{
			{Age: 36 * time.Hour, Label: "36 hours", Boost: 0.2},
			{Age: 7 * 24 * time.Hour, Label: "7 days", Boost: 0.12},
			{Age: 30 * 24 * time.Hour, Label: "30 days", Boost: 0.07},
			{Age: 180 * 24 * time.Hour, Label: "180 days", Boost: 0.03},
			{Age: 730 * 24 * time.Hour, Label: "2 years", Boost: 0.01},
		}, []float64{-0.005, -0.01, -0.015, -0.02})
	case "strong":
		return recencyBucketsWithAgePenalties([]RecencyRankBucket{
			{Age: 36 * time.Hour, Label: "36 hours", Boost: 2.6},
			{Age: 7 * 24 * time.Hour, Label: "7 days", Boost: 1.8},
			{Age: 30 * 24 * time.Hour, Label: "30 days", Boost: 1.2},
			{Age: 90 * 24 * time.Hour, Label: "90 days", Boost: 0.7},
			{Age: 180 * 24 * time.Hour, Label: "180 days", Boost: 0.4},
			{Age: 365 * 24 * time.Hour, Label: "1 year", Boost: 0.18},
			{Age: 730 * 24 * time.Hour, Label: "2 years", Boost: 0.06},
		}, []float64{-0.04, -0.08, -0.13, -0.2})
	default:
		return recencyBucketsWithAgePenalties([]RecencyRankBucket{
			{Age: 36 * time.Hour, Label: "36 hours", Boost: 1.6},
			{Age: 7 * 24 * time.Hour, Label: "7 days", Boost: 1.15},
			{Age: 30 * 24 * time.Hour, Label: "30 days", Boost: 0.75},
			{Age: 90 * 24 * time.Hour, Label: "90 days", Boost: 0.4},
			{Age: 180 * 24 * time.Hour, Label: "180 days", Boost: 0.22},
			{Age: 365 * 24 * time.Hour, Label: "1 year", Boost: 0.1},
			{Age: 730 * 24 * time.Hour, Label: "2 years", Boost: 0.02},
		}, []float64{-0.02, -0.04, -0.07, -0.1})
	}
}

func recencyBucketsWithAgePenalties(fresh []RecencyRankBucket, penalties []float64) []RecencyRankBucket {
	out := append([]RecencyRankBucket{}, fresh...)
	ages := []struct {
		age   time.Duration
		label string
	}{
		{ageYears(5), "2-5 years old"},
		{ageYears(10), "5-10 years old"},
		{ageYears(20), "10-20 years old"},
		{ageYears(200), "20+ years old"},
	}
	for index, bucket := range ages {
		if index >= len(penalties) {
			break
		}
		out = append(out, RecencyRankBucket{Age: bucket.age, Label: bucket.label, Boost: penalties[index]})
	}
	return out
}

func ageYears(years int) time.Duration {
	return time.Duration(years*365) * 24 * time.Hour
}

// Open creates or opens a single combined Bleve index. Tests use this mode; the
// production server uses OpenPerUser so message IDs can overlap safely by user.
func Open(path string) (*Service, error) {
	service := newSearchService()
	index, err := openIndex(path)
	if err != nil {
		service.reportBleveError(bleveErrorContext{Operation: "open-index"}, err)
		return nil, err
	}
	service.index = index
	return service, nil
}

// OpenPerUser creates a lazy per-user index service. Each tenant gets
// data/users/<id>/bleve, keeping search documents and index locks scoped to the
// same boundary as user SQLite and blob data.
func OpenPerUser(root string) (*Service, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	service := newSearchService()
	service.root = root
	service.perUser = true
	service.indexes = make(map[int64]bleve.Index)
	return service, nil
}

func newSearchService() *Service {
	service := &Service{writers: make(map[int64]*writerLock)}
	service.writeCoordinator = newBleveWriteCoordinator(bleveWriteCoordinatorConfig{
		OnWait: service.reportBleveCoordinatorWait,
	})
	return service
}

// openIndex either opens an existing Bleve index or creates the Rolltop mapping.
// Most fields are not stored in Bleve because SQLite remains the source of truth;
// the index stores only enough term data to find and rank message IDs.
func openIndex(path string) (bleve.Index, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	index, err := bleve.Open(path)
	if err == nil {
		return index, nil
	}
	if !errors.Is(err, bleve.ErrorIndexPathDoesNotExist) {
		return nil, fmt.Errorf("open Bleve index: %w", err)
	}
	mapping := bleve.NewIndexMapping()
	mapping.StoreDynamic = false
	doc := bleve.NewDocumentMapping()
	userField := keywordField()
	doc.AddFieldMappingsAt("user_id", userField)
	doc.AddFieldMappingsAt("mailbox_id", userField)
	doc.AddFieldMappingsAt("has_attachment", userField)
	doc.AddFieldMappingsAt("is_read", userField)
	doc.AddFieldMappingsAt("is_starred", userField)
	doc.AddFieldMappingsAt("is_encrypted", userField)
	doc.AddFieldMappingsAt("is_signed", userField)
	doc.AddFieldMappingsAt("plugin_language_code", userField)
	for _, field := range []string{
		"subject", "subject_compound", "from", "from_compound", "from_domain", "to", "cc",
		"message_id", "body", "attachment_names", "attachment_types", "attachments", "compound",
	} {
		doc.AddFieldMappingsAt(field, textField())
	}
	doc.AddFieldMappingsAt("date", dateField())
	mapping.DefaultMapping = doc
	index, err = bleve.New(path, mapping)
	if err != nil {
		return nil, fmt.Errorf("create Bleve index: %w", err)
	}
	return index, nil
}

func textField() *mapping.FieldMapping {
	field := bleve.NewTextFieldMapping()
	field.Store = false
	field.DocValues = false
	return field
}

func keywordField() *mapping.FieldMapping {
	field := bleve.NewKeywordFieldMapping()
	field.Store = false
	field.IncludeInAll = false
	return field
}

func dateField() *mapping.FieldMapping {
	field := bleve.NewDateTimeFieldMapping()
	field.Store = false
	field.IncludeInAll = false
	return field
}

// Close releases the combined index and all lazily opened per-user indexes.
func (s *Service) Close() error {
	s.mu.Lock()
	if !s.closing {
		s.closing = true
		s.closeDone = make(chan struct{})
		go s.closeAfterWrites(s.closeDone)
	}
	done := s.closeDone
	timeout := s.closeWriterTimeout
	if timeout <= 0 {
		timeout = searchCloseWriterTimeout
	}
	s.mu.Unlock()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		s.mu.Lock()
		err := s.closeErr
		s.mu.Unlock()
		return err
	case <-timer.C:
		err := errors.New("search index writer did not stop before close")
		s.reportBleveError(bleveErrorContext{Operation: "close-wait"}, err)
		return err
	}
}

func (s *Service) closeAfterWrites(done chan struct{}) {
	s.writes.Wait()
	var first error
	type userIndex struct {
		userID int64
		index  bleve.Index
	}
	s.mu.Lock()
	combined := s.index
	s.index = nil
	indexes := make([]userIndex, 0, len(s.indexes))
	for userID, index := range s.indexes {
		indexes = append(indexes, userIndex{userID: userID, index: index})
	}
	s.indexes = map[int64]bleve.Index{}
	s.mu.Unlock()
	if combined != nil {
		if err := combined.Close(); err != nil {
			s.reportBleveError(bleveErrorContext{Operation: "close-index"}, err)
			first = err
		}
	}
	for _, item := range indexes {
		if err := item.index.Close(); err != nil {
			s.reportBleveError(bleveErrorContext{Operation: "close-index", UserID: item.userID}, err)
			if first == nil {
				first = err
			}
		}
	}
	s.mu.Lock()
	s.closeErr = first
	close(done)
	s.mu.Unlock()
}

// writerForUser returns the write mutex for the target Bleve index. Production
// mode uses one writer per user index; combined-index test mode shares one writer
// so batch commits and deletes never race on the same underlying index handle.
func (s *Service) writerForUser(userID int64) *writerLock {
	key := int64(0)
	if s.perUser {
		key = userID
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writers == nil {
		s.writers = make(map[int64]*writerLock)
	}
	if s.writers[key] == nil {
		s.writers[key] = newWriterLock()
	}
	return s.writers[key]
}

func (s *Service) beginWrite() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return errSearchServiceClosing
	}
	s.writes.Add(1)
	return nil
}

const defaultWriterWaitLogAfter = 5 * time.Second

func (s *Service) writerWaitWarningAfter() time.Duration {
	if s != nil && s.writerWaitLogAfter > 0 {
		return s.writerWaitLogAfter
	}
	return defaultWriterWaitLogAfter
}

func (s *Service) reportBleveWriterWait(waiting, active bleveErrorContext, waited, activeFor time.Duration) {
	logger := log.Printf
	if s != nil && s.bleveErrorLog != nil {
		logger = s.bleveErrorLog
	}
	logger("bleve writer wait operation=%q user_id=%d account_id=%d mailbox_id=%d documents=%d waited=%s active_operation=%q active_user_id=%d active_account_id=%d active_mailbox_id=%d active_documents=%d active_batch_bytes=%d active_first_document_id=%d active_last_document_id=%d active_document_ids=%v active_for=%s",
		waiting.Operation, waiting.UserID, waiting.AccountID, waiting.MailboxID, waiting.Documents, waited.Round(time.Second),
		active.Operation, active.UserID, active.AccountID, active.MailboxID, active.Documents, active.BatchBytes,
		active.FirstDocumentID, active.LastDocumentID, active.DocumentIDs, activeFor.Round(time.Second))
}

func (s *Service) reportBleveCoordinatorWait(diagnostic bleveWriteWaitDiagnostic) {
	logger := log.Printf
	if s != nil && s.bleveErrorLog != nil {
		logger = s.bleveErrorLog
	}
	details := diagnostic.Request.Details
	logger("bleve coordinator wait operation=%q priority=%q user_id=%d account_id=%d mailbox_id=%d documents=%d batch_bytes=%d waited=%s queue_depth=%d active_writes=%d active_bytes=%d same_user_active=%t",
		details.Operation, diagnostic.Request.Priority.String(), details.UserID, details.AccountID, details.MailboxID,
		details.Documents, diagnostic.Request.Bytes, diagnostic.Waited.Round(time.Second), diagnostic.QueueDepth,
		diagnostic.ActiveWrites, diagnostic.ActiveBytes, diagnostic.SameUserActive)
}

// SetActiveWriterStallHandler installs the process-restart signal used when a
// production tenant's synchronous Bleve write exceeds the safety threshold.
// The handler is invoked at most once for this Service, after the durable
// recovery marker has been attempted. It must return promptly.
func (s *Service) SetActiveWriterStallHandler(handler func(userID int64)) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.activeStallHandler = handler
	s.mu.Unlock()
}

func (s *Service) activeWriterStallAfter() time.Duration {
	if s != nil && s.activeStallAfter > 0 {
		return s.activeStallAfter
	}
	return activeWriterStallTimeout
}

// watchActiveWriter is deliberately independent of the caller context. Bleve
// Batch has no cancellation API, so a canceled HTTP or sync request must not
// suppress detection of an operation that still owns the writer gate.
func (s *Service) watchActiveWriter(done <-chan struct{}, details bleveErrorContext) {
	if s == nil || !s.perUser || details.UserID <= 0 {
		return
	}
	timer := time.NewTimer(s.activeWriterStallAfter())
	defer timer.Stop()
	select {
	case <-done:
		return
	case <-timer.C:
		select {
		case <-done:
			return
		default:
		}
	}

	// The durable marker is best-effort: if the database write fails (disk
	// full, DB locked by a wedged writer), restart remains the recovery path
	// because it clears the in-process writer gate and reopens the index.
	// Skipping the restart just because the marker could not be written used
	// to leave this tenant's writer owned forever with no self-healing.
	markerErr := s.MarkSearchIndexRecoveryRequired(details.UserID)
	if markerErr != nil {
		time.Sleep(time.Second)
		markerErr = s.MarkSearchIndexRecoveryRequired(details.UserID)
	}
	s.activeStallOnce.Do(func() {
		s.mu.Lock()
		handler := s.activeStallHandler
		s.mu.Unlock()
		if handler != nil {
			handler(details.UserID)
		}
	})
	logger := log.Printf
	if s.bleveErrorLog != nil {
		logger = s.bleveErrorLog
	}
	logger("bleve active writer stalled operation=%q user_id=%d account_id=%d mailbox_id=%d documents=%d batch_bytes=%d first_document_id=%d last_document_id=%d document_ids=%v threshold=%s marker_written=%t marker_error_type=%T marker_error=%v",
		details.Operation, details.UserID, details.AccountID, details.MailboxID, details.Documents, details.BatchBytes,
		details.FirstDocumentID, details.LastDocumentID, details.DocumentIDs, s.activeWriterStallAfter(), markerErr == nil, markerErr, markerErr)
	if stack := filteredBleveBatchStack(); stack != "" {
		logger("bleve active writer stack user_id=%d operation=%q\n%s", details.UserID, details.Operation, stack)
	}
}

func filteredBleveBatchStack() string {
	buffer := make([]byte, 64*1024)
	for {
		length := runtime.Stack(buffer, true)
		if length < len(buffer) || len(buffer) >= 1024*1024 {
			buffer = buffer[:length]
			break
		}
		buffer = make([]byte, len(buffer)*2)
	}
	return filterBleveBatchStack(buffer)
}

func filterBleveBatchStack(stack []byte) string {
	for _, block := range bytes.Split(stack, []byte("\n\n")) {
		if !bytes.Contains(block, []byte("github.com/blevesearch/bleve")) ||
			!bytes.Contains(block, []byte(".(*Scorch).Batch")) {
			continue
		}
		lines := bytes.Split(block, []byte("\n"))
		filtered := make([]string, 0, len(lines)-1)
		filteredBytes := 0
		for index, rawLine := range lines {
			if index == 0 || len(bytes.TrimSpace(rawLine)) == 0 {
				continue
			}
			line := string(bytes.TrimSpace(rawLine))
			if strings.HasPrefix(line, "created by ") {
				line = strings.SplitN(line, " in goroutine ", 2)[0]
			} else if !bytes.HasPrefix(rawLine, []byte("\t")) {
				if argumentStart := strings.LastIndex(line, "("); argumentStart >= 0 {
					line = line[:argumentStart]
				}
			} else if offset := strings.Index(line, " +0x"); offset >= 0 {
				line = line[:offset]
			}
			projected := filteredBytes + len(line) + 1
			if projected > maxBleveStackBytes {
				break
			}
			filtered = append(filtered, line)
			filteredBytes = projected
		}
		return strings.Join(filtered, "\n")
	}
	return ""
}

// lockWriter waits in FIFO order for one tenant's Bleve writer while allowing
// cancellation to interrupt admission. The warning timer only emits diagnostics;
// it never times out a wait while an uncancellable Bleve batch owns the gate.
func (s *Service) lockWriter(ctx context.Context, writer *writerLock, details bleveErrorContext) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	waiter, acquired := writer.acquireOrQueue()
	if acquired {
		if err := ctx.Err(); err != nil {
			writer.Unlock()
			return err
		}
		return nil
	}
	waitStarted := time.Now()
	warning := time.NewTimer(s.writerWaitWarningAfter())
	defer warning.Stop()
	warningC := warning.C
	for {
		select {
		case <-ctx.Done():
			if !writer.cancelWaiter(waiter) {
				<-waiter.ready
				writer.Unlock()
			}
			return ctx.Err()
		case <-warningC:
			active, activeSince := writer.activeSnapshot()
			activeFor := time.Duration(0)
			if !activeSince.IsZero() {
				activeFor = time.Since(activeSince)
			}
			s.reportBleveWriterWait(details, active, time.Since(waitStarted), activeFor)
			warningC = nil
		case <-waiter.ready:
			if err := ctx.Err(); err != nil {
				writer.Unlock()
				return err
			}
			return nil
		}
	}
}

// indexForUser resolves the correct Bleve handle. In per-user mode it lazily opens
// and caches one index per tenant, with a double-check to avoid duplicate handles
// during concurrent searches or sync writes.
func (s *Service) indexForUser(userID int64) (bleve.Index, error) {
	if !s.perUser {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.closing || s.index == nil {
			return nil, fmt.Errorf("search index is not open")
		}
		return s.index, nil
	}
	if userID == 0 {
		return nil, fmt.Errorf("user id is required for search index")
	}
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return nil, errSearchServiceClosing
	}
	if index := s.indexes[userID]; index != nil {
		s.mu.Unlock()
		return index, nil
	}
	s.mu.Unlock()

	index, err := openIndex(filepath.Join(s.root, strconv.FormatInt(userID, 10), "bleve"))
	if err != nil {
		s.reportBleveError(bleveErrorContext{Operation: "open-index", UserID: userID}, err)
		return nil, err
	}
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		if err := index.Close(); err != nil {
			s.reportBleveError(bleveErrorContext{Operation: "close-raced-index", UserID: userID}, err)
		}
		return nil, errSearchServiceClosing
	}
	if existing := s.indexes[userID]; existing != nil {
		s.mu.Unlock()
		if err := index.Close(); err != nil {
			s.reportBleveError(bleveErrorContext{Operation: "close-duplicate-index", UserID: userID}, err)
		}
		return existing, nil
	}
	s.indexes[userID] = index
	s.mu.Unlock()
	return index, nil
}

// IndexMessage turns a stored message into a Bleve document. SQLite keeps the full
// message metadata/body; Bleve receives searchable text, normalized compound terms,
// filter fields, attachment text extracted from raw .eml, and plugin language data.
func (s *Service) IndexMessage(ctx context.Context, msg store.MessageRecord, attachments []AttachmentDoc) error {
	return s.IndexMessages(ctx, []MessageIndexDocument{{Message: msg, Attachments: attachments}})
}

// IndexMessages writes a batch of message documents to Bleve. The production
// sync path uses this to avoid forcing a separate Bleve commit for every IMAP
// message body while keeping tenant indexes separated by user ID.
func (s *Service) IndexMessages(ctx context.Context, documents []MessageIndexDocument) error {
	if len(documents) == 0 {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	plans, err := planMessageIndexTenants(ctx, documents)
	if err != nil {
		return err
	}
	for _, plan := range plans {
		if err := ctx.Err(); err != nil {
			return err
		}
		index, err := s.indexForUser(plan.userID)
		if err != nil {
			return err
		}
		writer := s.writerForUser(plan.userID)
		iterator := newMessageIndexChunkIterator(plan, messageIndexChunkTargetBytes, buildMessageDocument)
		for iterator.HasNext() {
			chunk, lease, err := s.prepareNextMessageIndexChunk(ctx, iterator)
			if err != nil {
				return err
			}
			if err := s.commitPreparedMessageChunk(ctx, writer, index, chunk, lease); err != nil {
				return err
			}
		}
	}
	return nil
}

// prepareNextMessageIndexChunk obtains a conservative coordinator reservation
// before projecting any message content, then reconciles it to the chunk's
// bounded string payload. The returned lease owns one Service write count.
func (s *Service) prepareNextMessageIndexChunk(ctx context.Context, iterator *messageIndexChunkIterator) (preparedMessageIndexChunk, *bleveWriteLease, error) {
	if iterator == nil || !iterator.HasNext() {
		return preparedMessageIndexChunk{}, nil, fmt.Errorf("prepare message index chunk: iterator is exhausted")
	}
	details := bleveErrorContext{
		Operation: "index-batch", UserID: iterator.userID, BatchBytes: iterator.reservationBytes(),
	}
	lease, err := s.beginWriterOperation(ctx, details)
	if err != nil {
		return preparedMessageIndexChunk{}, nil, err
	}
	chunk, ok, err := iterator.Next(ctx)
	if err != nil {
		s.finishUnstartedWriterOperation(lease)
		return preparedMessageIndexChunk{}, nil, err
	}
	if !ok {
		s.finishUnstartedWriterOperation(lease)
		return preparedMessageIndexChunk{}, nil, fmt.Errorf("prepare message index chunk: iterator produced no chunk")
	}
	lease.UpdateBytes(chunk.projectedBytes)
	return chunk, lease, nil
}

// commitPreparedMessageChunk maps an admitted projection into a Bleve Batch,
// then reconciles the lease to Bleve's actual document size before commit.
func (s *Service) commitPreparedMessageChunk(ctx context.Context, writer *writerLock, index bleve.Index, chunk preparedMessageIndexChunk, lease *bleveWriteLease) error {
	details := chunk.diagnostics("index-batch")
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		s.finishUnstartedWriterOperation(lease)
		return err
	}
	batch, err := chunk.newBleveBatch(index)
	if err != nil {
		details.Operation = "prepare-index-batch"
		s.reportBleveError(details, err)
		s.finishUnstartedWriterOperation(lease)
		return err
	}
	details.BatchBytes = batch.TotalDocsSize()
	lease.UpdateBytes(details.BatchBytes)
	return s.runAdmittedWriterOperation(ctx, writer, details, lease, func() error {
		err := index.Batch(batch)
		s.reportBleveError(details, err)
		return err
	})
}

func (s *Service) finishUnstartedWriterOperation(lease *bleveWriteLease) {
	lease.Release()
	s.writes.Done()
}

// commitBatch transfers the writer lock to the goroutine running Bleve's
// synchronous Batch call. Bleve does not accept a context, so the caller must
// be able to stop waiting while that goroutine retains exclusive ownership of
// the index until the underlying commit actually returns.
func (s *Service) commitBatch(ctx context.Context, writer *writerLock, index bleve.Index, batch *bleve.Batch, details bleveErrorContext) error {
	details.BatchBytes = batch.TotalDocsSize()
	return s.runWriterOperation(ctx, writer, details, func() error {
		err := index.Batch(batch)
		s.reportBleveError(details, err)
		return err
	})
}

func (s *Service) runWriterOperation(ctx context.Context, writer *writerLock, details bleveErrorContext, operation func() error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	lease, err := s.beginWriterOperation(ctx, details)
	if err != nil {
		return err
	}
	return s.runAdmittedWriterOperation(ctx, writer, details, lease, operation)
}

func (s *Service) beginWriterOperation(ctx context.Context, details bleveErrorContext) (*bleveWriteLease, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := s.beginWrite(); err != nil {
		return nil, err
	}
	if s.writeCoordinator == nil {
		return nil, nil
	}
	lease, err := s.writeCoordinator.Acquire(ctx, bleveWriteRequest{
		Details: details, Priority: blevePriorityForOperation(ctx, details.Operation), Bytes: details.BatchBytes,
	})
	if err != nil {
		s.writes.Done()
		return nil, err
	}
	return lease, nil
}

func (s *Service) runAdmittedWriterOperation(ctx context.Context, writer *writerLock, details bleveErrorContext, lease *bleveWriteLease, operation func() error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := s.lockWriter(ctx, writer, details); err != nil {
		s.finishUnstartedWriterOperation(lease)
		return err
	}
	writer.setActive(details)
	result := make(chan error, 1)
	operationDone := make(chan struct{})
	go s.watchActiveWriter(operationDone, details)
	go func() {
		defer s.writes.Done()
		defer lease.Release()
		defer close(operationDone)
		defer func() {
			writer.clearActive()
			writer.Unlock()
		}()
		if err := ctx.Err(); err != nil {
			result <- err
			return
		}
		result <- operation()
	}()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// buildMessageDocument centralizes the SQLite-to-Bleve projection so single and
// batched indexing stay byte-for-byte equivalent.
func buildMessageDocument(msg store.MessageRecord, attachments []AttachmentDoc) map[string]any {
	if msg.IsEncrypted {
		attachments = nil
	}
	names := make([]string, 0, len(attachments))
	contentTypes := make([]string, 0, len(attachments))
	texts := make([]string, 0, len(attachments))
	for _, att := range attachments {
		names = append(names, att.Filename)
		contentTypes = append(contentTypes, att.ContentType)
		if strings.TrimSpace(att.Text) != "" {
			texts = append(texts, att.Text)
		}
	}
	hasAttachment := msg.HasAttachments || len(attachments) > 0
	bodyForIndex := msg.BodyText
	if msg.IsEncrypted {
		bodyForIndex = ""
	}
	subject := boundedIndexText(msg.Subject, maxIndexedHeaderBytes)
	fromAddr := boundedIndexText(msg.FromAddr, maxIndexedHeaderBytes)
	toAddr := boundedIndexText(msg.ToAddr, maxIndexedHeaderBytes)
	ccAddr := boundedIndexText(msg.CCAddr, maxIndexedHeaderBytes)
	messageID := boundedIndexText(msg.MessageIDHeader, maxIndexedHeaderBytes)
	bodyForIndex = boundedIndexText(bodyForIndex, maxIndexedBodyBytes)
	attachmentNames := boundedIndexJoin(names, maxIndexedNamesBytes)
	attachmentTypes := boundedIndexJoin(contentTypes, maxIndexedNamesBytes)
	attachmentText := boundedIndexJoin(texts, maxIndexedAttachmentsBytes)
	compoundBody := store.MessageBodyPreview(bodyForIndex, maxCompoundFieldBytes/4)
	compoundAttachments := store.MessageBodyPreview(attachmentText, maxCompoundFieldBytes/4)
	return map[string]any{
		"user_id":              strconv.FormatInt(msg.UserID, 10),
		"mailbox_id":           strconv.FormatInt(msg.MailboxID, 10),
		"subject":              subject,
		"subject_compound":     boundedIndexText(compoundSearchText(subject), maxIndexedHeaderBytes),
		"from":                 fromAddr,
		"from_compound":        boundedIndexText(compoundSearchText(fromAddr), maxIndexedHeaderBytes),
		"from_domain":          boundedIndexText(emailDomainTerms(fromAddr), maxIndexedHeaderBytes),
		"to":                   toAddr,
		"cc":                   ccAddr,
		"message_id":           messageID,
		"body":                 bodyForIndex,
		"date":                 msg.Date,
		"is_read":              strconv.FormatBool(msg.IsRead),
		"is_starred":           strconv.FormatBool(msg.IsStarred),
		"is_encrypted":         strconv.FormatBool(msg.IsEncrypted),
		"is_signed":            strconv.FormatBool(msg.IsSigned),
		"plugin_language_code": normalizeLanguageCode(msg.LanguageCode),
		"has_attachment":       strconv.FormatBool(hasAttachment),
		"attachment_names":     attachmentNames,
		"attachment_types":     attachmentTypes,
		"attachments":          attachmentText,
		"compound": boundedIndexText(compoundSearchText(
			subject,
			fromAddr,
			toAddr,
			ccAddr,
			messageID,
			compoundBody,
			attachmentNames,
			compoundAttachments,
		), maxCompoundFieldBytes),
	}
}

// DeleteMessage removes one tenant-scoped message document from the appropriate Bleve index.
func (s *Service) DeleteMessage(ctx context.Context, userID, messageID int64) error {
	return s.DeleteMessages(ctx, userID, []int64{messageID})
}

// DeleteMessages removes tenant-scoped message documents in bounded Bleve batches.
// Production indexes are separated per user; combined test indexes additionally
// filter each candidate batch by the indexed user_id before deleting documents.
func (s *Service) DeleteMessages(ctx context.Context, userID int64, messageIDs []int64) error {
	return s.DeleteMessagesWithProgress(ctx, userID, messageIDs, nil)
}

// DeleteMessagesWithProgress removes tenant-scoped documents in bounded commits
// and reports each committed batch. The callback runs without the Bleve writer
// lock so callers may persist UI progress safely.
func (s *Service) DeleteMessagesWithProgress(ctx context.Context, userID int64, messageIDs []int64, onBatch func(int) error) error {
	if userID <= 0 || len(messageIDs) == 0 {
		return nil
	}
	unique := make([]int64, 0, len(messageIDs))
	seen := make(map[int64]struct{}, len(messageIDs))
	for _, id := range messageIDs {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}
	if len(unique) == 0 {
		return nil
	}
	index, err := s.indexForUser(userID)
	if err != nil {
		return err
	}
	const batchSize = 500
	for start := 0; start < len(unique); start += batchSize {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		end := start + batchSize
		if end > len(unique) {
			end = len(unique)
		}
		deleteIDs := make([]string, 0, end-start)
		for _, id := range unique[start:end] {
			deleteIDs = append(deleteIDs, strconv.FormatInt(id, 10))
		}
		committed := 0
		writer := s.writerForUser(userID)
		err = s.runWriterOperation(ctx, writer, bleveErrorContext{
			Operation: "delete-batch", UserID: userID, Documents: len(deleteIDs),
		}, func() error {
			if !s.perUser {
				userQuery := bleve.NewTermQuery(strconv.FormatInt(userID, 10))
				userQuery.SetField("user_id")
				docQuery := bleve.NewDocIDQuery(deleteIDs)
				request := bleve.NewSearchRequestOptions(bleve.NewConjunctionQuery(userQuery, docQuery), len(deleteIDs), 0, false)
				result, searchErr := index.SearchInContext(ctx, request)
				if searchErr != nil {
					s.reportBleveError(bleveErrorContext{
						Operation: "delete-lookup", UserID: userID, Documents: len(deleteIDs),
					}, searchErr)
					return searchErr
				}
				deleteIDs = deleteIDs[:0]
				for _, hit := range result.Hits {
					deleteIDs = append(deleteIDs, hit.ID)
				}
			}
			if len(deleteIDs) == 0 {
				return nil
			}
			batch := index.NewBatch()
			for _, id := range deleteIDs {
				batch.Delete(id)
			}
			if err := index.Batch(batch); err != nil {
				s.reportBleveError(bleveErrorContext{
					Operation: "delete-batch", UserID: userID, Documents: len(deleteIDs),
				}, err)
				return err
			}
			committed = len(deleteIDs)
			return nil
		})
		if err != nil {
			return err
		}
		if committed == 0 {
			continue
		}
		if onBatch != nil {
			if err := onBatch(committed); err != nil {
				return err
			}
		}
	}
	return nil
}

// CountMailboxMessages counts indexed documents for a mailbox so settings can show search-index progress.
func (s *Service) CountMailboxMessages(ctx context.Context, userID, mailboxID int64) (int, error) {
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	default:
	}
	if userID == 0 || mailboxID == 0 {
		return 0, nil
	}
	index, err := s.indexForUser(userID)
	if err != nil {
		return 0, err
	}
	res, err := index.SearchInContext(ctx, mailboxSearchRequest(userID, mailboxID, 0, 0))
	if err != nil {
		s.reportBleveError(bleveErrorContext{Operation: "count-mailbox", UserID: userID, MailboxID: mailboxID}, err)
		return 0, err
	}
	return int(res.Total), nil
}

// MessageIDsIndexed reports which of the supplied local message IDs currently have
// documents in the user-scoped Bleve index. Search repair uses this to cheaply
// catch messages that were stored in SQLite but missed a previous index commit.
func (s *Service) MessageIDsIndexed(ctx context.Context, userID int64, messageIDs []int64) (map[int64]bool, error) {
	out := map[int64]bool{}
	if userID == 0 || len(messageIDs) == 0 {
		return out, nil
	}
	docIDs := make([]string, 0, len(messageIDs))
	seen := map[int64]bool{}
	for _, id := range messageIDs {
		if id == 0 || seen[id] {
			continue
		}
		seen[id] = true
		docIDs = append(docIDs, strconv.FormatInt(id, 10))
	}
	if len(docIDs) == 0 {
		return out, nil
	}
	index, err := s.indexForUser(userID)
	if err != nil {
		return nil, err
	}
	userQuery := bleve.NewTermQuery(strconv.FormatInt(userID, 10))
	userQuery.SetField("user_id")
	docQuery := bleve.NewDocIDQuery(docIDs)
	req := bleve.NewSearchRequestOptions(bleve.NewConjunctionQuery(userQuery, docQuery), len(docIDs), 0, false)
	res, err := index.SearchInContext(ctx, req)
	if err != nil {
		s.reportBleveError(bleveErrorContext{Operation: "lookup-message-ids", UserID: userID, Documents: len(docIDs)}, err)
		return nil, err
	}
	for _, hit := range res.Hits {
		id, err := strconv.ParseInt(hit.ID, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parse indexed message hit id: %w", err)
		}
		out[id] = true
	}
	return out, nil
}

// MailboxMessageIDs returns the local message IDs currently present in one mailbox's search index.
func (s *Service) MailboxMessageIDs(ctx context.Context, userID, mailboxID int64) (map[int64]bool, error) {
	out := map[int64]bool{}
	if userID == 0 || mailboxID == 0 {
		return out, nil
	}
	index, err := s.indexForUser(userID)
	if err != nil {
		return nil, err
	}
	const pageSize = 1000
	for offset := 0; ; offset += pageSize {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		res, err := index.SearchInContext(ctx, mailboxSearchRequest(userID, mailboxID, pageSize, offset))
		if err != nil {
			s.reportBleveError(bleveErrorContext{Operation: "list-mailbox-ids", UserID: userID, MailboxID: mailboxID}, err)
			return nil, err
		}
		if len(res.Hits) == 0 {
			return out, nil
		}
		for _, hit := range res.Hits {
			id, err := strconv.ParseInt(hit.ID, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("parse mailbox search hit id: %w", err)
			}
			out[id] = true
		}
		if len(res.Hits) < pageSize {
			return out, nil
		}
	}
}

// PurgeMailbox removes all search documents for one tenant-owned mailbox. It does not touch SQLite or IMAP.
func (s *Service) PurgeMailbox(ctx context.Context, userID, mailboxID int64) (int, error) {
	return s.PurgeMailboxWithProgress(ctx, userID, mailboxID, nil)
}

// PurgeMailboxWithProgress removes mailbox search documents and reports each deleted batch.
func (s *Service) PurgeMailboxWithProgress(ctx context.Context, userID, mailboxID int64, onBatch func(int) error) (int, error) {
	if userID == 0 || mailboxID == 0 {
		return 0, nil
	}
	index, err := s.indexForUser(userID)
	if err != nil {
		return 0, err
	}
	deleted := 0
	// Keep purge commits short so mailbox rebuild progress remains visible and
	// other mailbox writers for the same tenant get frequent opportunities.
	const batchSize = 100
	for {
		select {
		case <-ctx.Done():
			return deleted, ctx.Err()
		default:
		}
		res, err := index.SearchInContext(ctx, mailboxSearchRequest(userID, mailboxID, batchSize, 0))
		if err != nil {
			s.reportBleveError(bleveErrorContext{Operation: "purge-mailbox-lookup", UserID: userID, MailboxID: mailboxID}, err)
			return deleted, err
		}
		if len(res.Hits) == 0 {
			return deleted, nil
		}
		batch := index.NewBatch()
		for _, hit := range res.Hits {
			batch.Delete(hit.ID)
		}
		writer := s.writerForUser(userID)
		err = s.commitBatch(ctx, writer, index, batch, bleveErrorContext{
			Operation: "purge-mailbox-batch", UserID: userID, MailboxID: mailboxID, Documents: len(res.Hits),
		})
		if err != nil {
			return deleted, err
		}
		deleted += len(res.Hits)
		if onBatch != nil {
			if err := onBatch(len(res.Hits)); err != nil {
				return deleted, err
			}
		}
	}
}

func mailboxSearchRequest(userID, mailboxID int64, size, from int) *bleve.SearchRequest {
	userQuery := bleve.NewTermQuery(strconv.FormatInt(userID, 10))
	userQuery.SetField("user_id")
	mailboxQuery := bleve.NewTermQuery(strconv.FormatInt(mailboxID, 10))
	mailboxQuery.SetField("mailbox_id")
	req := bleve.NewSearchRequestOptions(bleve.NewConjunctionQuery(userQuery, mailboxQuery), size, from, false)
	req.SortBy([]string{"_id"})
	return req
}

// CountUserMessages returns the number of message documents currently present
// in the user's Bleve index. Storage stats use it to show how much mail is
// actually searchable without opening another user's index.
func (s *Service) CountUserMessages(ctx context.Context, userID int64) (int, error) {
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	default:
	}
	if userID == 0 {
		return 0, nil
	}
	q := bleve.NewTermQuery(strconv.FormatInt(userID, 10))
	q.SetField("user_id")
	req := bleve.NewSearchRequestOptions(q, 0, 0, false)
	index, err := s.indexForUser(userID)
	if err != nil {
		return 0, err
	}
	res, err := index.SearchInContext(ctx, req)
	if err != nil {
		s.reportBleveError(bleveErrorContext{Operation: "count-user", UserID: userID}, err)
		return 0, err
	}
	return int(res.Total), nil
}

// Search runs a query and returns matching message IDs with default options.
func (s *Service) Search(ctx context.Context, userID int64, queryText string, limit, offset int) ([]int64, error) {
	return s.SearchWithOptions(ctx, userID, queryText, limit, offset, SearchOptions{})
}

// SearchWithOptions returns only IDs for list-building callers that will hydrate
// full conversations from SQLite.
func (s *Service) SearchWithOptions(ctx context.Context, userID int64, queryText string, limit, offset int, opts SearchOptions) ([]int64, error) {
	res, err := s.search(ctx, userID, queryText, limit, offset, opts, false)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(res.Hits))
	for _, hit := range res.Hits {
		id, err := strconv.ParseInt(hit.ID, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parse search hit id: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// SearchHitsWithOptions asks Bleve for term locations so the UI can show what
// matched in snippets, attachments, and the message-detail iframe.
func (s *Service) SearchHitsWithOptions(ctx context.Context, userID int64, queryText string, limit, offset int, opts SearchOptions) ([]Hit, error) {
	res, err := s.search(ctx, userID, queryText, limit, offset, opts, true)
	if err != nil {
		return nil, err
	}
	hits := make([]Hit, 0, len(res.Hits))
	for _, hit := range res.Hits {
		id, err := strconv.ParseInt(hit.ID, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parse search hit id: %w", err)
		}
		hits = append(hits, Hit{ID: id, Score: hit.Score, Terms: hitMatchTerms(queryText, hit.Locations), Fields: hitMatchFields(hit.Locations), QueryTerms: hitMatchQueryTerms(queryText, hit.Locations)})
	}
	return hits, nil
}

// MatchMessage re-runs the current search against one document with default options.
func (s *Service) MatchMessage(ctx context.Context, userID, messageID int64, queryText string) (Hit, bool, error) {
	return s.MatchMessageWithOptions(ctx, userID, messageID, queryText, SearchOptions{})
}

// MatchMessageWithOptions uses the same query-time behavior as the search results
// list so message-detail highlighting stays consistent with the user's profile.
func (s *Service) MatchMessageWithOptions(ctx context.Context, userID, messageID int64, queryText string, opts SearchOptions) (Hit, bool, error) {
	result, ok, err := s.explainMessageWithOptions(ctx, userID, messageID, queryText, opts, false)
	if err != nil || !ok {
		return Hit{}, ok, err
	}
	return Hit{ID: result.ID, Score: result.Score, Terms: result.Terms, Fields: result.Fields, QueryTerms: result.QueryTerms}, true, nil
}

// ExplainMessageWithOptions re-runs the same best-match query against one
// document and requests Bleve's scorer explanation. Message-detail UI calls this
// on demand from the action menu, not during normal search result rendering.
func (s *Service) ExplainMessageWithOptions(ctx context.Context, userID, messageID int64, queryText string, opts SearchOptions) (ExplanationResult, bool, error) {
	return s.explainMessageWithOptions(ctx, userID, messageID, queryText, opts, true)
}

// ExplainMessagesWithOptions runs the same query against a bounded set of local
// message IDs and returns the highest-scoring matching message. Message-detail
// explanations use this for grouped conversations so the panel can explain the
// exact Bleve hit that made the conversation appear in search results.
func (s *Service) ExplainMessagesWithOptions(ctx context.Context, userID int64, messageIDs []int64, queryText string, opts SearchOptions) (ExplanationResult, bool, error) {
	return s.explainMessageIDsWithOptions(ctx, userID, messageIDs, queryText, opts, true)
}

func (s *Service) ScoreMessageWithOptions(ctx context.Context, userID, messageID int64, queryText string, opts SearchOptions) (float64, bool, error) {
	result, ok, err := s.explainMessageWithOptions(ctx, userID, messageID, queryText, opts, false)
	if err != nil || !ok {
		return 0, ok, err
	}
	return result.Score, true, nil
}

func (s *Service) explainMessageWithOptions(ctx context.Context, userID, messageID int64, queryText string, opts SearchOptions, explain bool) (ExplanationResult, bool, error) {
	return s.explainMessageIDsWithOptions(ctx, userID, []int64{messageID}, queryText, opts, explain)
}

func (s *Service) explainMessageIDsWithOptions(ctx context.Context, userID int64, messageIDs []int64, queryText string, opts SearchOptions, explain bool) (ExplanationResult, bool, error) {
	select {
	case <-ctx.Done():
		return ExplanationResult{}, false, ctx.Err()
	default:
	}
	queryText = strings.TrimSpace(queryText)
	if userID == 0 || len(messageIDs) == 0 || queryText == "" {
		return ExplanationResult{}, false, nil
	}
	docIDs := make([]string, 0, len(messageIDs))
	seenDocIDs := map[int64]bool{}
	for _, messageID := range messageIDs {
		if messageID == 0 || seenDocIDs[messageID] {
			continue
		}
		seenDocIDs[messageID] = true
		docIDs = append(docIDs, strconv.FormatInt(messageID, 10))
	}
	if len(docIDs) == 0 {
		return ExplanationResult{}, false, nil
	}
	docQuery := bleve.NewDocIDQuery(docIDs)
	query := bleve.NewConjunctionQuery(buildQuery(userID, queryText, opts), docQuery)
	req := bleve.NewSearchRequestOptions(query, 1, 0, false)
	req.IncludeLocations = true
	req.Explain = explain
	index, err := s.indexForUser(userID)
	if err != nil {
		return ExplanationResult{}, false, err
	}
	res, err := index.SearchInContext(ctx, req)
	if err != nil {
		s.reportBleveError(bleveErrorContext{Operation: "explain", UserID: userID, Documents: len(docIDs)}, err)
		return ExplanationResult{}, false, err
	}
	if len(res.Hits) == 0 {
		return ExplanationResult{}, false, nil
	}
	hit := res.Hits[0]
	matchedID, err := strconv.ParseInt(hit.ID, 10, 64)
	if err != nil {
		return ExplanationResult{}, false, fmt.Errorf("parse explain hit id: %w", err)
	}
	return ExplanationResult{
		ID:                matchedID,
		Score:             hit.Score,
		Terms:             hitMatchTerms(queryText, hit.Locations),
		Fields:            hitMatchFields(hit.Locations),
		QueryTerms:        hitMatchQueryTerms(queryText, hit.Locations),
		FieldMatches:      hitMatchFieldTerms(queryText, hit.Locations),
		TermContributions: scoreTermContributions(queryText, hit.Expl),
		Raw:               hit.Expl,
	}, true, nil
}

// search applies pagination bounds, builds the tenant-scoped query, optionally
// asks for term locations, and leaves result hydration to web/store layers.
func (s *Service) search(ctx context.Context, userID int64, queryText string, limit, offset int, opts SearchOptions, includeLocations bool) (*bleve.SearchResult, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	query := buildQuery(userID, queryText, opts)
	req := bleve.NewSearchRequestOptions(query, limit, offset, false)
	req.IncludeLocations = includeLocations
	index, err := s.indexForUser(userID)
	if err != nil {
		return nil, err
	}
	result, err := index.SearchInContext(ctx, req)
	if err != nil {
		s.reportBleveError(bleveErrorContext{Operation: "search", UserID: userID}, err)
		return nil, err
	}
	return result, nil
}

// buildQuery is the high-level search grammar bridge. It turns Gmail-like filters
// into required clauses, text into fuzzy/compound matching clauses, and best-match
// ranking into sender-history should clauses plus a custom-score recency nudge.
func buildQuery(userID int64, queryText string, opts SearchOptions) blevequery.Query {
	parsed := parseQuery(queryText)
	behavior := opts.Behavior.normalized()
	parts := []blevequery.Query{}

	userQuery := bleve.NewTermQuery(strconv.FormatInt(userID, 10))
	userQuery.SetField("user_id")
	parts = append(parts, userQuery)

	if parsed.HasAttachment != nil {
		q := bleve.NewTermQuery(strconv.FormatBool(*parsed.HasAttachment))
		q.SetField("has_attachment")
		parts = append(parts, q)
	}
	if parsed.IsRead != nil {
		q := bleve.NewTermQuery(strconv.FormatBool(*parsed.IsRead))
		q.SetField("is_read")
		parts = append(parts, q)
	}
	if parsed.IsStarred != nil {
		q := bleve.NewTermQuery(strconv.FormatBool(*parsed.IsStarred))
		q.SetField("is_starred")
		parts = append(parts, q)
	}
	if parsed.IsEncrypted != nil {
		q := bleve.NewTermQuery(strconv.FormatBool(*parsed.IsEncrypted))
		q.SetField("is_encrypted")
		parts = append(parts, q)
	}
	if parsed.IsSigned != nil {
		q := bleve.NewTermQuery(strconv.FormatBool(*parsed.IsSigned))
		q.SetField("is_signed")
		parts = append(parts, q)
	}
	if parsed.Language != "" {
		pluginQ := bleve.NewTermQuery(parsed.Language)
		pluginQ.SetField("plugin_language_code")
		parts = append(parts, pluginQ)
	}
	if parsed.Filename != "" {
		q := bleve.NewMatchQuery(parsed.Filename)
		q.SetField("attachment_names")
		parts = append(parts, q)
	}
	if parsed.Subject != "" {
		q := bleve.NewMatchPhraseQuery(parsed.Subject)
		q.SetField("subject")
		parts = append(parts, q)
	}
	if parsed.From != "" {
		parts = append(parts, fromQuery(parsed.From))
	}
	if parsed.To != "" {
		q := bleve.NewMatchQuery(parsed.To)
		q.SetField("to")
		parts = append(parts, q)
	}
	if parsed.CC != "" {
		q := bleve.NewMatchQuery(parsed.CC)
		q.SetField("cc")
		parts = append(parts, q)
	}
	if !parsed.After.IsZero() || !parsed.Before.IsZero() {
		startInclusive := true
		endInclusive := false
		q := bleve.NewDateRangeInclusiveQuery(parsed.After, parsed.Before, &startInclusive, &endInclusive)
		q.SetField("date")
		parts = append(parts, q)
	}
	if parsed.Text == "" {
		if len(parts) == 1 {
			parts = append(parts, bleve.NewMatchAllQuery())
		}
	} else {
		parts = append(parts, textQuery(parsed.Text, parsed.TextQuoted, behavior))
	}
	mustNot := negatedTextQueries(parsed.NegatedText, behavior)
	must := bleve.NewConjunctionQuery(parts...)
	base := blevequery.Query(must)
	var should []blevequery.Query
	if behavior.SenderBoostScale > 0 {
		should = append(should, senderBoostQueries(opts.SenderBoosts, behavior.SenderBoostScale)...)
	}
	if behavior.ContactBoostScale > 0 {
		should = append(should, senderBoostQueries(opts.ContactBoosts, behavior.ContactBoostScale)...)
	}
	if len(should) > 0 || len(mustNot) > 0 {
		base = blevequery.NewBooleanQuery([]blevequery.Query{must}, should, mustNot)
	}
	if parsed.After.IsZero() && parsed.Before.IsZero() {
		base = recencyScoreQuery(base, time.Now().UTC(), behavior)
	}
	return base
}

func recencyScoreQuery(query blevequery.Query, now time.Time, behavior normalizedSearchBehavior) blevequery.Query {
	if query == nil || now.IsZero() {
		return query
	}
	buckets := RecencyRankBuckets(behavior.RecencyBias)
	if len(buckets) == 0 {
		return query
	}
	return blevequery.NewCustomScoreQueryWithScorer(query, func(match *blevesearch.DocumentMatch) float64 {
		messageTime, ok := documentMatchDate(match.Fields["date"])
		if !ok {
			return match.Score
		}
		return match.Score + recencyRankBoostForDate(now, messageTime, buckets)
	}, []string{"date"}, nil)
}

func recencyRankBoostForDate(now, messageTime time.Time, buckets []RecencyRankBucket) float64 {
	if now.IsZero() || messageTime.IsZero() {
		return 0
	}
	age := now.Sub(messageTime)
	if age < 0 {
		age = 0
	}
	for _, bucket := range buckets {
		if age <= bucket.Age {
			return bucket.Boost
		}
	}
	return 0
}

func documentMatchDate(value any) (time.Time, bool) {
	switch v := value.(type) {
	case time.Time:
		return v, !v.IsZero()
	case string:
		return parseDocumentMatchDateString(v)
	case []string:
		for _, item := range v {
			if parsed, ok := parseDocumentMatchDateString(item); ok {
				return parsed, true
			}
		}
	case []any:
		for _, item := range v {
			if parsed, ok := documentMatchDate(item); ok {
				return parsed, true
			}
		}
	case float64:
		if v > 0 {
			return time.Unix(0, int64(v)).UTC(), true
		}
	case int64:
		if v > 0 {
			return time.Unix(0, v).UTC(), true
		}
	case int:
		if v > 0 {
			return time.Unix(0, int64(v)).UTC(), true
		}
	}
	return time.Time{}, false
}

func parseDocumentMatchDateString(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	if ns, err := strconv.ParseInt(value, 10, 64); err == nil && ns > 0 {
		if ns > 1_000_000_000_000 {
			return time.Unix(0, ns).UTC(), true
		}
		return time.Unix(ns, 0).UTC(), true
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05 -0700 MST", "2006-01-02 15:04:05 -0700"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UTC(), true
		}
	}
	return time.Time{}, false
}

func senderBoostQueries(boosts []SenderBoost, scale float64) []blevequery.Query {
	if len(boosts) == 0 || scale <= 0 {
		return nil
	}
	if len(boosts) > 40 {
		boosts = boosts[:40]
	}
	out := make([]blevequery.Query, 0, len(boosts))
	for _, boost := range boosts {
		sender := strings.TrimSpace(boost.Sender)
		if sender == "" || boost.Boost <= 0 {
			continue
		}
		q := bleve.NewMatchQuery(sender)
		q.SetField("from")
		q.SetBoost(boost.Boost * scale)
		out = append(out, q)
	}
	return out
}

// fromQuery gives full email/domain searches stricter treatment than general text
// so queries like from:example.com or from:user@example.com do not devolve into
// loose body-word matches.
func fromQuery(text string) blevequery.Query {
	text = strings.Trim(strings.TrimSpace(text), `"`)
	if q := emailAddressQuery(text); q != nil {
		return boostQuery(q, 16)
	}
	var disjuncts []blevequery.Query

	domain := strings.TrimPrefix(strings.ToLower(text), "@")
	if at := strings.LastIndex(domain, "@"); at >= 0 {
		domain = domain[at+1:]
	}
	domainMode := false
	if terms := domainQueryTerms(domain); len(terms) > 1 {
		domainMode = true
		disjuncts = append(disjuncts, boostQuery(termConjunction("from", terms), 12))
		disjuncts = append(disjuncts, boostQuery(termConjunction("from_domain", terms), 18))
	} else {
		q := bleve.NewMatchQuery(text)
		q.SetField("from")
		q.SetBoost(8)
		disjuncts = append(disjuncts, q)
	}
	if normalized := normalizeSearchText(text); !domainMode && normalized != "" && normalized != text {
		nq := bleve.NewMatchQuery(normalized)
		nq.SetField("from")
		nq.SetBoost(4)
		disjuncts = append(disjuncts, nq)
	}
	return bleve.NewDisjunctionQuery(disjuncts...)
}

// textQuery implements the user-facing search feel. Quoted text is literal and
// avoids fuzzy word breakdown; unquoted text can use all-term matching, compact
// phrase boosts, domain matching, and controlled fuzziness for typos.
func textQuery(text string, quoted bool, behavior normalizedSearchBehavior) blevequery.Query {
	var disjuncts []blevequery.Query
	normalized := normalizeSearchText(text)
	terms := strings.Fields(normalized)
	if quoted {
		for _, field := range literalTextFields(behavior) {
			phrase := bleve.NewMatchPhraseQuery(text)
			phrase.SetField(field)
			phrase.SetBoost(4)
			disjuncts = append(disjuncts, phrase)
			if len(terms) == 1 {
				q := bleve.NewMatchQuery(text)
				q.SetField(field)
				q.SetBoost(3)
				disjuncts = append(disjuncts, q)
				if normalized != "" && normalized != strings.ToLower(strings.TrimSpace(text)) {
					nq := bleve.NewMatchQuery(normalized)
					nq.SetField(field)
					nq.SetBoost(2)
					disjuncts = append(disjuncts, nq)
				}
			}
		}
		if len(disjuncts) == 1 {
			return disjuncts[0]
		}
		return bleve.NewDisjunctionQuery(disjuncts...)
	}
	if q := domainTextQuery(text); q != nil {
		disjuncts = append(disjuncts, q)
	} else if len(terms) > 1 {
		disjuncts = append(disjuncts, allTextTermsQuery(terms, behavior))
		disjuncts = append(disjuncts, textPhraseQuery(text, 1.5, behavior))
	} else {
		disjuncts = append(disjuncts, defaultOrWeightedTextQuery(text, behavior))
		if normalized != "" && normalized != text {
			disjuncts = append(disjuncts, defaultOrWeightedTextQuery(normalized, behavior))
		}
	}
	joined := strings.ReplaceAll(normalized, " ", "")
	if joined != "" {
		if joined != normalized {
			disjuncts = append(disjuncts, boostQuery(defaultOrWeightedTextQuery(joined, behavior), 0.8))
		}
		if behavior.CompactSplitting && (joined != normalized || canSplitCompactTerm(joined)) {
			disjuncts = append(disjuncts, exactCompactQueries(joined, behavior)...)
			disjuncts = append(disjuncts, splitPhraseQueries(joined, behavior)...)
			disjuncts = append(disjuncts, splitWordQueries(joined, behavior)...)
			if behavior.fuzzyEnabled() && behavior.attachmentBoostScale() > 0 && len(joined) >= 5 {
				fq := bleve.NewFuzzyQuery(joined)
				fq.SetField("compound")
				fq.SetFuzziness(fuzzinessFor(joined, behavior))
				fq.SetBoost(0.6 * behavior.attachmentBoostScale())
				disjuncts = append(disjuncts, fq)
			}
		}
	}
	if behavior.fuzzyEnabled() && len(terms) <= 1 {
		for _, term := range terms {
			if len(term) >= 5 {
				disjuncts = append(disjuncts, fuzzyTextQuery(term, 0.5, behavior))
			}
		}
	}
	if len(disjuncts) == 1 {
		return disjuncts[0]
	}
	return bleve.NewDisjunctionQuery(disjuncts...)
}

func negatedTextQueries(terms []negatedTextTerm, behavior normalizedSearchBehavior) []blevequery.Query {
	out := make([]blevequery.Query, 0, len(terms))
	for _, term := range terms {
		if q := negatedTextQuery(term.Text, term.Quoted, behavior); q != nil {
			out = append(out, q)
		}
	}
	return out
}

func negatedTextQuery(text string, quoted bool, behavior normalizedSearchBehavior) blevequery.Query {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	if quoted || strings.ContainsAny(text, " \t\r\n") {
		disjuncts := make([]blevequery.Query, 0, len(literalTextFields(behavior)))
		for _, field := range literalTextFields(behavior) {
			phrase := bleve.NewMatchPhraseQuery(text)
			phrase.SetField(field)
			disjuncts = append(disjuncts, phrase)
		}
		return bleve.NewDisjunctionQuery(disjuncts...)
	}
	disjuncts := []blevequery.Query{baseTextTermQuery(text, behavior)}
	normalized := normalizeSearchText(text)
	if normalized != "" && normalized != strings.ToLower(strings.TrimSpace(text)) {
		disjuncts = append(disjuncts, baseTextTermQuery(normalized, behavior))
	}
	if len(disjuncts) == 1 {
		return disjuncts[0]
	}
	return bleve.NewDisjunctionQuery(disjuncts...)
}

func literalTextFields(behavior normalizedSearchBehavior) []string {
	fields := []string{"subject", "from", "to", "cc", "message_id", "body"}
	if behavior.attachmentBoostScale() > 0 {
		fields = append(fields, "attachment_names", "attachment_types", "attachments")
	}
	return fields
}

func allTextTermsQuery(terms []string, behavior normalizedSearchBehavior) blevequery.Query {
	parts := make([]blevequery.Query, 0, len(terms))
	for _, term := range terms {
		parts = append(parts, textTermQuery(term, behavior))
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return bleve.NewConjunctionQuery(parts...)
}

func textTermQuery(term string, behavior normalizedSearchBehavior) blevequery.Query {
	queries := []blevequery.Query{baseTextTermQuery(term, behavior)}
	if behavior.fuzzyEnabled() && len(term) >= 5 {
		queries = append(queries, fuzzyTextQuery(term, 0.5, behavior))
	}
	if len(queries) == 1 {
		return queries[0]
	}
	return bleve.NewDisjunctionQuery(queries...)
}

func baseTextTermQuery(term string, behavior normalizedSearchBehavior) blevequery.Query {
	disjuncts := []blevequery.Query{
		boostedMatch("from", term, 6),
		boostedMatch("subject", term, 4),
		boostedMatch("to", term, 2),
		boostedMatch("cc", term, 2),
		boostedMatch("message_id", term, 1),
		boostedMatch("body", term, 1),
	}
	if scale := behavior.attachmentBoostScale(); scale > 0 {
		disjuncts = append(disjuncts,
			boostedMatch("attachment_names", term, 1.2*scale),
			boostedMatch("attachment_types", term, 0.8*scale),
			boostedMatch("attachments", term, scale),
		)
	}
	return bleve.NewDisjunctionQuery(disjuncts...)
}

func defaultOrWeightedTextQuery(term string, behavior normalizedSearchBehavior) blevequery.Query {
	return baseTextTermQuery(term, behavior)
}

func textPhraseQuery(text string, boost float64, behavior normalizedSearchBehavior) blevequery.Query {
	disjuncts := []blevequery.Query{
		boostedPhrase("from", text, boost*6),
		boostedPhrase("subject", text, boost*4),
		boostedPhrase("to", text, boost*2),
		boostedPhrase("cc", text, boost*2),
		boostedPhrase("message_id", text, boost),
		boostedPhrase("body", text, boost),
	}
	if scale := behavior.attachmentBoostScale(); scale > 0 {
		disjuncts = append(disjuncts,
			boostedPhrase("attachment_names", text, boost*1.2*scale),
			boostedPhrase("attachment_types", text, boost*0.8*scale),
			boostedPhrase("attachments", text, boost*scale),
		)
	}
	return bleve.NewDisjunctionQuery(disjuncts...)
}

func fuzzyTextQuery(term string, boost float64, behavior normalizedSearchBehavior) blevequery.Query {
	disjuncts := []blevequery.Query{
		boostedFuzzy("from", term, boost*3, behavior),
		boostedFuzzy("subject", term, boost*2, behavior),
		boostedFuzzy("to", term, boost, behavior),
		boostedFuzzy("cc", term, boost, behavior),
		boostedFuzzy("message_id", term, boost, behavior),
		boostedFuzzy("body", term, boost, behavior),
	}
	if scale := behavior.attachmentBoostScale(); scale > 0 {
		disjuncts = append(disjuncts,
			boostedFuzzy("attachment_names", term, boost*1.2*scale, behavior),
			boostedFuzzy("attachment_types", term, boost*0.8*scale, behavior),
			boostedFuzzy("attachments", term, boost*scale, behavior),
		)
	}
	return bleve.NewDisjunctionQuery(disjuncts...)
}

func boostedFuzzy(field, term string, boost float64, behavior normalizedSearchBehavior) blevequery.Query {
	q := bleve.NewFuzzyQuery(term)
	q.SetField(field)
	q.SetFuzziness(fuzzinessFor(term, behavior))
	q.SetBoost(boost)
	return q
}

// hitMatchTerms filters Bleve's raw location map down to user-meaningful terms,
// excluding implementation fields and terms unrelated to the original query.
func hitMatchTerms(queryText string, locations blevesearch.FieldTermLocationMap) []string {
	if len(locations) == 0 {
		return nil
	}
	needles := queryNeedles(parseQuery(queryText))
	seen := map[string]bool{}
	var terms []string
	for field, termLocations := range locations {
		if !matchTermField(field) {
			continue
		}
		for term := range termLocations {
			term = strings.TrimSpace(strings.ToLower(term))
			if term == "" || seen[term] || !termRelevantToQuery(term, needles) {
				continue
			}
			seen[term] = true
			terms = append(terms, term)
		}
	}
	sort.Slice(terms, func(i, j int) bool {
		if len([]rune(terms[i])) == len([]rune(terms[j])) {
			return terms[i] < terms[j]
		}
		return len([]rune(terms[i])) > len([]rune(terms[j]))
	})
	if len(terms) > 10 {
		terms = terms[:10]
	}
	return terms
}

func matchTermField(field string) bool {
	switch field {
	case "subject", "subject_compound", "from", "from_compound", "from_domain", "to", "cc", "message_id", "body", "attachment_names", "attachment_types", "attachments", "compound":
		return true
	default:
		return false
	}
}

func hitMatchFields(locations blevesearch.FieldTermLocationMap) []string {
	if len(locations) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var fields []string
	for field := range locations {
		if !matchTermField(field) || seen[field] {
			continue
		}
		seen[field] = true
		fields = append(fields, field)
	}
	sort.Strings(fields)
	return fields
}

func hitMatchFieldTerms(queryText string, locations blevesearch.FieldTermLocationMap) []FieldTermMatch {
	if len(locations) == 0 {
		return nil
	}
	needles := queryNeedles(parseQuery(queryText))
	fields := hitMatchFields(locations)
	out := make([]FieldTermMatch, 0, len(fields))
	for _, field := range fields {
		termLocations := locations[field]
		seen := map[string]bool{}
		terms := make([]string, 0, len(termLocations))
		for term := range termLocations {
			term = strings.TrimSpace(strings.ToLower(term))
			if term == "" || seen[term] || !termRelevantToQuery(term, needles) {
				continue
			}
			seen[term] = true
			terms = append(terms, term)
		}
		sort.Slice(terms, func(i, j int) bool {
			if len([]rune(terms[i])) == len([]rune(terms[j])) {
				return terms[i] < terms[j]
			}
			return len([]rune(terms[i])) > len([]rune(terms[j]))
		})
		if len(terms) > 8 {
			terms = terms[:8]
		}
		out = append(out, FieldTermMatch{Field: field, Terms: terms})
	}
	return out
}

func hitMatchQueryTerms(queryText string, locations blevesearch.FieldTermLocationMap) []string {
	matches := hitMatchFieldTerms(queryText, locations)
	if len(matches) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, match := range matches {
		for _, term := range match.Terms {
			queryTerm := match.Field + ":" + term
			if seen[queryTerm] {
				continue
			}
			seen[queryTerm] = true
			out = append(out, queryTerm)
		}
	}
	if len(out) > 12 {
		out = out[:12]
	}
	return out
}

var explanationFieldTermRE = regexp.MustCompile(`^(?:weight|fieldWeight|queryWeight)\(([^:()\s]+):([^\s\)^]+)`)
var explanationTermFreqRE = regexp.MustCompile(`termFreq\(([^:()\s]+):([^\)=]+)\)=([0-9.]+)`)

func scoreTermContributions(queryText string, raw *ScoreExplanation) []TermContribution {
	if raw == nil {
		return nil
	}
	needles := queryNeedles(parseQuery(queryText))
	if len(needles) == 0 {
		return nil
	}
	byTerm := map[string]*TermContribution{}
	var walk func(*ScoreExplanation)
	walk = func(node *ScoreExplanation) {
		if node == nil {
			return
		}
		message := strings.TrimSpace(node.Message)
		if strings.HasPrefix(message, "weight(") || strings.HasPrefix(message, "fieldWeight(") {
			field, term, ok := explanationFieldTerm(message)
			if ok && matchTermField(field) && termRelevantToQuery(term, needles) {
				contribution := explanationNodeContribution(node, field, term)
				mergeTermContribution(byTerm, contribution)
				if strings.HasPrefix(message, "weight(") {
					return
				}
			}
		}
		for _, child := range node.Children {
			walk(child)
		}
	}
	walk(raw)
	if len(byTerm) == 0 {
		return nil
	}
	out := make([]TermContribution, 0, len(byTerm))
	for _, contribution := range byTerm {
		out = append(out, *contribution)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score == out[j].Score {
			return out[i].QueryTerm < out[j].QueryTerm
		}
		return out[i].Score > out[j].Score
	})
	if len(out) > 12 {
		out = out[:12]
	}
	return out
}

func explanationFieldTerm(message string) (string, string, bool) {
	matches := explanationFieldTermRE.FindStringSubmatch(message)
	if len(matches) < 3 {
		matches = explanationTermFreqRE.FindStringSubmatch(message)
	}
	if len(matches) < 3 {
		return "", "", false
	}
	field := strings.TrimSpace(matches[1])
	term := strings.TrimSpace(strings.ToLower(matches[2]))
	if i := strings.Index(term, "^"); i >= 0 {
		term = term[:i]
	}
	term = strings.Trim(term, `"`)
	if field == "" || term == "" {
		return "", "", false
	}
	return field, term, true
}

func explanationNodeContribution(node *ScoreExplanation, field, term string) TermContribution {
	out := TermContribution{Field: field, Term: term, QueryTerm: field + ":" + term, Score: node.Value}
	var scan func(*ScoreExplanation)
	scan = func(item *ScoreExplanation) {
		if item == nil {
			return
		}
		message := strings.TrimSpace(item.Message)
		switch {
		case strings.HasPrefix(message, "tf("):
			out.TermFrequency = maxFloat(out.TermFrequency, item.Value)
		case strings.HasPrefix(message, "fieldNorm("):
			out.FieldNorm = maxFloat(out.FieldNorm, item.Value)
		case strings.HasPrefix(message, "idf("):
			out.IDF = maxFloat(out.IDF, item.Value)
		case message == "boost":
			out.Boost = maxFloat(out.Boost, item.Value)
		case message == "queryNorm":
			out.QueryNorm = maxFloat(out.QueryNorm, item.Value)
		case strings.HasPrefix(message, "queryWeight("):
			out.QueryWeight = maxFloat(out.QueryWeight, item.Value)
		}
		for _, child := range item.Children {
			scan(child)
		}
	}
	for _, child := range node.Children {
		scan(child)
	}
	return out
}

func mergeTermContribution(byTerm map[string]*TermContribution, next TermContribution) {
	if next.QueryTerm == "" {
		return
	}
	current := byTerm[next.QueryTerm]
	if current == nil {
		copy := next
		byTerm[next.QueryTerm] = &copy
		return
	}
	current.Score += next.Score
	current.TermFrequency = maxFloat(current.TermFrequency, next.TermFrequency)
	current.FieldNorm = maxFloat(current.FieldNorm, next.FieldNorm)
	current.IDF = maxFloat(current.IDF, next.IDF)
	current.QueryWeight = maxFloat(current.QueryWeight, next.QueryWeight)
	current.Boost = maxFloat(current.Boost, next.Boost)
	current.QueryNorm = maxFloat(current.QueryNorm, next.QueryNorm)
}

func maxFloat(a, b float64) float64 {
	if b > a {
		return b
	}
	return a
}

type queryNeedle struct {
	Term        string
	MaxDistance int
}

// queryNeedles records the literal words/compounds the user typed so fuzzy Bleve
// matches can still be judged against the intended query before highlighting.
func queryNeedles(parsed parsedQuery) []queryNeedle {
	seen := map[string]bool{}
	var needles []queryNeedle
	add := func(value string) {
		value = strings.Trim(strings.TrimSpace(strings.ToLower(value)), `"`)
		if value == "" {
			return
		}
		addNeedle := func(term string) {
			term = strings.TrimSpace(strings.ToLower(term))
			if term == "" || seen[term] {
				return
			}
			seen[term] = true
			maxDistance := 0
			if len(term) >= 5 {
				maxDistance = fuzzinessFor(term, SearchBehavior{}.normalized())
			}
			needles = append(needles, queryNeedle{Term: term, MaxDistance: maxDistance})
		}
		addNeedle(value)
		normalized := normalizeSearchText(value)
		for _, term := range strings.Fields(normalized) {
			addNeedle(term)
		}
		if joined := strings.ReplaceAll(normalized, " ", ""); joined != "" {
			addNeedle(joined)
		}
	}
	for _, value := range []string{parsed.Text, parsed.Filename, parsed.From, parsed.To, parsed.CC, parsed.Subject} {
		add(value)
	}
	return needles
}

func termRelevantToQuery(term string, needles []queryNeedle) bool {
	if len(needles) == 0 {
		return false
	}
	for _, needle := range needles {
		if term == needle.Term {
			return true
		}
		if len(term) >= minSplitFragmentLength && len(needle.Term) >= minSplitFragmentLength && (strings.Contains(term, needle.Term) || strings.Contains(needle.Term, term)) {
			return true
		}
		if needle.MaxDistance > 0 && boundedEditDistance(term, needle.Term, needle.MaxDistance) <= needle.MaxDistance {
			return true
		}
	}
	return false
}

func boundedEditDistance(a, b string, max int) int {
	if max < 0 {
		return max + 1
	}
	ar := []rune(a)
	br := []rune(b)
	if len(ar)-len(br) > max || len(br)-len(ar) > max {
		return max + 1
	}
	prev := make([]int, len(br)+1)
	curr := make([]int, len(br)+1)
	for j := range prev {
		prev[j] = j
	}
	for i, ra := range ar {
		curr[0] = i + 1
		rowMin := curr[0]
		for j, rb := range br {
			cost := 1
			if ra == rb {
				cost = 0
			}
			curr[j+1] = minInt(curr[j]+1, prev[j+1]+1, prev[j]+cost)
			if curr[j+1] < rowMin {
				rowMin = curr[j+1]
			}
		}
		if rowMin > max {
			return max + 1
		}
		prev, curr = curr, prev
	}
	return prev[len(br)]
}

func minInt(values ...int) int {
	out := values[0]
	for _, value := range values[1:] {
		if value < out {
			out = value
		}
	}
	return out
}

func domainTextQuery(text string) blevequery.Query {
	terms := domainQueryTerms(text)
	if len(terms) < 2 {
		return nil
	}
	return bleve.NewDisjunctionQuery(
		boostQuery(termConjunction("", terms), 1),
		boostQuery(termConjunction("from_domain", terms), 15),
	)
}

func domainQueryTerms(text string) []string {
	text = strings.Trim(strings.TrimSpace(text), `"`)
	text = strings.TrimPrefix(text, "@")
	if at := strings.LastIndex(text, "@"); at >= 0 {
		text = text[at+1:]
	}
	if strings.ContainsAny(text, " \t\r\n") || !strings.Contains(text, ".") {
		return nil
	}
	return strings.Fields(normalizeSearchText(text))
}

func termConjunction(field string, terms []string) blevequery.Query {
	parts := make([]blevequery.Query, 0, len(terms))
	for _, term := range terms {
		q := bleve.NewMatchQuery(term)
		if field != "" {
			q.SetField(field)
		}
		parts = append(parts, q)
	}
	return bleve.NewConjunctionQuery(parts...)
}

func exactCompactQueries(joined string, behavior normalizedSearchBehavior) []blevequery.Query {
	if joined == "" {
		return nil
	}
	out := []blevequery.Query{
		boostedMatch("subject_compound", joined, 80),
		boostedMatch("from_domain", joined, 70),
		boostedMatch("from", joined, 60),
		boostedMatch("from_compound", joined, 50),
	}
	if behavior.attachmentBoostScale() > 0 {
		out = append(out, boostedMatch("compound", joined, 45*behavior.attachmentBoostScale()))
	}
	return out
}

func splitPhraseQueries(term string, behavior normalizedSearchBehavior) []blevequery.Query {
	if strings.Contains(term, " ") || !canSplitCompactTerm(term) || len(term) > 40 {
		return nil
	}
	attachmentScale := behavior.attachmentBoostScale()
	var out []blevequery.Query
	for split := minSplitFragmentLength; split <= len(term)-minSplitFragmentLength; split++ {
		phrase := term[:split] + " " + term[split:]
		out = append(out, boostedPhrase("subject", phrase, 35))
		out = append(out, boostedPhrase("body", phrase, 42))
		if attachmentScale > 0 {
			out = append(out, boostedPhrase("attachments", phrase, 24*attachmentScale))
		}
		out = append(out, boostedPhrase("from", phrase, 20))
	}
	return out
}

func splitWordQueries(term string, behavior normalizedSearchBehavior) []blevequery.Query {
	if strings.Contains(term, " ") || !canSplitCompactTerm(term) || len(term) > 40 {
		return nil
	}
	var out []blevequery.Query
	for split := minSplitFragmentLength; split <= len(term)-minSplitFragmentLength; split++ {
		out = append(out, boostQuery(bleve.NewConjunctionQuery(splitTermQuery(term[:split], behavior), splitTermQuery(term[split:], behavior)), 0.2))
	}
	return out
}

func splitTermQuery(term string, behavior normalizedSearchBehavior) blevequery.Query {
	queries := []blevequery.Query{baseTextTermQuery(term, behavior)}
	if behavior.fuzzyEnabled() && len(term) >= 4 {
		queries = append(queries, fuzzyTextQuery(term, 0.5, behavior))
	}
	return bleve.NewDisjunctionQuery(queries...)
}

func canSplitCompactTerm(term string) bool {
	return len(term) >= minSplitFragmentLength*2
}

func boostedMatch(field, text string, boost float64) blevequery.Query {
	q := bleve.NewMatchQuery(text)
	q.SetField(field)
	q.SetBoost(boost)
	return q
}

func boostedPhrase(field, text string, boost float64) blevequery.Query {
	q := bleve.NewMatchPhraseQuery(text)
	q.SetField(field)
	q.SetBoost(boost)
	return q
}

func boostQuery(q blevequery.Query, boost float64) blevequery.Query {
	if q == nil {
		return nil
	}
	if bq, ok := q.(interface{ SetBoost(float64) }); ok {
		bq.SetBoost(boost)
	}
	return q
}

func fuzzinessFor(term string, behavior normalizedSearchBehavior) int {
	if behavior.Fuzzy == "forgiving" && len([]rune(term)) >= 7 {
		return 2
	}
	return 1
}

func normalizeSearchText(text string) string {
	var b strings.Builder
	var lastSpace bool
	for _, r := range strings.ToLower(text) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			lastSpace = false
			continue
		}
		if !lastSpace {
			b.WriteByte(' ')
			lastSpace = true
		}
	}
	return strings.TrimSpace(b.String())
}

func compoundSearchText(values ...string) string {
	normalized := normalizeSearchText(strings.Join(values, " "))
	words := strings.Fields(normalized)
	if len(words) < 2 {
		return ""
	}
	var b strings.Builder
	appendTerm := func(term string) bool {
		if term == "" {
			return true
		}
		if b.Len()+len(term)+1 > maxCompoundFieldBytes {
			return false
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(term)
		return true
	}
	for i := 0; i < len(words)-1; i++ {
		if len(words[i]) >= minSplitFragmentLength && len(words[i+1]) >= minSplitFragmentLength {
			if !appendTerm(words[i] + words[i+1]) {
				return b.String()
			}
		}
		if i+2 < len(words) {
			if len(words[i]) >= minSplitFragmentLength && len(words[i+1]) >= minSplitFragmentLength && len(words[i+2]) >= minSplitFragmentLength {
				if !appendTerm(words[i] + words[i+1] + words[i+2]) {
					return b.String()
				}
			}
		}
	}
	return b.String()
}

var emailAddressRE = regexp.MustCompile(`(?i)([a-z0-9._%+\-]+)@([a-z0-9.\-]+\.[a-z0-9\-]+)`)
var emailDomainRE = regexp.MustCompile(`(?i)[a-z0-9._%+\-]+@([a-z0-9.\-]+\.[a-z0-9\-]+)`)

func emailAddressQuery(value string) blevequery.Query {
	value = strings.Trim(strings.TrimSpace(value), `"`)
	match := emailAddressRE.FindStringSubmatch(value)
	if len(match) != 3 {
		return nil
	}
	localTerms := strings.Fields(normalizeSearchText(match[1]))
	domainTerms := domainQueryTerms(match[2])
	if len(localTerms) == 0 || len(domainTerms) == 0 {
		return nil
	}
	localQuery := termConjunction("from", localTerms)
	if len(localTerms) > 1 {
		localQuery = bleve.NewDisjunctionQuery(localQuery, termConjunction("from_compound", compactAdjacentTerms(localTerms)))
	}
	return bleve.NewConjunctionQuery(localQuery, termConjunction("from_domain", domainTerms))
}

func emailDomainTerms(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return ""
	}
	seen := map[string]bool{}
	var out []string
	add := func(term string) {
		term = strings.Trim(strings.TrimSpace(term), "<>.,;:\"'()[]")
		if term == "" || seen[term] {
			return
		}
		seen[term] = true
		out = append(out, term)
	}
	matches := emailDomainRE.FindAllStringSubmatch(value, -1)
	for _, m := range matches {
		addDomainTerms(add, m[1])
	}
	if len(matches) == 0 {
		addDomainTerms(add, strings.TrimPrefix(value, "@"))
	}
	return strings.Join(out, " ")
}

func compactAdjacentTerms(words []string) []string {
	out := make([]string, 0, len(words)*2)
	for i := 0; i < len(words)-1; i++ {
		out = append(out, words[i]+words[i+1])
		if i+2 < len(words) {
			out = append(out, words[i]+words[i+1]+words[i+2])
		}
	}
	return out
}

func addDomainTerms(add func(string), domain string) {
	domain = strings.Trim(strings.ToLower(domain), "<>.,;:\"'()[]")
	if domain == "" {
		return
	}
	add(domain)
	normalized := normalizeSearchText(domain)
	add(normalized)
	add(strings.ReplaceAll(normalized, " ", ""))
	parts := strings.Split(domain, ".")
	for i := 0; i < len(parts); i++ {
		add(strings.Join(parts[i:], "."))
	}
}

type parsedQuery struct {
	Text          string
	TextQuoted    bool
	NegatedText   []negatedTextTerm
	HasAttachment *bool
	IsRead        *bool
	IsStarred     *bool
	IsEncrypted   *bool
	IsSigned      *bool
	Language      string
	Filename      string
	From          string
	To            string
	CC            string
	Subject       string
	After         time.Time
	Before        time.Time
}

type negatedTextTerm struct {
	Text   string
	Quoted bool
}

var operatorRE = regexp.MustCompile(`(?i)(^|\s)(-?)(has:attachment|is:read|is:unread|is:starred|is:notstarred|is:encrypted|is:unencrypted|is:signed|is:unsigned|lang:("[^"]+"|\S+)|filename:("[^"]+"|\S+)|from:("[^"]+"|\S+)|to:("[^"]+"|\S+)|cc:("[^"]+"|\S+)|subject:("[^"]+"|\S+)|after:("[^"]+"|\S+)|before:("[^"]+"|\S+)|older_than:("[^"]+"|\S+)|newer_than:("[^"]+"|\S+)|year:("[^"]+"|\S+))`)

// parseQuery extracts supported operators while preserving the remaining free text.
// The parser is intentionally small and predictable rather than a full Gmail clone.
func parseQuery(queryText string) parsedQuery {
	text, quoted, negated := parseFreeText(queryText)
	out := parsedQuery{Text: text, TextQuoted: quoted, NegatedText: negated}
	matches := operatorRE.FindAllStringSubmatchIndex(queryText, -1)
	if len(matches) == 0 {
		return out
	}
	var cleaned strings.Builder
	last := 0
	for _, m := range matches {
		start, end := m[0], m[1]
		if start > last {
			cleaned.WriteString(queryText[last:start])
		}
		token := strings.TrimSpace(queryText[start:end])
		negated := strings.HasPrefix(token, "-")
		token = strings.TrimPrefix(token, "-")
		lower := strings.ToLower(token)
		switch {
		case lower == "has:attachment":
			v := !negated
			out.HasAttachment = &v
		case lower == "is:read":
			v := !negated
			out.IsRead = &v
		case lower == "is:unread":
			v := negated
			out.IsRead = &v
		case lower == "is:starred":
			v := !negated
			out.IsStarred = &v
		case lower == "is:notstarred":
			v := negated
			out.IsStarred = &v
		case lower == "is:encrypted":
			v := !negated
			out.IsEncrypted = &v
		case lower == "is:unencrypted":
			v := negated
			out.IsEncrypted = &v
		case lower == "is:signed":
			v := !negated
			out.IsSigned = &v
		case lower == "is:unsigned":
			v := negated
			out.IsSigned = &v
		case strings.HasPrefix(lower, "lang:"):
			out.Language = normalizeLanguageCode(strings.Trim(operatorValue(token), `"`))
		case strings.HasPrefix(lower, "filename:"):
			out.Filename = strings.Trim(operatorValue(token), `"`)
		case strings.HasPrefix(lower, "from:"):
			out.From = strings.Trim(operatorValue(token), `"`)
		case strings.HasPrefix(lower, "to:"):
			out.To = strings.Trim(operatorValue(token), `"`)
		case strings.HasPrefix(lower, "cc:"):
			out.CC = strings.Trim(operatorValue(token), `"`)
		case strings.HasPrefix(lower, "subject:"):
			out.Subject = strings.Trim(operatorValue(token), `"`)
		case strings.HasPrefix(lower, "after:"):
			out.After = parseSearchDate(operatorValue(token), false)
		case strings.HasPrefix(lower, "before:"):
			out.Before = parseSearchDate(operatorValue(token), false)
		case strings.HasPrefix(lower, "older_than:"):
			out.Before = parseRelativeSearchDate(operatorValue(token))
		case strings.HasPrefix(lower, "newer_than:"):
			out.After = parseRelativeSearchDate(operatorValue(token))
		case strings.HasPrefix(lower, "year:"):
			if start, end := parseSearchYear(operatorValue(token)); !start.IsZero() {
				out.After = start
				out.Before = end
			}
		}
		last = end
	}
	if last < len(queryText) {
		cleaned.WriteString(queryText[last:])
	}
	out.Text, out.TextQuoted, out.NegatedText = parseFreeText(cleaned.String())
	return out
}

func parseFreeText(value string) (string, bool, []negatedTextTerm) {
	tokens := scanFreeTextTokens(value)
	positive := make([]string, 0, len(tokens))
	var negated []negatedTextTerm
	for _, token := range tokens {
		if token.Negated {
			text, quoted := parseQueryText(token.Text)
			if text != "" {
				negated = append(negated, negatedTextTerm{Text: text, Quoted: quoted})
			}
			continue
		}
		positive = append(positive, token.Text)
	}
	text, quoted := parseQueryText(strings.Join(positive, " "))
	return text, quoted, negated
}

type freeTextToken struct {
	Text    string
	Negated bool
}

func scanFreeTextTokens(value string) []freeTextToken {
	var tokens []freeTextToken
	for i := 0; i < len(value); {
		r, size := utf8.DecodeRuneInString(value[i:])
		if unicode.IsSpace(r) {
			i += size
			continue
		}
		negated := false
		if r == '-' {
			next, _ := utf8.DecodeRuneInString(value[i+size:])
			if next != utf8.RuneError && !unicode.IsSpace(next) {
				negated = true
				i += size
			}
		}
		var b strings.Builder
		inQuote := false
		for i < len(value) {
			r, size = utf8.DecodeRuneInString(value[i:])
			if r == '"' {
				inQuote = !inQuote
				b.WriteRune(r)
				i += size
				continue
			}
			if unicode.IsSpace(r) && !inQuote {
				break
			}
			b.WriteRune(r)
			i += size
		}
		if text := strings.TrimSpace(b.String()); text != "" {
			tokens = append(tokens, freeTextToken{Text: text, Negated: negated})
		}
	}
	return tokens
}

func parseQueryText(value string) (string, bool) {
	text := strings.TrimSpace(value)
	if len(text) >= 2 && text[0] == '"' && text[len(text)-1] == '"' {
		return strings.TrimSpace(text[1 : len(text)-1]), true
	}
	return text, false
}

func parseSearchDate(value string, endOfDay bool) time.Time {
	value = strings.Trim(strings.TrimSpace(value), `"`)
	if value == "" {
		return time.Time{}
	}
	now := time.Now().UTC()
	switch strings.ToLower(value) {
	case "today":
		return dayBoundary(now, endOfDay)
	case "yesterday":
		return dayBoundary(now.AddDate(0, 0, -1), endOfDay)
	}
	for _, layout := range []string{"2006/01/02", "2006-01-02", "01/02/2006"} {
		if t, err := time.ParseInLocation(layout, value, time.UTC); err == nil {
			return dayBoundary(t, endOfDay)
		}
	}
	return time.Time{}
}

func parseRelativeSearchDate(value string) time.Time {
	duration, ok := parseRelativeSearchDuration(value)
	if !ok {
		return time.Time{}
	}
	return time.Now().UTC().Add(-duration)
}

func parseRelativeSearchDuration(value string) (time.Duration, bool) {
	value = strings.ToLower(strings.Trim(strings.TrimSpace(value), `"`))
	if value == "" {
		return 0, false
	}
	unit := value[len(value)-1]
	n, err := strconv.Atoi(value[:len(value)-1])
	if err != nil || n <= 0 {
		return 0, false
	}
	switch unit {
	case 'd':
		return time.Duration(n) * 24 * time.Hour, true
	case 'w':
		return time.Duration(n) * 7 * 24 * time.Hour, true
	case 'm':
		return time.Duration(n) * 30 * 24 * time.Hour, true
	case 'y':
		return time.Duration(n) * 365 * 24 * time.Hour, true
	}
	return 0, false
}

func parseSearchYear(value string) (time.Time, time.Time) {
	value = strings.Trim(strings.TrimSpace(value), `"`)
	if len(value) != 4 {
		return time.Time{}, time.Time{}
	}
	year, err := strconv.Atoi(value)
	if err != nil || year < 1 || year > 9999 {
		return time.Time{}, time.Time{}
	}
	start := time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC)
	return start, start.AddDate(1, 0, 0)
}

func dayBoundary(t time.Time, endOfDay bool) time.Time {
	y, m, d := t.Date()
	start := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	if !endOfDay {
		return start
	}
	return start.Add(24 * time.Hour)
}

func operatorValue(token string) string {
	if idx := strings.Index(token, ":"); idx >= 0 && idx+1 < len(token) {
		return token[idx+1:]
	}
	return ""
}
