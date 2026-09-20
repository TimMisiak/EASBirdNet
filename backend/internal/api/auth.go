package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/ngaitonde/EASBirdNet/backend/internal/db"
)

// Sign-in is OpenID Connect against Google and Microsoft. The browser is sent
// to the provider by /api/v1/auth/{provider}/start and comes back to
// .../callback with a code, which the server swaps for an ID token over its
// own connection to the provider.
//
// The roster is the allow-list, not the provider: signing in proves which
// address you own, and a verified address nobody has added is refused. That is
// why the sign-in audience can safely be "any Microsoft account".

// Providers Birdsense knows how to sign people in with. The name is what goes
// in the roster's identity records (SCHEMA.md, users.identity.provider).
const (
	ProviderMicrosoft = "microsoft"
	ProviderGoogle    = "google"
)

// microsoftConsumerTenant is the tenant every personal Microsoft account
// (outlook.com, hotmail.com, and addresses people brought to one) signs in
// from. It is the same well-known id for everyone.
const microsoftConsumerTenant = "9188040d-6c67-4c5b-b112-36a304b66dad"

// ProviderConfig is one identity provider, as configured.
type ProviderConfig struct {
	// Name is ProviderMicrosoft or ProviderGoogle.
	Name string
	// ClientID and ClientSecret come from the provider's app registration.
	ClientID     string
	ClientSecret string
	// Tenant, for Microsoft, is the directory to sign in against: "common"
	// accepts any organization and any personal account, which is what a
	// program of volunteers with their own addresses needs. A tenant id here
	// instead restricts sign-in to that one directory.
	Tenant string
}

// AuthConfig is everything sign-in needs that isn't the roster.
type AuthConfig struct {
	// PublicURL is where a browser reaches Birdsense: scheme and host, no
	// trailing slash. The redirect URI is built from it, so it has to match
	// what is registered with the provider exactly.
	//
	// It is configuration rather than the request's Host header on purpose: a
	// spoofed Host would otherwise choose where the provider sends people back
	// to.
	PublicURL string
	Providers []ProviderConfig
}

// Authenticator holds the configured providers and what discovery found out
// about them.
type Authenticator struct {
	publicURL string
	log       *slog.Logger
	providers map[string]*authProvider
}

// authProvider is one provider's configuration plus the endpoints and keys
// discovered from it. Discovery is a network call, so it is done once and
// retried on demand rather than at startup only -- sign-in being unavailable
// for a minute is better than the server refusing to start.
type authProvider struct {
	cfg ProviderConfig
	// issuer is the discovery document to read.
	issuer string
	// issuerTemplate is set when the issuer the provider reports is not the
	// one asked for. Microsoft's "common" endpoint reports a per-tenant
	// issuer, so the check moves to verifyIssuer.
	issuerTemplate string
	redirectURL    string

	mu       sync.Mutex
	oauth    *oauth2.Config
	verifier *oidc.IDTokenVerifier
}

// NewAuthenticator prepares the configured providers and tries to reach each
// one. A provider that can't be reached yet is kept and retried on first use,
// so a blip at the provider doesn't stop the server (the pattern BirdNET
// already uses: warn, carry on, and work when it works).
func NewAuthenticator(ctx context.Context, cfg AuthConfig, log *slog.Logger) (*Authenticator, error) {
	if len(cfg.Providers) == 0 {
		return nil, nil
	}
	if cfg.PublicURL == "" {
		return nil, errors.New("sign-in needs a public URL to build its redirect URI from")
	}
	base := strings.TrimSuffix(cfg.PublicURL, "/")
	if u, err := url.Parse(base); err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("the public URL must be a scheme and host, like https://owls.eastsideaudubon.org, not %q", cfg.PublicURL)
	}

	a := &Authenticator{publicURL: base, log: log, providers: map[string]*authProvider{}}
	for _, p := range cfg.Providers {
		switch p.Name {
		case ProviderMicrosoft, ProviderGoogle:
		default:
			return nil, fmt.Errorf("unknown identity provider %q", p.Name)
		}
		if p.ClientID == "" || p.ClientSecret == "" {
			return nil, fmt.Errorf("the %s provider needs a client id and secret", p.Name)
		}
		ap := &authProvider{cfg: p, redirectURL: base + "/api/v1/auth/" + p.Name + "/callback"}
		switch p.Name {
		case ProviderMicrosoft:
			tenant := p.Tenant
			if tenant == "" {
				tenant = "common"
			}
			ap.issuer = "https://login.microsoftonline.com/" + tenant + "/v2.0"
			// Asked for "common" or "organizations", Microsoft answers with a
			// placeholder issuer rather than a real one, because the real one
			// depends on who signs in.
			if tenant == "common" || tenant == "organizations" || tenant == "consumers" {
				ap.issuerTemplate = "https://login.microsoftonline.com/{tenantid}/v2.0"
			}
		case ProviderGoogle:
			ap.issuer = "https://accounts.google.com"
		}
		a.providers[p.Name] = ap
		if err := ap.connect(ctx); err != nil {
			log.Warn("couldn't reach the identity provider yet, so sign-in with it will retry on first use",
				"provider", p.Name, "issuer", ap.issuer, "err", err)
		} else {
			log.Info("sign-in ready", "provider", p.Name, "issuer", ap.issuer, "redirect_uri", ap.redirectURL)
		}
	}
	return a, nil
}

// Names is the providers offered, for the sign-in page.
func (a *Authenticator) Names() []string {
	if a == nil {
		return nil
	}
	names := make([]string, 0, len(a.providers))
	for _, name := range []string{ProviderMicrosoft, ProviderGoogle} {
		if _, ok := a.providers[name]; ok {
			names = append(names, name)
		}
	}
	return names
}

// RedirectURIs is every redirect URI that has to be registered with a
// provider, so startup can log exactly what to paste into the app
// registration.
func (a *Authenticator) RedirectURIs() map[string]string {
	if a == nil {
		return nil
	}
	out := map[string]string{}
	for name, p := range a.providers {
		out[name] = p.redirectURL
	}
	return out
}

func (a *Authenticator) provider(ctx context.Context, name string) (*authProvider, error) {
	p, ok := a.providers[name]
	if !ok {
		return nil, errNoSuchProvider
	}
	if err := p.connect(ctx); err != nil {
		return nil, err
	}
	return p, nil
}

var errNoSuchProvider = errors.New("api: no such identity provider")

// connect discovers the provider's endpoints and keys, once.
func (p *authProvider) connect(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.oauth != nil {
		return nil
	}
	discoverCtx := ctx
	if p.issuerTemplate != "" {
		// go-oidc refuses a discovery document whose issuer isn't the one
		// asked for. Microsoft's multi-tenant endpoints always fail that, so
		// tell go-oidc what to expect and check the real issuer per token in
		// verifyIssuer instead.
		discoverCtx = oidc.InsecureIssuerURLContext(ctx, p.issuerTemplate)
	}
	op, err := oidc.NewProvider(discoverCtx, p.issuer)
	if err != nil {
		return err
	}
	p.verifier = op.Verifier(&oidc.Config{
		ClientID:        p.cfg.ClientID,
		SkipIssuerCheck: p.issuerTemplate != "",
	})
	p.oauth = &oauth2.Config{
		ClientID:     p.cfg.ClientID,
		ClientSecret: p.cfg.ClientSecret,
		Endpoint:     op.Endpoint(),
		RedirectURL:  p.redirectURL,
		Scopes:       []string{oidc.ScopeOpenID, "profile", "email"},
	}
	return nil
}

// verifyIssuer checks the issuer of a token from a multi-tenant endpoint,
// which go-oidc was told to skip. Microsoft signs with a per-tenant issuer, so
// the token has to name the tenant it says it came from.
func (p *authProvider) verifyIssuer(issuer, tenant string) error {
	if p.issuerTemplate == "" {
		return nil
	}
	if tenant == "" {
		return errors.New("the token has no tenant claim")
	}
	want := strings.ReplaceAll(p.issuerTemplate, "{tenantid}", tenant)
	if issuer != want {
		return fmt.Errorf("the token's issuer is %q, not the %q its tenant claim implies", issuer, want)
	}
	return nil
}

// --- the flow ---

// authState is what the server has to remember between sending someone to the
// provider and their coming back. It goes in a short-lived signed cookie
// rather than in memory, so it survives a restart mid-sign-in and needs no
// shared store if this ever runs on more than one replica.
type authState struct {
	Provider string `json:"p"`
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
}

const authStateCookie = "bs_auth"

// authStateLife is how long someone has to finish signing in.
const authStateLife = 15 * time.Minute

// authStart sends the browser to the provider.
func (h *handlers) authStart(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("provider")
	p, err := h.auth.provider(r.Context(), name)
	switch {
	case errors.Is(err, errNoSuchProvider):
		h.problem(w, http.StatusNotFound, "no such identity provider")
		return
	case err != nil:
		h.log.Error("couldn't reach the identity provider", "provider", name, "err", err)
		h.signInFailed(w, r, "provider-unreachable")
		return
	}

	state := authState{Provider: name, State: randomToken(), Nonce: randomToken(), Verifier: oauth2.GenerateVerifier()}
	encoded, err := json.Marshal(state)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:  authStateCookie,
		Value: h.keys.sign(string(encoded), h.now().Add(authStateLife)),
		Path:  "/api/v1/auth/",
		// Lax, not Strict: the provider sends the browser back with a
		// top-level GET, which Strict would not send this cookie on.
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: h.secureCookies,
		MaxAge: int(authStateLife / time.Second),
	})
	http.Redirect(w, r, p.oauth.AuthCodeURL(state.State,
		oidc.Nonce(state.Nonce),
		oauth2.S256ChallengeOption(state.Verifier),
	), http.StatusFound)
}

// authCallback is where the provider sends the browser back. It answers with a
// redirect either way, because what is looking at it is a person, not script:
// into the app when it worked, back to the sign-in page with a reason when it
// didn't.
func (h *handlers) authCallback(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("provider")
	// However this ends, the state has been used.
	http.SetCookie(w, &http.Cookie{Name: authStateCookie, Value: "", Path: "/api/v1/auth/", MaxAge: -1})

	state, err := h.authState(r)
	if err != nil {
		h.log.Info("sign-in came back without usable state", "provider", name, "err", err)
		h.signInFailed(w, r, "expired")
		return
	}
	if state.Provider != name {
		h.signInFailed(w, r, "expired")
		return
	}
	if q := r.URL.Query(); q.Get("error") != "" {
		// The person said no at the provider, or the provider refused.
		h.log.Info("the identity provider refused", "provider", name, "error", q.Get("error"), "description", q.Get("error_description"))
		h.signInFailed(w, r, "denied")
		return
	}
	if got := r.URL.Query().Get("state"); got != state.State {
		h.log.Warn("sign-in state did not match", "provider", name)
		h.signInFailed(w, r, "state")
		return
	}

	p, err := h.auth.provider(r.Context(), name)
	if err != nil {
		h.signInFailed(w, r, "provider-unreachable")
		return
	}

	me, err := h.identify(r.Context(), p, r.URL.Query().Get("code"), state)
	var refused signInRefused
	switch {
	case errors.As(err, &refused):
		h.log.Info("sign-in refused", "provider", name, "reason", refused.reason, "err", refused.err)
		h.signInFailed(w, r, refused.reason)
		return
	case err != nil:
		h.log.Error("sign-in failed", "provider", name, "err", err)
		h.signInFailed(w, r, "failed")
		return
	}

	h.setSession(w, me)
	h.log.Info("signed in", "provider", name, "email", me.Email, "role", me.Role)
	// Where the app puts someone on arrival, as the sign-in page does.
	landing := "/app"
	if me.Role == db.RoleAdmin {
		landing = "/admin/people"
	}
	http.Redirect(w, r, landing, http.StatusFound)
}

// signInRefused is a sign-in that failed for a reason the person should see,
// rather than a fault to log and hide.
type signInRefused struct {
	reason string
	err    error
}

func (e signInRefused) Error() string { return "sign-in refused: " + e.reason }
func (e signInRefused) Unwrap() error { return e.err }

func refuse(reason string, err error) error { return signInRefused{reason: reason, err: err} }

// authState reads back what authStart put in the cookie.
func (h *handlers) authState(r *http.Request) (authState, error) {
	var state authState
	c, err := r.Cookie(authStateCookie)
	if err != nil {
		return state, errors.New("api: no sign-in state cookie")
	}
	value, err := h.keys.verify(c.Value, h.now())
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal([]byte(value), &state); err != nil {
		return state, err
	}
	return state, nil
}

// signInFailed sends the browser back to the sign-in page, which turns the
// reason into a sentence.
func (h *handlers) signInFailed(w http.ResponseWriter, r *http.Request, reason string) {
	http.Redirect(w, r, "/signin?error="+url.QueryEscape(reason), http.StatusFound)
}

// --- who signed in ---

// idClaims is what Birdsense reads out of an ID token.
type idClaims struct {
	Subject string `json:"sub"`
	Name    string `json:"name"`
	Email   string `json:"email"`
	Nonce   string `json:"nonce"`
	// PreferredUsername is the address Microsoft shows people, and is what
	// carries it when the email claim is absent.
	PreferredUsername string `json:"preferred_username"`
	// EmailVerified is Google's word that the address is the person's.
	EmailVerified *bool `json:"email_verified"`
	// Tenant is Microsoft's "tid": which directory signed this person in.
	Tenant string `json:"tid"`
	// EmailDomainOwnerVerified is Microsoft's "xms_edov": the signing tenant
	// has proved it owns the domain of the email claim. See trustedEmail.
	EmailDomainOwnerVerified *bool `json:"xms_edov"`
}

// identify turns the code the provider sent back into someone on the roster.
func (h *handlers) identify(ctx context.Context, p *authProvider, code string, state authState) (db.User, error) {
	if code == "" {
		return db.User{}, refuse("failed", errors.New("the provider sent no code"))
	}
	token, err := p.oauth.Exchange(ctx, code, oauth2.VerifierOption(state.Verifier))
	if err != nil {
		return db.User{}, fmt.Errorf("exchanging the code: %w", err)
	}
	raw, ok := token.Extra("id_token").(string)
	if !ok {
		return db.User{}, errors.New("the provider returned no ID token")
	}
	idToken, err := p.verifier.Verify(ctx, raw)
	if err != nil {
		return db.User{}, fmt.Errorf("verifying the ID token: %w", err)
	}

	var claims idClaims
	if err := idToken.Claims(&claims); err != nil {
		return db.User{}, fmt.Errorf("reading the ID token: %w", err)
	}
	if err := p.verifyIssuer(idToken.Issuer, claims.Tenant); err != nil {
		return db.User{}, err
	}
	// The nonce ties this token to the request that started this sign-in, so a
	// token captured from another one can't be replayed here.
	if claims.Nonce != state.Nonce {
		return db.User{}, errors.New("the ID token's nonce is not this sign-in's")
	}

	email, err := trustedEmail(p, claims)
	if err != nil {
		return db.User{}, refuse("unverified-email", err)
	}

	me, err := h.store.GetUserByEmail(ctx, email)
	switch {
	case errors.Is(err, db.ErrNotFound):
		return db.User{}, refuse("not-on-roster", fmt.Errorf("%s is not on the roster", email))
	case err != nil:
		return db.User{}, err
	}
	if me.RemovedAt != nil {
		return db.User{}, refuse("not-on-roster", fmt.Errorf("%s was removed from the roster", email))
	}

	return h.store.UpdateUser(ctx, me.ID, func(u *db.User) error {
		if u.RemovedAt != nil {
			return db.ErrNotFound
		}
		// The identity is bound at the first sign-in and kept for the audit
		// trail. It is re-bound rather than enforced, because a subject is
		// only stable for one app registration: moving the registration to
		// another tenant issues everyone a new one, and the roster address --
		// which the provider has just vouched for -- is what says who this is.
		if u.Identity == nil {
			u.Identity = &db.Identity{Provider: p.cfg.Name, Subject: claims.Subject}
		} else if u.Identity.Provider != p.cfg.Name || u.Identity.Subject != claims.Subject {
			h.log.Warn("rebinding a roster identity: the provider or subject changed",
				"email", u.Email, "was_provider", u.Identity.Provider, "now_provider", p.cfg.Name)
			u.Identity = &db.Identity{Provider: p.cfg.Name, Subject: claims.Subject}
		}
		// The roster keeps the name a coordinator typed; fill it in only if
		// there isn't one.
		if strings.TrimSpace(u.Name) == "" && claims.Name != "" {
			u.Name = claims.Name
		}
		t := h.stamp()
		u.LastSignInAt = &t
		return nil
	})
}

// trustedEmail is the address Birdsense will match against the roster, or an
// error saying why this token's address can't be trusted to be the person's.
//
// This matters because sign-in accepts any Microsoft account, which means any
// organization's directory. A directory administrator can put whatever they
// like in one of their users' email claims, including a volunteer's address,
// so an email claim alone is not proof of anything. Three cases are proof:
//
//   - a personal Microsoft account, where the address is the account itself;
//   - an organization that has proved to Microsoft it owns the domain of the
//     address (xms_edov), which is an optional claim the app registration has
//     to ask for;
//   - Google saying email_verified.
//
// Everything else is refused, with a reason in the log.
func trustedEmail(p *authProvider, c idClaims) (string, error) {
	email := strings.ToLower(strings.TrimSpace(c.Email))
	if email == "" {
		email = strings.ToLower(strings.TrimSpace(c.PreferredUsername))
	}
	if email == "" || !strings.Contains(email, "@") {
		return "", errors.New("the token carries no email address")
	}

	switch p.cfg.Name {
	case ProviderGoogle:
		if c.EmailVerified == nil || !*c.EmailVerified {
			return "", fmt.Errorf("Google did not report %s as verified", email)
		}
		return email, nil

	case ProviderMicrosoft:
		switch {
		case c.Tenant == microsoftConsumerTenant:
			// A personal account: the address is the account.
			return email, nil
		case c.EmailDomainOwnerVerified != nil && *c.EmailDomainOwnerVerified:
			return email, nil
		case p.issuerTemplate == "":
			// Sign-in is restricted to one configured directory, so its
			// administrators are the ones running this program anyway.
			return email, nil
		}
		return "", fmt.Errorf("the tenant %s has not proved it owns the domain of %s (add the optional claim xms_edov to the app registration)", c.Tenant, email)
	}
	return "", fmt.Errorf("unknown provider %q", p.cfg.Name)
}
