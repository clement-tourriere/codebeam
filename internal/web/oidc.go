package web

import (
	"errors"
	"net/http"
	"strings"
	"time"
)

// oidcAuthCookie carries state, nonce, and the PKCE verifier across the SSO
// round trip, signed and short-lived.
const (
	oidcAuthCookie = "codebeam_oidc_auth"
	oidcAuthTTL    = 10 * time.Minute
	oidcProvider   = "oidc"
)

// handleOIDCStart begins the SSO login: it stashes state/nonce/verifier in a
// signed cookie and redirects to the provider's authorization endpoint.
func (s *Server) handleOIDCStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	if !s.oidc.Configured() {
		http.NotFound(w, r)
		return
	}
	state, nonce, verifier := randomToken(), randomToken(), randomToken()
	authURL, err := s.oidc.AuthCodeURL(r.Context(), state, nonce, verifier, s.oauthRedirectURI(oidcProvider))
	if err != nil {
		redirectError(w, r, s.authErrorTarget(r), err)
		return
	}
	s.setSignedCookie(w, oidcAuthCookie, state+":"+nonce+":"+verifier, oidcAuthTTL)
	http.Redirect(w, r, authURL, http.StatusFound)
}

// handleOIDCCallback completes the SSO login: state check, code exchange,
// ID-token validation (done by the oidc client), the optional email-domain
// allowlist, then user provisioning and the session cookie.
func (s *Server) handleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	if !s.oidc.Configured() {
		http.NotFound(w, r)
		return
	}
	raw, ok := s.readSignedCookie(r, oidcAuthCookie)
	http.SetCookie(w, &http.Cookie{Name: oidcAuthCookie, Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	parts := strings.SplitN(raw, ":", 3)
	if !ok || len(parts) != 3 || parts[0] == "" || parts[0] != r.URL.Query().Get("state") {
		redirectError(w, r, "/login", errors.New("invalid SSO state"))
		return
	}
	nonce, verifier := parts[1], parts[2]
	if errCode := r.URL.Query().Get("error"); errCode != "" {
		redirectError(w, r, "/login", errors.New("SSO login failed: "+errCode))
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		redirectError(w, r, "/login", errors.New("SSO callback did not include a code"))
		return
	}
	identity, err := s.oidc.Exchange(r.Context(), code, verifier, nonce, s.oauthRedirectURI(oidcProvider))
	if err != nil {
		redirectError(w, r, "/login", err)
		return
	}
	if err := s.checkOIDCDomain(identity.Email, identity.EmailVerified); err != nil {
		redirectError(w, r, "/login", err)
		return
	}
	user, err := s.store.UpsertUserIdentity(
		r.Context(),
		oidcProvider,
		identity.Subject,
		identity.Email,
		identity.Email,
		identity.Name,
		identity.Picture,
		"", // no provider API token to keep — OIDC is login-only
	)
	if err != nil {
		redirectError(w, r, "/login", err)
		return
	}
	s.setSession(w, user.ID)
	http.Redirect(w, r, s.consumeLoginNext(w, r, "/repos"), http.StatusSeeOther)
}

// checkOIDCDomain enforces CODEBEAM_OIDC_ALLOWED_DOMAINS when configured. The
// email must be provider-verified: an unverified email is attacker-controlled
// at many IdPs, so trusting it for the domain gate would let anyone claim a
// company address.
func (s *Server) checkOIDCDomain(email string, verified bool) error {
	if len(s.cfg.OIDCAllowedDomains) == 0 {
		return nil
	}
	if !verified {
		return errors.New("this instance restricts sign-in by email domain, but the provider did not confirm your email address is verified")
	}
	_, domain, found := strings.Cut(strings.ToLower(strings.TrimSpace(email)), "@")
	if !found {
		return errors.New("SSO account has no email address; this instance restricts sign-in by email domain")
	}
	for _, allowed := range s.cfg.OIDCAllowedDomains {
		if domain == allowed {
			return nil
		}
	}
	return errors.New("the email domain " + domain + " is not allowed on this instance")
}
