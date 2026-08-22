// File overview: Password reset request and completion handlers.

package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"rolltop/backend/auth"
	mmcrypto "rolltop/backend/crypto"
	"rolltop/backend/smtpclient"
	"rolltop/backend/store"
)

const passwordResetFromAddressSetting = "password_reset_from_address"

func (s *Server) apiPasswordResetRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if !s.verifyCSRF(w, r) {
		return
	}
	var in struct {
		Email string `json:"email"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	user, err := s.store.GetUserByEmail(r.Context(), in.Email)
	if err == nil && strings.TrimSpace(user.BackupEmail) != "" {
		if token, tokenErr := auth.NewOpaqueToken(); tokenErr == nil {
			tokenHash := mmcrypto.TokenHash(token)
			expires := time.Now().Add(45 * time.Minute)
			if createErr := s.store.CreatePasswordResetToken(r.Context(), user.ID, tokenHash, expires); createErr == nil {
				link := s.passwordResetLink(r, token)
				_ = s.sendPasswordResetEmail(r.Context(), user, link, expires)
			}
		}
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) apiPasswordResetComplete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if !s.verifyCSRF(w, r) {
		return
	}
	var in struct {
		Token    string `json:"token"`
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if len(in.Password) < 12 {
		writeAPIError(w, http.StatusBadRequest, "Password must be at least 12 characters.")
		return
	}
	hash, err := auth.HashPassword(in.Password)
	if err != nil {
		s.serverError(w, err)
		return
	}
	user, err := s.store.UsePasswordResetToken(r.Context(), mmcrypto.TokenHash(strings.TrimSpace(in.Token)), hash)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "Password reset link is invalid or expired.")
		return
	}
	if err := s.loginUser(w, r, user.ID); err != nil {
		s.serverError(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// passwordResetLink builds the absolute link emailed to the user's backup
// address. When a canonical public base URL is configured it is always used;
// the incoming request Host is only trusted when no canonical origin exists,
// because proxies that forward arbitrary Host headers would otherwise let an
// attacker point the emailed link at their own machine.
func (s *Server) passwordResetLink(r *http.Request, token string) string {
	if base := strings.TrimSpace(s.publicBaseURL); base != "" {
		if u, err := url.Parse(base); err == nil && u.Scheme != "" && u.Host != "" {
			u.Path = "/reset-password"
			u.RawQuery = ""
			u.Fragment = ""
			q := u.Query()
			q.Set("token", token)
			u.RawQuery = q.Encode()
			return u.String()
		}
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	u := url.URL{Scheme: scheme, Host: r.Host, Path: "/reset-password"}
	q := u.Query()
	q.Set("token", token)
	u.RawQuery = q.Encode()
	return u.String()
}

func (s *Server) sendPasswordResetEmail(ctx context.Context, user store.User, link string, expires time.Time) error {
	if s.sender == nil {
		return fmt.Errorf("password reset SMTP sender is not configured")
	}
	from, _ := s.store.GetSystemSetting(ctx, passwordResetFromAddressSetting)
	identity, smtpAccount, err := s.passwordResetSender(ctx, from)
	if err != nil {
		return err
	}
	msg := smtpclient.Message{
		From:      identity.Header,
		To:        []string{user.BackupEmail},
		Subject:   "Reset your rolltop password",
		BodyText:  passwordResetBody(user, link, expires),
		MessageID: smtpclient.NewMessageID(identity.Email),
		Date:      time.Now(),
	}
	_, err = s.sender.Send(ctx, smtpEnvelopeForIdentity(identity, smtpAccount), msg)
	return err
}

func passwordResetBody(user store.User, link string, expires time.Time) string {
	return fmt.Sprintf("A password reset was requested for %s.\n\nUse this link to choose a new rolltop password:\n%s\n\nThis link expires at %s. If you did not request this, ignore this email.", user.Email, link, expires.Format(time.RFC1123Z))
}

func (s *Server) passwordResetSender(ctx context.Context, from string) (composeIdentity, store.SMTPAccount, error) {
	from = store.NormalizeContactEmail(from)
	users, err := s.store.ListUsers(ctx)
	if err != nil {
		return composeIdentity{}, store.SMTPAccount{}, err
	}
	if from != "" {
		for _, user := range users {
			if store.NormalizeContactEmail(user.Email) != from {
				if !s.userHasIdentityEmail(ctx, user, from) {
					continue
				}
			}
			identity, account, err := s.passwordResetSenderForUser(ctx, user, from)
			if err == nil {
				return identity, account, nil
			}
		}
		return composeIdentity{}, store.SMTPAccount{}, fmt.Errorf("no SMTP identity is configured for password reset From address")
	}
	for _, user := range users {
		if !user.IsAdmin {
			continue
		}
		identity, account, err := s.passwordResetSenderForUser(ctx, user, store.NormalizeContactEmail(user.Email))
		if err == nil {
			return identity, account, nil
		}
	}
	for _, user := range users {
		if !user.IsAdmin {
			continue
		}
		identity, account, err := s.passwordResetSenderForUser(ctx, user, "")
		if err == nil {
			return identity, account, nil
		}
	}
	return composeIdentity{}, store.SMTPAccount{}, fmt.Errorf("no admin SMTP identity is configured for password resets")
}

func (s *Server) userHasIdentityEmail(ctx context.Context, user store.User, email string) bool {
	for _, identity := range s.composeIdentityChoices(ctx, currentUser{User: user}) {
		if store.NormalizeContactEmail(identity.Email) == email {
			return true
		}
	}
	return false
}

func (s *Server) passwordResetSenderForUser(ctx context.Context, user store.User, from string) (composeIdentity, store.SMTPAccount, error) {
	for _, identity := range s.composeIdentityChoices(ctx, currentUser{User: user}) {
		if from != "" && store.NormalizeContactEmail(identity.Email) != from {
			continue
		}
		account, err := s.smtpAccountForIdentity(ctx, user.ID, identity)
		if err == nil {
			return identity, account, nil
		}
	}
	return composeIdentity{}, store.SMTPAccount{}, store.ErrNotFound
}
