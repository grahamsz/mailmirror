// File overview: Runtime backend plugin that adds OpenID Connect sign-in.

package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/mail"
	"net/url"
	"os"
	"strings"
	"time"

	"rolltop/backend/auth"
	"rolltop/backend/plugins"
	"rolltop/backend/store"
)

const (
	pluginID    = "oidc"
	apiPath     = "plugins/oidc"
	stateCookie = "rt_oidc_state"
)

type oidcPlugin struct {
	routes []plugins.ProtectedAPIRouteHandle
}

func RolltopPlugin() plugins.BackendPlugin {
	return &oidcPlugin{}
}

func (p *oidcPlugin) ID() string { return pluginID }

func (p *oidcPlugin) Start(host plugins.BackendStartHost) error {
	p.unregister()
	login, err := host.RegisterPublicAPI(pluginID, plugins.PublicAPIRoute{Path: apiPath + "/login", Handle: p.login})
	if err != nil {
		return err
	}
	callback, err := host.RegisterPublicAPI(pluginID, plugins.PublicAPIRoute{Path: apiPath + "/callback", Handle: p.callback})
	if err != nil {
		login.Unregister()
		return err
	}
	p.routes = []plugins.ProtectedAPIRouteHandle{login, callback}
	return nil
}

func (p *oidcPlugin) Stop(plugins.BackendStartHost) error {
	p.unregister()
	return nil
}

func (p *oidcPlugin) unregister() {
	for _, route := range p.routes {
		route.Unregister()
	}
	p.routes = nil
}

func (p *oidcPlugin) AuthProviders(context.Context, plugins.BackendHost) []plugins.AuthProvider {
	cfg := oidcConfigFromEnv()
	if !cfg.Configured() {
		return nil
	}
	return []plugins.AuthProvider{{ID: pluginID, Name: cfg.ProviderName, LoginURL: "/api/" + apiPath + "/login"}}
}

func (p *oidcPlugin) login(host plugins.APIHost, _ string, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	cfg := oidcConfigFromEnv()
	if !cfg.Configured() {
		host.WriteAPIError(w, http.StatusServiceUnavailable, "OIDC is not configured.")
		return
	}
	discovery, err := discoverOIDC(r.Context(), cfg.Issuer)
	if err != nil {
		host.ServerError(w, err)
		return
	}
	state, err := randomToken()
	if err != nil {
		host.ServerError(w, err)
		return
	}
	nonce, err := randomToken()
	if err != nil {
		host.ServerError(w, err)
		return
	}
	redirectURI := cfg.redirectURL(r)
	http.SetCookie(w, &http.Cookie{
		Name:     stateCookie,
		Value:    state + "." + nonce,
		Path:     "/api/" + apiPath,
		MaxAge:   600,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   requestIsHTTPS(r),
	})
	authURL, err := url.Parse(discovery.AuthorizationEndpoint)
	if err != nil {
		host.ServerError(w, err)
		return
	}
	query := authURL.Query()
	query.Set("client_id", cfg.ClientID)
	query.Set("redirect_uri", redirectURI)
	query.Set("response_type", "code")
	query.Set("scope", cfg.Scopes)
	query.Set("state", state)
	query.Set("nonce", nonce)
	authURL.RawQuery = query.Encode()
	http.Redirect(w, r, authURL.String(), http.StatusFound)
}

func (p *oidcPlugin) callback(host plugins.APIHost, _ string, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	cfg := oidcConfigFromEnv()
	if !cfg.Configured() {
		host.WriteAPIError(w, http.StatusServiceUnavailable, "OIDC is not configured.")
		return
	}
	if oidcErr := strings.TrimSpace(r.URL.Query().Get("error")); oidcErr != "" {
		host.WriteAPIError(w, http.StatusUnauthorized, "OIDC sign-in failed: "+oidcErr)
		return
	}
	code := strings.TrimSpace(r.URL.Query().Get("code"))
	state := strings.TrimSpace(r.URL.Query().Get("state"))
	if code == "" || state == "" {
		host.WriteAPIError(w, http.StatusBadRequest, "OIDC callback is missing code or state.")
		return
	}
	cookie, err := r.Cookie(stateCookie)
	if err != nil {
		host.WriteAPIError(w, http.StatusBadRequest, "OIDC sign-in state has expired.")
		return
	}
	clearStateCookie(w, r)
	expectedState, nonce, ok := strings.Cut(cookie.Value, ".")
	if !ok || subtle.ConstantTimeCompare([]byte(state), []byte(expectedState)) != 1 {
		host.WriteAPIError(w, http.StatusBadRequest, "OIDC sign-in state is invalid.")
		return
	}
	discovery, err := discoverOIDC(r.Context(), cfg.Issuer)
	if err != nil {
		host.ServerError(w, err)
		return
	}
	token, err := exchangeCode(r.Context(), discovery.TokenEndpoint, cfg, cfg.redirectURL(r), code)
	if err != nil {
		host.WriteAPIError(w, http.StatusUnauthorized, err.Error())
		return
	}
	claims, err := validateIDToken(r.Context(), token.IDToken, discovery.JWKSURI, cfg.Issuer, cfg.ClientID, nonce)
	if err != nil {
		host.WriteAPIError(w, http.StatusUnauthorized, err.Error())
		return
	}
	if claims.Email == "" && discovery.UserinfoEndpoint != "" && token.AccessToken != "" {
		userinfoEmail, userinfoName, userinfoVerified, userinfoErr := fetchUserinfo(r.Context(), discovery.UserinfoEndpoint, token.AccessToken)
		if userinfoErr != nil {
			host.WriteAPIError(w, http.StatusUnauthorized, "OIDC account email could not be confirmed.")
			return
		}
		claims.Email = userinfoEmail
		if claims.Name == "" {
			claims.Name = userinfoName
		}
		// Userinfo-derived verification is only trusted when the ID token made
		// no claim at all; it can never override an explicit false.
		if claims.EmailVerified == nil {
			claims.EmailVerified = userinfoVerified
		}
	}
	email, err := normalizeEmail(claims.Email)
	if err != nil {
		host.WriteAPIError(w, http.StatusUnauthorized, "OIDC account has no usable email address.")
		return
	}
	// Account linking happens purely by email match below, so an unverified or
	// unasserted email_verified claim would let anyone who can set their IdP
	// profile email log into an existing Rolltop account.
	if claims.EmailVerified == nil || !*claims.EmailVerified {
		host.WriteAPIError(w, http.StatusUnauthorized, "OIDC email address is not verified.")
		return
	}
	if !cfg.EmailAllowed(email) {
		host.WriteAPIError(w, http.StatusForbidden, "OIDC email address is not allowed.")
		return
	}
	st, ok := host.Store().(*store.Store)
	if !ok || st == nil {
		host.ServerError(w, errors.New("store is not available"))
		return
	}
	user, err := st.GetUserByEmail(r.Context(), email)
	if err != nil {
		if !cfg.AutoCreate {
			host.WriteAPIError(w, http.StatusUnauthorized, "No Rolltop user exists for this OIDC account.")
			return
		}
		user, err = createOIDCUser(r.Context(), st, email, firstNonEmpty(claims.Name, email))
	}
	if err != nil {
		host.ServerError(w, err)
		return
	}
	if err := host.LoginUserID(w, r, user.ID); err != nil {
		host.ServerError(w, err)
		return
	}
	http.Redirect(w, r, "/mail", http.StatusFound)
}

type oidcConfig struct {
	Issuer         string
	ClientID       string
	ClientSecret   string
	RedirectURL    string
	Scopes         string
	ProviderName   string
	AllowedEmails  map[string]bool
	AllowedDomains map[string]bool
	AutoCreate     bool
}

func oidcConfigFromEnv() oidcConfig {
	return oidcConfig{
		Issuer:         strings.TrimRight(strings.TrimSpace(os.Getenv("ROLLTOP_OIDC_ISSUER")), "/"),
		ClientID:       strings.TrimSpace(os.Getenv("ROLLTOP_OIDC_CLIENT_ID")),
		ClientSecret:   strings.TrimSpace(os.Getenv("ROLLTOP_OIDC_CLIENT_SECRET")),
		RedirectURL:    strings.TrimSpace(os.Getenv("ROLLTOP_OIDC_REDIRECT_URL")),
		Scopes:         firstNonEmpty(strings.TrimSpace(os.Getenv("ROLLTOP_OIDC_SCOPES")), "openid email profile"),
		ProviderName:   firstNonEmpty(strings.TrimSpace(os.Getenv("ROLLTOP_OIDC_NAME")), "OIDC"),
		AllowedEmails:  csvSet(os.Getenv("ROLLTOP_OIDC_ALLOWED_EMAILS")),
		AllowedDomains: csvSet(os.Getenv("ROLLTOP_OIDC_ALLOWED_DOMAINS")),
		AutoCreate:     boolEnv(os.Getenv("ROLLTOP_OIDC_AUTO_CREATE")),
	}
}

func (c oidcConfig) Configured() bool {
	return c.Issuer != "" && c.ClientID != "" && c.ClientSecret != ""
}

func (c oidcConfig) redirectURL(r *http.Request) string {
	if c.RedirectURL != "" {
		return c.RedirectURL
	}
	return requestBaseURL(r) + "/api/" + apiPath + "/callback"
}

func (c oidcConfig) EmailAllowed(email string) bool {
	if len(c.AllowedEmails) == 0 && len(c.AllowedDomains) == 0 {
		return true
	}
	if c.AllowedEmails[email] {
		return true
	}
	_, domain, ok := strings.Cut(email, "@")
	return ok && c.AllowedDomains[strings.ToLower(domain)]
}

type discoveryDoc struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
	UserinfoEndpoint      string `json:"userinfo_endpoint"`
}

func discoverOIDC(ctx context.Context, issuer string) (discoveryDoc, error) {
	var doc discoveryDoc
	if issuer == "" {
		return doc, errors.New("OIDC issuer is not configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, issuer+"/.well-known/openid-configuration", nil)
	if err != nil {
		return doc, err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return doc, err
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		return doc, fmt.Errorf("OIDC discovery failed with status %d", res.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&doc); err != nil {
		return doc, err
	}
	if strings.TrimRight(doc.Issuer, "/") != issuer {
		return doc, errors.New("OIDC issuer mismatch")
	}
	if doc.AuthorizationEndpoint == "" || doc.TokenEndpoint == "" || doc.JWKSURI == "" {
		return doc, errors.New("OIDC discovery document is incomplete")
	}
	return doc, nil
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	IDToken     string `json:"id_token"`
	TokenType   string `json:"token_type"`
}

func exchangeCode(ctx context.Context, tokenEndpoint string, cfg oidcConfig, redirectURI, code string) (tokenResponse, error) {
	values := url.Values{}
	values.Set("grant_type", "authorization_code")
	values.Set("code", code)
	values.Set("redirect_uri", redirectURI)
	values.Set("client_id", cfg.ClientID)
	values.Set("client_secret", cfg.ClientSecret)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint, strings.NewReader(values.Encode()))
	if err != nil {
		return tokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return tokenResponse{}, err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode/100 != 2 {
		return tokenResponse{}, fmt.Errorf("OIDC token exchange failed with status %d", res.StatusCode)
	}
	var token tokenResponse
	if err := json.Unmarshal(raw, &token); err != nil {
		return tokenResponse{}, err
	}
	if token.IDToken == "" {
		return tokenResponse{}, errors.New("OIDC token response did not include an id_token")
	}
	return token, nil
}

type idTokenClaims struct {
	Iss           string          `json:"iss"`
	Sub           string          `json:"sub"`
	Aud           json.RawMessage `json:"aud"`
	Exp           int64           `json:"exp"`
	Nbf           int64           `json:"nbf"`
	Nonce         string          `json:"nonce"`
	Email         string          `json:"email"`
	EmailVerified *bool           `json:"email_verified"`
	Name          string          `json:"name"`
}

func validateIDToken(ctx context.Context, token, jwksURI, issuer, clientID, nonce string) (idTokenClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return idTokenClaims{}, errors.New("OIDC id_token is malformed")
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := decodeJWTPart(parts[0], &header); err != nil {
		return idTokenClaims{}, err
	}
	if header.Alg != "RS256" {
		return idTokenClaims{}, fmt.Errorf("OIDC id_token uses unsupported alg %q", header.Alg)
	}
	key, err := fetchRS256Key(ctx, jwksURI, header.Kid)
	if err != nil {
		return idTokenClaims{}, err
	}
	signed := []byte(parts[0] + "." + parts[1])
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return idTokenClaims{}, err
	}
	sum := sha256.Sum256(signed)
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, sum[:], sig); err != nil {
		return idTokenClaims{}, errors.New("OIDC id_token signature is invalid")
	}
	var claims idTokenClaims
	if err := decodeJWTPart(parts[1], &claims); err != nil {
		return idTokenClaims{}, err
	}
	now := time.Now().Unix()
	if strings.TrimRight(claims.Iss, "/") != strings.TrimRight(issuer, "/") {
		return idTokenClaims{}, errors.New("OIDC id_token issuer is invalid")
	}
	if !audienceContains(claims.Aud, clientID) {
		return idTokenClaims{}, errors.New("OIDC id_token audience is invalid")
	}
	if claims.Exp == 0 || now > claims.Exp {
		return idTokenClaims{}, errors.New("OIDC id_token has expired")
	}
	if claims.Nbf != 0 && now+60 < claims.Nbf {
		return idTokenClaims{}, errors.New("OIDC id_token is not valid yet")
	}
	if nonce != "" && claims.Nonce != nonce {
		return idTokenClaims{}, errors.New("OIDC id_token nonce is invalid")
	}
	return claims, nil
}

func fetchRS256Key(ctx context.Context, jwksURI, kid string) (*rsa.PublicKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, jwksURI, nil)
	if err != nil {
		return nil, err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		return nil, fmt.Errorf("OIDC JWKS fetch failed with status %d", res.StatusCode)
	}
	var jwks struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			Use string `json:"use"`
			Alg string `json:"alg"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 2<<20)).Decode(&jwks); err != nil {
		return nil, err
	}
	for _, candidate := range jwks.Keys {
		if candidate.Kty != "RSA" || (kid != "" && candidate.Kid != kid) {
			continue
		}
		nBytes, err := base64.RawURLEncoding.DecodeString(candidate.N)
		if err != nil {
			return nil, err
		}
		eBytes, err := base64.RawURLEncoding.DecodeString(candidate.E)
		if err != nil {
			return nil, err
		}
		e := 0
		for _, b := range eBytes {
			e = e*256 + int(b)
		}
		if e == 0 {
			continue
		}
		return &rsa.PublicKey{N: new(big.Int).SetBytes(nBytes), E: e}, nil
	}
	return nil, errors.New("OIDC signing key was not found")
}

func fetchUserinfo(ctx context.Context, endpoint, accessToken string) (string, string, *bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", "", nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", nil, err
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		return "", "", nil, fmt.Errorf("OIDC userinfo failed with status %d", res.StatusCode)
	}
	var out struct {
		Email         string `json:"email"`
		Name          string `json:"name"`
		EmailVerified *bool  `json:"email_verified"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&out); err != nil {
		return "", "", nil, err
	}
	return out.Email, out.Name, out.EmailVerified, nil
}

func createOIDCUser(ctx context.Context, st *store.Store, email, name string) (store.User, error) {
	password, err := randomToken()
	if err != nil {
		return store.User{}, err
	}
	hash, err := auth.HashPassword(password + randomStringSuffix())
	if err != nil {
		return store.User{}, err
	}
	users, err := st.ListUsers(ctx)
	if err != nil {
		return store.User{}, err
	}
	user, err := st.CreateUser(ctx, email, name, hash, len(users) == 0)
	if err != nil {
		return store.User{}, err
	}
	_, _ = st.EnsureMeContactForEmail(ctx, user.ID, user.Email, firstNonEmpty(user.Name, user.Email))
	return user, nil
}

func decodeJWTPart(part string, dest any) error {
	raw, err := base64.RawURLEncoding.DecodeString(part)
	if err != nil {
		return err
	}
	return json.NewDecoder(bytes.NewReader(raw)).Decode(dest)
}

func audienceContains(raw json.RawMessage, clientID string) bool {
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		return one == clientID
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err != nil {
		return false
	}
	for _, item := range many {
		if item == clientID {
			return true
		}
	}
	return false
}

func randomToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

func randomStringSuffix() string {
	token, err := randomToken()
	if err != nil {
		return ".oidc"
	}
	return "." + token
}

func clearStateCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: stateCookie, Value: "", Path: "/api/" + apiPath, MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: requestIsHTTPS(r)})
}

func normalizeEmail(value string) (string, error) {
	addr, err := mail.ParseAddress(strings.TrimSpace(value))
	if err != nil {
		return "", err
	}
	email := strings.ToLower(strings.TrimSpace(addr.Address))
	if email == "" || !strings.Contains(email, "@") {
		return "", errors.New("invalid email")
	}
	return email, nil
}

func requestBaseURL(r *http.Request) string {
	scheme := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto"))
	if scheme == "" {
		if r.TLS != nil {
			scheme = "https"
		} else {
			scheme = "http"
		}
	}
	host := strings.TrimSpace(r.Header.Get("X-Forwarded-Host"))
	if host == "" {
		host = r.Host
	}
	return scheme + "://" + host
}

func requestIsHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https")
}

func csvSet(value string) map[string]bool {
	out := map[string]bool{}
	for _, item := range strings.Split(value, ",") {
		item = strings.ToLower(strings.TrimSpace(item))
		if item != "" {
			out[item] = true
		}
	}
	return out
}

func boolEnv(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
