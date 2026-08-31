// File overview: Remote email image cache integration for message rendering.

package web

import (
	"context"
	"net/http"
	"strings"
	"time"

	"rolltop/backend/plugins"
	"rolltop/backend/remoteimages"
	"rolltop/backend/store"
)

func (s *Server) cachedRemoteImageURLs(ctx context.Context, userID int64, msg store.MessageRecord, bodyHTML string) map[string]string {
	if s == nil || s.store == nil || s.blobs == nil || strings.TrimSpace(bodyHTML) == "" {
		return nil
	}
	candidates := remoteimages.Extract(bodyHTML)
	if len(candidates) == 0 {
		return nil
	}
	out := map[string]string{}
	var missing []remoteimages.Candidate
	for _, candidate := range candidates {
		hash := remoteimages.Hash(candidate.URL)
		cache, err := s.store.GetRemoteImageCacheByHash(ctx, userID, hash)
		if err == nil && cache.Status == store.RemoteImageStatusOK && strings.TrimSpace(cache.BlobPath) != "" {
			out[hash] = remoteimages.CachedURL(hash)
			continue
		}
		if err != nil || !store.RemoteImageCacheFresh(cache, time.Now().Unix()) {
			missing = append(missing, candidate)
		}
	}
	if len(missing) > 0 {
		s.remoteImageCache().WarmMessageAsync(plugins.RemoteImageFetchRequest{
			UserID:    userID,
			MessageID: msg.MessageIDHeader,
			Sender:    msg.FromAddr,
		}, bodyHTML)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (s *Server) remoteImageCache() remoteimages.Cache {
	return remoteimages.Cache{
		Store: s.store,
		Blobs: s.blobs,
		Allow: func(ctx context.Context, req plugins.RemoteImageFetchRequest) (plugins.RemoteImageFetchDecision, error) {
			return s.allowRemoteImageFetch(ctx, req)
		},
	}
}

func (s *Server) allowRemoteImageFetch(ctx context.Context, req plugins.RemoteImageFetchRequest) (plugins.RemoteImageFetchDecision, error) {
	if s == nil || s.store == nil || !s.pluginEnabled(ctx, plugins.RemoteImageBlocklist) {
		return plugins.RemoteImageFetchDecision{Allow: true}, nil
	}
	hook, ok := remoteImageBlocklistHook()
	if !ok {
		return plugins.RemoteImageFetchDecision{Allow: true}, nil
	}
	return hook.AllowRemoteImageFetch(ctx, s.store.DB(), req)
}

func (s *Server) handleRemoteImage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	cu, ok := s.requireAuth(w, r)
	if !ok {
		return
	}
	hash := strings.Trim(strings.TrimPrefix(r.URL.Path, "/remote-images/"), "/")
	if hash == "" {
		http.NotFound(w, r)
		return
	}
	cache, err := s.store.GetRemoteImageCacheByHash(r.Context(), cu.User.ID, hash)
	if err != nil {
		// The lookup error has to be classified before the cache fields are
		// read: on a store failure the zero-valued cache also fails the checks
		// below, which turned every such failure into a silent 404.
		if store.IsNotFound(err) {
			http.NotFound(w, r)
			return
		}
		s.serverError(w, r, err)
		return
	}
	if cache.Status != store.RemoteImageStatusOK || strings.TrimSpace(cache.BlobPath) == "" {
		http.NotFound(w, r)
		return
	}
	file, err := s.blobs.OpenUserBlob(cu.User.ID, cache.BlobPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer file.Close()
	contentType := strings.TrimSpace(cache.ContentType)
	if contentType == "" {
		contentType = "image/*"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Header().Set("Vary", "Cookie")
	http.ServeContent(w, r, hash, cache.FetchedAt, file)
}
