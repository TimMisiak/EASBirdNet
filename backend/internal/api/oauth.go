package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"

	"github.com/ngaitonde/EASBirdNet/backend/internal/db"
)

// An MCP client -- a claude.ai connector, Claude Code, anything that speaks
// the MCP authorization spec -- signs in with OAuth 2.1, and Birdsense is its
// own authorization server for it. It can't hand the client to Microsoft or
// Google: neither lets a client register itself, and what lets anyone in is
// the roster, which only Birdsense knows. So the authorization endpoint is
// the sign-in Birdsense already has, plus a page that asks "let this app read
// Birdsense as you?" (<bs-connect-page>), and the tokens it issues are signed
// values like the session cookie (cookies.go), each under a key of its own.
//
// Nothing here is stored. A client's registration is its client_id, signed; a
// code is single-use by a list of the ones spent, held in memory for the
// minute a code lives (one replica: DEPLOYMENT.md); and a token is good until
// it expires, while its holder is on the roster. So removing someone ends
// every connection they made, and rotating BIRDSENSE_SESSION_KEY ends every
// connection anyone made -- but one connection can't be ended on its own,
// which would take a stored grant per connection to do.
//
// The flow, as the client sees it:
//
//	POST /api/v1/mcp                        401, naming the resource metadata
//	GET  /.well-known/oauth-protected-resource/api/v1/mcp
//	GET  /.well-known/oauth-authorization-server
//	POST /api/v1/oauth/register             a client_id (RFC 7591)
//	GET  /api/v1/oauth/authorize?...        -> /connect?request=..., in a browser
//	     (sign in if need be; Allow posts to /api/v1/oauth/authorize)
//	     -> the client's redirect_uri?code=...&state=...&iss=...
//	POST /api/v1/oauth/token                code + PKCE verifier -> tokens
//	POST /api/v1/mcp                        Authorization: Bearer <access token>

const (
	// mcpPath is the MCP endpoint, and with the public URL in front of it, the
	// resource every access token is for.
	mcpPath = "/api/v1/mcp"
	// mcpScope is the one scope there is: reading what Birdsense has heard.
	// Nothing an MCP client can do changes anything.
	mcpScope = "birdsense.read"

	oauthAuthorizePath = "/api/v1/oauth/authorize"
	oauthTokenPath     = "/api/v1/oauth/token"
	oauthRegisterPath  = "/api/v1/oauth/register"
	// connectPage is the frontend's consent page.
	connectPage = "/connect"

	// oauthRequestLife is how long someone has between a client sending them
	// here and pressing Allow, signing in included.
	oauthRequestLife = 15 * time.Minute
	// oauthCodeLife is how long a client has to swap a code for tokens. It
	// does so at once, server to server.
	oauthCodeLife = time.Minute
	// oauthAccessLife is an access token's life. Short, because a refresh is
	// where the roster is checked against the grant -- though every request
	// checks it too.
	oauthAccessLife = time.Hour
	// oauthGrantLife is how long a connection lasts before its person has to
	// press Allow again: the session cookie's life, so connecting an app
	// lasts no longer than signing in does. Refreshing doesn't extend it.
	oauthGrantLife = sessionLife
	// oauthClientLife is a registration's life. A client registers once and
	// keeps its client_id, so this is "for good", within what a signed value
	// can say.
	oauthClientLife = 10 * 365 * 24 * time.Hour

	// Bounds on what a client may register, so a client_id stays a sensible
	// length and the consent page a sensible size.
	maxRedirectURIs   = 5
	maxRedirectURILen = 2000
	maxClientNameLen  = 100
)

// Token endpoint authentication methods (RFC 7591). A client that registers
// with a secret gets one derived from its client_id, so there is still
// nothing to store.
const (
	authMethodNone   = "none"
	authMethodPost   = "client_secret_post"
	authMethodBasic  = "client_secret_basic"
	grantCode        = "authorization_code"
	grantRefresh     = "refresh_token"
	defaultAppName   = "An app"
	oauthFormMaxSize = 64 << 10
)

// oauthServer is the authorization server: where it is, and the keys it signs
// with, one per kind of value so that none can be passed off as another.
type oauthServer struct {
	// issuer is the public URL, which is the authorization server's
	// identifier, and resource is the MCP endpoint's URL under it.
	issuer   string
	resource string

	clients  *keyset
	secrets  *keyset
	requests *keyset
	codes    *keyset
	access   *keyset
	refresh  *keyset
	exports  *keyset

	mu sync.Mutex
	// spent is every code swapped in the last oauthCodeLife, by id, with
	// when it expires: a code is single-use, and it is only worth
	// remembering until it would have expired anyway.
	spent map[string]time.Time
}

func newOAuthServer(publicURL string, keys *keyset) *oauthServer {
	issuer := strings.TrimSuffix(publicURL, "/")
	return &oauthServer{
		issuer:   issuer,
		resource: issuer + mcpPath,
		clients:  keys.derive("oauth-client"),
		secrets:  keys.derive("oauth-client-secret"),
		requests: keys.derive("oauth-request"),
		codes:    keys.derive("oauth-code"),
		access:   keys.derive("oauth-access"),
		refresh:  keys.derive("oauth-refresh"),
		exports:  keys.derive("mcp-export"),
		spent:    map[string]time.Time{},
	}
}

// resourceMetadataURL is where the MCP endpoint's protected resource metadata
// is (RFC 9728): the well-known path with the resource's own path after it.
func (o *oauthServer) resourceMetadataURL() string {
	return o.issuer + "/.well-known/oauth-protected-resource" + mcpPath
}

// spend marks a code used, and says whether it already was.
func (o *oauthServer) spend(id string, expires, now time.Time) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	for k, at := range o.spent {
		if now.After(at) {
			delete(o.spent, k)
		}
	}
	if _, used := o.spent[id]; used {
		return false
	}
	o.spent[id] = expires
	return true
}

// --- the signed values ---

// oauthClient is a registration, and the whole of what a client_id says.
type oauthClient struct {
	// ID makes each registration its own, so two that ask for the same
	// thing in the same second -- two people adding the same app -- aren't
	// one client, and a code issued to one can't be swapped by the other.
	ID           string   `json:"i"`
	Name         string   `json:"n"`
	RedirectURIs []string `json:"r"`
	AuthMethod   string   `json:"m"`
}

// oauthRequest is an authorization request that has passed the checks that
// need no one signed in, carried to the consent page and back.
type oauthRequest struct {
	ClientID    string `json:"c"`
	RedirectURI string `json:"r"`
	Challenge   string `json:"x"`
	State       string `json:"s,omitempty"`
}

// oauthCode is what Allow hands the client to swap for tokens.
type oauthCode struct {
	ID          string `json:"i"`
	ClientID    string `json:"c"`
	RedirectURI string `json:"r"`
	Challenge   string `json:"x"`
	Email       string `json:"e"`
	// Granted is when Allow was pressed, which the grant's life runs from.
	Granted int64 `json:"g"`
}

// oauthToken is an access or a refresh token's content. Expires repeats the
// signed expiry so the MCP endpoint can say when a token runs out.
type oauthToken struct {
	ClientID string `json:"c"`
	Email    string `json:"e"`
	Granted  int64  `json:"g"`
	Expires  int64  `json:"x"`
	// Audience is the resource an access token is for; a refresh token has
	// none, because only the token endpoint reads one.
	Audience string `json:"a,omitempty"`
}

func signJSON(k *keyset, v any, expires time.Time) string {
	b, err := json.Marshal(v)
	if err != nil {
		// Every value signed here is a struct of strings and numbers.
		panic(err)
	}
	return k.sign(string(b), expires)
}

func verifyJSON(k *keyset, signed string, now time.Time, v any) error {
	value, err := k.verify(signed, now)
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(value), v)
}

func (o *oauthServer) client(id string, now time.Time) (oauthClient, error) {
	var c oauthClient
	if id == "" {
		return c, errors.New("no client_id")
	}
	err := verifyJSON(o.clients, id, now, &c)
	return c, err
}

// clientSecret is the secret a client registered with one was given: a MAC of
// its client_id, so it needn't be stored to be checked.
func (o *oauthServer) clientSecret(clientID string) string {
	return o.secrets.mac(clientID)
}

// --- metadata ---

// authServerMetadata is RFC 8414's document, which is where a client finds
// every endpoint below.
func (h *handlers) authServerMetadata(w http.ResponseWriter, r *http.Request) {
	o := h.oauth
	w.Header().Set("Cache-Control", "public, max-age=3600")
	h.json(w, http.StatusOK, map[string]any{
		"issuer":                                o.issuer,
		"authorization_endpoint":                o.issuer + oauthAuthorizePath,
		"token_endpoint":                        o.issuer + oauthTokenPath,
		"registration_endpoint":                 o.issuer + oauthRegisterPath,
		"scopes_supported":                      []string{mcpScope},
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{grantCode, grantRefresh},
		"token_endpoint_auth_methods_supported": []string{authMethodNone, authMethodPost, authMethodBasic},
		"code_challenge_methods_supported":      []string{"S256"},
		// The redirect back carries iss (RFC 9207), so a client talking to
		// more than one server can tell whose code it holds.
		"authorization_response_iss_parameter_supported": true,
	})
}

// resourceMetadata is RFC 9728's document for the MCP endpoint: which
// authorization server its tokens come from.
func (o *oauthServer) resourceMetadata() http.Handler {
	return auth.ProtectedResourceMetadataHandler(&oauthex.ProtectedResourceMetadata{
		Resource:               o.resource,
		AuthorizationServers:   []string{o.issuer},
		ScopesSupported:        []string{mcpScope},
		BearerMethodsSupported: []string{"header"},
		ResourceName:           "Birdsense",
	})
}

// --- registration ---

// registerClient is dynamic client registration (RFC 7591), which is how
// claude.ai and most other MCP clients introduce themselves. Anyone may
// register: a registration grants nothing until someone on the roster presses
// Allow for it, and what it records -- a name and where to send codes -- is
// what the consent page shows them.
func (h *handlers) registerClient(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RedirectURIs            []string `json:"redirect_uris"`
		ClientName              string   `json:"client_name"`
		TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
		GrantTypes              []string `json:"grant_types"`
		ResponseTypes           []string `json:"response_types"`
	}
	// Unknown fields are allowed, unlike the rest of the API: clients send
	// RFC 7591's whole vocabulary (logo_uri, software_id, ...), and refusing
	// one would refuse the client.
	if err := json.NewDecoder(io.LimitReader(r.Body, oauthFormMaxSize)).Decode(&body); err != nil {
		registrationError(w, h, "invalid_client_metadata", "the body must be JSON client metadata")
		return
	}

	if len(body.RedirectURIs) == 0 || len(body.RedirectURIs) > maxRedirectURIs {
		registrationError(w, h, "invalid_redirect_uri", "register between one and five redirect_uris")
		return
	}
	for _, uri := range body.RedirectURIs {
		if problem := redirectURIProblem(uri); problem != "" {
			registrationError(w, h, "invalid_redirect_uri", problem)
			return
		}
	}
	method := body.TokenEndpointAuthMethod
	switch method {
	case "":
		// RFC 7591's default.
		method = authMethodBasic
	case authMethodNone, authMethodPost, authMethodBasic:
	default:
		registrationError(w, h, "invalid_client_metadata", "token_endpoint_auth_method must be none, client_secret_post or client_secret_basic")
		return
	}
	for _, g := range body.GrantTypes {
		if g != grantCode && g != grantRefresh {
			registrationError(w, h, "invalid_client_metadata", "grant_types may only be authorization_code and refresh_token")
			return
		}
	}
	for _, t := range body.ResponseTypes {
		if t != "code" {
			registrationError(w, h, "invalid_client_metadata", "response_types may only be code")
			return
		}
	}

	c := oauthClient{ID: randomToken()[:16], Name: clientName(body.ClientName), RedirectURIs: body.RedirectURIs, AuthMethod: method}
	issued := h.now()
	id := signJSON(h.oauth.clients, c, issued.Add(oauthClientLife))
	out := map[string]any{
		"client_id":                  id,
		"client_id_issued_at":        issued.Unix(),
		"client_name":                c.Name,
		"redirect_uris":              c.RedirectURIs,
		"token_endpoint_auth_method": method,
		"grant_types":                []string{grantCode, grantRefresh},
		"response_types":             []string{"code"},
		"scope":                      mcpScope,
	}
	if method != authMethodNone {
		out["client_secret"] = h.oauth.clientSecret(id)
		out["client_secret_expires_at"] = 0
	}
	h.log.Info("an MCP client registered", "client", c.Name, "redirect_uris", c.RedirectURIs)
	w.Header().Set("Cache-Control", "no-store")
	h.json(w, http.StatusCreated, out)
}

func registrationError(w http.ResponseWriter, h *handlers, code, description string) {
	h.json(w, http.StatusBadRequest, map[string]string{"error": code, "error_description": description})
}

// redirectURIProblem says what is wrong with a redirect URI a client wants to
// register, or "". A code goes wherever this points, so it has to be a place
// only the client controls: an https URL, or a port on the client's own
// machine (RFC 8252), which is how a local client like Claude Code receives
// one. Custom schemes aren't taken -- any app on a machine can claim one.
func redirectURIProblem(uri string) string {
	if len(uri) > maxRedirectURILen {
		return "a redirect_uri is too long"
	}
	u, err := url.Parse(uri)
	switch {
	case err != nil || !u.IsAbs() || u.Host == "":
		return "each redirect_uri must be an absolute URL"
	case u.Fragment != "" || strings.Contains(uri, "#"):
		return "a redirect_uri can't have a fragment"
	case u.User != nil:
		return "a redirect_uri can't carry credentials"
	case u.Scheme == "https":
		return ""
	case u.Scheme == "http" && isLoopback(u.Hostname()):
		return ""
	}
	return "each redirect_uri must be https, or http on localhost"
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// clientName is the name a client gave, made fit to show on the consent
// page: printable, one line, short, and with no invisible formatting -- a
// right-to-left override could make one name read as another.
func clientName(name string) string {
	name = strings.Map(func(r rune) rune {
		switch {
		case unicode.IsControl(r):
			return ' '
		case unicode.Is(unicode.Cf, r):
			return -1
		}
		return r
	}, name)
	name = strings.Join(strings.Fields(name), " ")
	if len([]rune(name)) > maxClientNameLen {
		name = string([]rune(name)[:maxClientNameLen]) + "…"
	}
	if name == "" {
		return defaultAppName
	}
	return name
}

// redirectMatches says whether uri is one the client registered. A loopback
// URI matches on everything but its port, as RFC 8252 asks: a local client
// takes whatever port is free when it runs.
func (c oauthClient) redirectMatches(uri string) bool {
	if slices.Contains(c.RedirectURIs, uri) {
		return true
	}
	got, err := url.Parse(uri)
	if err != nil || got.Scheme != "http" || !isLoopback(got.Hostname()) {
		return false
	}
	for _, registered := range c.RedirectURIs {
		want, err := url.Parse(registered)
		if err == nil && want.Scheme == "http" && want.Hostname() == got.Hostname() &&
			want.Path == got.Path && want.RawQuery == got.RawQuery {
			return true
		}
	}
	return false
}

// --- authorization ---

// authorize is the authorization endpoint, which a client opens in the
// person's browser. It checks what it can without anyone signed in, then
// hands the request to the consent page, signed so the page can't be fed
// one this didn't check.
//
// Until the client and its redirect URI check out, a failure is shown on the
// consent page rather than sent anywhere: the redirect URI isn't yet known to
// be the client's. After that, failures go back to the client (RFC 6749
// 4.1.2.1).
func (h *handlers) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	now := h.now()
	c, err := h.oauth.client(q.Get("client_id"), now)
	if err != nil {
		http.Redirect(w, r, connectPage+"?error=unknown-client", http.StatusFound)
		return
	}
	redirectURI := q.Get("redirect_uri")
	if redirectURI == "" && len(c.RedirectURIs) == 1 {
		redirectURI = c.RedirectURIs[0]
	}
	if !c.redirectMatches(redirectURI) {
		http.Redirect(w, r, connectPage+"?error=redirect-uri", http.StatusFound)
		return
	}

	state := q.Get("state")
	refuse := func(code, description string) {
		http.Redirect(w, r, h.oauth.redirectWith(redirectURI, url.Values{
			"error": {code}, "error_description": {description}, "state": {state},
		}), http.StatusFound)
	}
	switch {
	case q.Get("response_type") != "code":
		refuse("unsupported_response_type", "response_type must be code")
		return
	case q.Get("code_challenge") == "" || q.Get("code_challenge_method") != "S256":
		refuse("invalid_request", "PKCE is required, with code_challenge_method S256")
		return
	case !h.oauth.isResource(q.Get("resource")):
		refuse("invalid_target", "the only resource here is "+h.oauth.resource)
		return
	}
	// Scope isn't checked: there is one, and whatever a client asks for, the
	// grant is that one.

	request := signJSON(h.oauth.requests, oauthRequest{
		ClientID: q.Get("client_id"), RedirectURI: redirectURI,
		Challenge: q.Get("code_challenge"), State: state,
	}, now.Add(oauthRequestLife))
	http.Redirect(w, r, connectPage+"?request="+url.QueryEscape(request), http.StatusFound)
}

// isResource says whether a client's resource parameter (RFC 8707) names the
// MCP endpoint. A client that sends none gets a token for it anyway: it is
// the only resource there is.
func (o *oauthServer) isResource(resource string) bool {
	return resource == "" || strings.TrimSuffix(resource, "/") == o.resource
}

// redirectWith adds parameters to a client's redirect URI, iss among them.
func (o *oauthServer) redirectWith(redirectURI string, params url.Values) string {
	u, err := url.Parse(redirectURI)
	if err != nil {
		// Registration parsed it already.
		return redirectURI
	}
	q := u.Query()
	for k, vs := range params {
		if len(vs) > 0 && vs[0] != "" {
			q.Set(k, vs[0])
		}
	}
	q.Set("iss", o.issuer)
	u.RawQuery = q.Encode()
	return u.String()
}

// pendingRequest reads back the request the consent page was given.
func (h *handlers) pendingRequest(signed string) (oauthRequest, oauthClient, bool) {
	var req oauthRequest
	now := h.now()
	if err := verifyJSON(h.oauth.requests, signed, now, &req); err != nil {
		return req, oauthClient{}, false
	}
	c, err := h.oauth.client(req.ClientID, now)
	return req, c, err == nil
}

const msgConnectExpired = "That request has expired. Go back to the app you were connecting and start again."

// describeRequest is what the consent page shows: which app is asking, and
// where it will send them back to.
func (h *handlers) describeRequest(w http.ResponseWriter, r *http.Request, me db.User) {
	req, c, ok := h.pendingRequest(r.URL.Query().Get("request"))
	if !ok {
		h.problem(w, http.StatusBadRequest, msgConnectExpired)
		return
	}
	host := req.RedirectURI
	if u, err := url.Parse(req.RedirectURI); err == nil {
		host = u.Host
	}
	h.json(w, http.StatusOK, map[string]any{
		"client":       c.Name,
		"redirectHost": host,
		"user":         personOf(me),
	})
}

// decideRequest is the consent page's answer: a code if they pressed Allow,
// access_denied if they didn't, either way as the URL to send the browser to.
// It answers with JSON rather than a redirect because a fetch made it; the
// page does the navigating.
//
// This needs the session cookie and a JSON body, so another site can't press
// Allow for someone: the cookie is SameSite=Lax, which a cross-site POST
// doesn't carry.
func (h *handlers) decideRequest(w http.ResponseWriter, r *http.Request, me db.User) {
	var body struct {
		Request string `json:"request"`
		Approve bool   `json:"approve"`
	}
	if err := decode(r, &body); err != nil {
		h.problem(w, http.StatusBadRequest, err.Error())
		return
	}
	req, c, ok := h.pendingRequest(body.Request)
	if !ok {
		h.problem(w, http.StatusBadRequest, msgConnectExpired)
		return
	}
	if !body.Approve {
		h.log.Info("an MCP client was refused", "client", c.Name, "email", me.Email)
		h.json(w, http.StatusOK, map[string]string{"redirect": h.oauth.redirectWith(req.RedirectURI, url.Values{
			"error": {"access_denied"}, "error_description": {"the request was declined"}, "state": {req.State},
		})})
		return
	}

	now := h.now()
	code := signJSON(h.oauth.codes, oauthCode{
		ID: randomToken(), ClientID: req.ClientID, RedirectURI: req.RedirectURI,
		Challenge: req.Challenge, Email: me.Email, Granted: now.Unix(),
	}, now.Add(oauthCodeLife))
	h.log.Info("an MCP client was connected", "client", c.Name, "email", me.Email)
	h.json(w, http.StatusOK, map[string]string{"redirect": h.oauth.redirectWith(req.RedirectURI, url.Values{
		"code": {code}, "state": {req.State},
	})})
}

// --- tokens ---

// tokenError is the token endpoint's error shape (RFC 6749 5.2).
func (h *handlers) tokenError(w http.ResponseWriter, status int, code, description string) {
	w.Header().Set("Cache-Control", "no-store")
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Basic realm="birdsense"`)
	}
	h.json(w, status, map[string]string{"error": code, "error_description": description})
}

// token is the token endpoint: a code, or a refresh token, for an access
// token and a new refresh token.
func (h *handlers) token(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, oauthFormMaxSize)
	if err := r.ParseForm(); err != nil {
		h.tokenError(w, http.StatusBadRequest, "invalid_request", "the body must be a form")
		return
	}
	now := h.now()

	clientID, secret, basic := r.BasicAuth()
	if basic {
		// RFC 6749 2.3.1: Basic credentials are form-encoded first.
		clientID, _ = url.QueryUnescape(clientID)
		secret, _ = url.QueryUnescape(secret)
	} else {
		clientID, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	c, err := h.oauth.client(clientID, now)
	if err != nil {
		h.tokenError(w, http.StatusUnauthorized, "invalid_client", "that client isn't registered here")
		return
	}
	if c.AuthMethod != authMethodNone &&
		subtle.ConstantTimeCompare([]byte(secret), []byte(h.oauth.clientSecret(clientID))) != 1 {
		h.tokenError(w, http.StatusUnauthorized, "invalid_client", "the client secret is wrong")
		return
	}
	if !h.oauth.isResource(r.PostForm.Get("resource")) {
		h.tokenError(w, http.StatusBadRequest, "invalid_target", "the only resource here is "+h.oauth.resource)
		return
	}

	var grant oauthToken
	switch r.PostForm.Get("grant_type") {
	case grantCode:
		var code oauthCode
		if err := verifyJSON(h.oauth.codes, r.PostForm.Get("code"), now, &code); err != nil {
			h.tokenError(w, http.StatusBadRequest, "invalid_grant", "that code is unknown or has expired")
			return
		}
		switch {
		case code.ClientID != clientID:
			h.tokenError(w, http.StatusBadRequest, "invalid_grant", "that code was issued to another client")
			return
		case r.PostForm.Has("redirect_uri") && r.PostForm.Get("redirect_uri") != code.RedirectURI:
			h.tokenError(w, http.StatusBadRequest, "invalid_grant", "redirect_uri doesn't match the authorization request")
			return
		case !pkceMatches(r.PostForm.Get("code_verifier"), code.Challenge):
			h.tokenError(w, http.StatusBadRequest, "invalid_grant", "code_verifier doesn't match the code challenge")
			return
		case !h.oauth.spend(code.ID, now.Add(oauthCodeLife), now):
			h.log.Warn("an MCP authorization code was presented twice", "client", c.Name, "email", code.Email)
			h.tokenError(w, http.StatusBadRequest, "invalid_grant", "that code has been used")
			return
		}
		grant = oauthToken{ClientID: clientID, Email: code.Email, Granted: code.Granted}
	case grantRefresh:
		if err := verifyJSON(h.oauth.refresh, r.PostForm.Get("refresh_token"), now, &grant); err != nil {
			h.tokenError(w, http.StatusBadRequest, "invalid_grant", "that refresh token is unknown or has expired; connect again")
			return
		}
		if grant.ClientID != clientID {
			h.tokenError(w, http.StatusBadRequest, "invalid_grant", "that refresh token was issued to another client")
			return
		}
	case "":
		h.tokenError(w, http.StatusBadRequest, "invalid_request", "grant_type is required")
		return
	default:
		h.tokenError(w, http.StatusBadRequest, "unsupported_grant_type", "grant_type must be authorization_code or refresh_token")
		return
	}

	// The roster is the allow-list here too: a code or a refresh token in the
	// hands of someone since removed is worth nothing.
	if _, ok, err := h.onRoster(r, grant.Email); err != nil {
		h.fail(w, r, err)
		return
	} else if !ok {
		h.tokenError(w, http.StatusBadRequest, "invalid_grant", "that address is no longer on the roster")
		return
	}

	grantEnds := time.Unix(grant.Granted, 0).Add(oauthGrantLife)
	accessEnds := now.Add(oauthAccessLife)
	if accessEnds.After(grantEnds) {
		accessEnds = grantEnds
	}
	access := grant
	access.Expires, access.Audience = accessEnds.Unix(), h.oauth.resource
	refresh := grant
	refresh.Expires, refresh.Audience = grantEnds.Unix(), ""

	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	h.json(w, http.StatusOK, map[string]any{
		"access_token":  signJSON(h.oauth.access, access, accessEnds),
		"token_type":    "Bearer",
		"expires_in":    int(accessEnds.Sub(now) / time.Second),
		"refresh_token": signJSON(h.oauth.refresh, refresh, grantEnds),
		"scope":         mcpScope,
	})
}

// pkceMatches checks a code verifier against the S256 challenge it was made
// from (RFC 7636).
func pkceMatches(verifier, challenge string) bool {
	if len(verifier) < 43 || len(verifier) > 128 {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	return subtle.ConstantTimeCompare([]byte(base64.RawURLEncoding.EncodeToString(sum[:])), []byte(challenge)) == 1
}

// onRoster is someone on the roster by address, and whether they still are.
func (h *handlers) onRoster(r *http.Request, email string) (db.User, bool, error) {
	u, err := h.store.GetUserByEmail(r.Context(), email)
	switch {
	case errors.Is(err, db.ErrNotFound):
		return db.User{}, false, nil
	case err != nil:
		return db.User{}, false, err
	}
	return u, u.RemovedAt == nil, nil
}

// verifyAccess is the MCP endpoint's token check: signed by us, unexpired,
// for this resource, and held by someone still on the roster.
func (h *handlers) verifyAccess(r *http.Request, token string) (*auth.TokenInfo, error) {
	var t oauthToken
	if err := verifyJSON(h.oauth.access, token, h.now(), &t); err != nil || t.Audience != h.oauth.resource {
		return nil, auth.ErrInvalidToken
	}
	_, ok, err := h.onRoster(r, t.Email)
	switch {
	case err != nil:
		return nil, err
	case !ok:
		return nil, auth.ErrInvalidToken
	}
	return &auth.TokenInfo{
		Scopes: []string{mcpScope}, Expiration: time.Unix(t.Expires, 0), UserID: t.Email,
	}, nil
}
