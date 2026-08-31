// File overview: Mailbox listing, search, pagination, and message flag API handlers.

package web

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"rolltop/backend/plugins"
	"rolltop/backend/store"
)

// apiMail returns a paged conversation list for All Mail or one mailbox. It asks
// SQLite for extra rows because conversation grouping can collapse several message
// rows into one visible thread.
func (s *Server) apiMail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	cu, ok := s.requireAPIAuth(w, r)
	if !ok {
		return
	}
	timing := newSearchTiming()
	page := pageFromRequest(r)
	var mailboxID int64
	if raw := strings.TrimSpace(r.URL.Query().Get("mailbox")); raw != "" {
		id, parseErr := strconv.ParseInt(raw, 10, 64)
		if parseErr != nil || id <= 0 {
			http.NotFound(w, r)
			return
		}
		mailboxID = id
	}
	cacheKey := mailListCacheKey{UserID: cu.User.ID, MailboxID: mailboxID, Page: page}
	if mailboxID == 0 && page == 1 && s.writeMailListPageIfFresh(w, r, cacheKey) {
		return
	}
	if s.writeMailListNotModifiedIfFresh(w, r, cacheKey) {
		return
	}
	generation := s.mailListGeneration(cu.User.ID)
	response, err := s.mailPageResponse(r.Context(), cu.User, mailboxID, page, timing)
	if err != nil {
		if store.IsNotFound(err) {
			http.NotFound(w, r)
			return
		}
		s.serverError(w, r, err)
		return
	}
	writeMailTimingHeaders(w, timing, page)
	etag, ok := writeJSONCachedWithETag(w, r, response)
	if ok {
		s.rememberMailListETag(cacheKey, etag, generation)
	}
}

func (s *Server) mailPageResponse(ctx context.Context, user store.User, mailboxID int64, page int, timing *searchTiming) (map[string]any, error) {
	const pageSize = 50
	offset := (page - 1) * pageSize
	fetchLimit := pageSize*3 + 1
	var activeMailbox *apiMailbox
	var messages []store.MessageRecord
	var err error
	if mailboxID != 0 {
		mb, mbErr := s.store.GetMailboxForUser(ctx, user.ID, mailboxID)
		if mbErr != nil {
			return nil, mbErr
		}
		active := apiMailboxFromStore(mb)
		activeMailbox = &active
		hydrateDone := timing.measure(&timing.hydrate)
		messages, err = s.store.ListLatestThreadMessagesForMailbox(ctx, user.ID, mb.ID, fetchLimit, offset)
		hydrateDone()
	} else {
		hydrateDone := timing.measure(&timing.hydrate)
		messages, err = s.store.ListLatestThreadMessagesForUser(ctx, user.ID, fetchLimit, offset)
		hydrateDone()
	}
	if err != nil {
		return nil, err
	}
	timing.seeds = len(messages)
	own := s.ownAddresses(ctx, user)
	renderDone := timing.measure(&timing.render)
	conversations, err := s.conversationViews(ctx, user.ID, messages, own)
	renderDone()
	if err != nil {
		return nil, err
	}
	hasNext := len(conversations) > pageSize
	if hasNext {
		conversations = conversations[:pageSize]
	}
	return map[string]any{
		"conversations":  s.apiConversationsWithAnnotations(ctx, user.ID, conversations),
		"page":           page,
		"has_prev":       page > 1,
		"has_next":       hasNext,
		"active_mailbox": activeMailbox,
	}, nil
}

// apiSearch combines URL query parsing, optional mailbox filtering, sender-history
// boosts, Bleve search hits, and SQLite conversation hydration into the search
// result payload consumed by SearchView.
func (s *Server) apiSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	cu, ok := s.requireAPIAuth(w, r)
	if !ok {
		return
	}
	const pageSize = 50
	timing := newSearchTiming()
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	filterDone := timing.measure(&timing.filter)
	searchQuery, mailboxFilter, err := s.searchMailboxFilter(r.Context(), cu.User.ID, q)
	filterDone()
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if searchQuery != "" && strings.Contains(strings.ToLower(searchQuery), "lang:") && !s.pluginEnabled(r.Context(), plugins.LanguageSearch) {
		writeAPIError(w, http.StatusBadRequest, "Language search is disabled.")
		return
	}
	if strings.TrimSpace(searchQuery) != "" {
		if _, err := s.ensureRecentSearchDocuments(r.Context(), cu.User.ID); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	page := pageFromRequest(r)
	cacheKey := mailListCacheKey{UserID: cu.User.ID, Page: page, Search: true, Query: q}
	if s.writeSearchNotModifiedIfFresh(w, r, cacheKey) {
		return
	}
	generation := s.mailListGeneration(cu.User.ID)
	offset := (page - 1) * pageSize
	own := s.ownAddresses(r.Context(), cu.User)
	var seeds []conversationSeed
	if searchQuery == "" && !mailboxFilter.enabled {
		var messages []store.MessageRecord
		hydrateDone := timing.measure(&timing.hydrate)
		messages, err = s.store.ListLatestThreadMessagesForUser(r.Context(), cu.User.ID, pageSize*3+1, offset)
		hydrateDone()
		seeds = conversationSeedsFromMessages(messages)
	} else {
		boostDone := timing.measure(&timing.sender)
		opts := s.searchOptionsWithRankingBoosts(r.Context(), cu.User)
		boostDone()
		seeds, err = s.searchConversationSeedHits(r.Context(), cu.User.ID, searchQuery, page, pageSize, opts, own, mailboxFilter, timing)
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	timing.seeds = len(seeds)
	renderDone := timing.measure(&timing.render)
	conversations, err := s.conversationViewsWithSearchDetails(r.Context(), cu.User.ID, seeds, own, searchQuery)
	renderDone()
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	hasNext := len(conversations) > pageSize
	if hasNext {
		conversations = conversations[:pageSize]
	}
	writeSearchTimingHeaders(w, timing, page)
	etag, ok := writeJSONCachedWithETag(w, r, map[string]any{
		"conversations": s.apiConversationsWithAnnotations(r.Context(), cu.User.ID, conversations),
		"page":          page,
		"has_prev":      page > 1,
		"has_next":      hasNext,
		"query":         q,
	})
	if ok {
		s.rememberMailListETag(cacheKey, etag, generation)
	}
}
