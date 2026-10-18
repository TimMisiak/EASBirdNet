package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/ngaitonde/EASBirdNet/backend/internal/db"
)

// testPublicURL is where the test server says it is: the issuer, and the base
// of the MCP resource.
const testPublicURL = "https://birdsense.example"

const testRedirectURI = "https://client.example/callback"

// oauthServerFor is a test server with the MCP endpoint on, and the clock it
// reads. The clock starts at the real time rather than testNow, because the
// MCP SDK checks an access token's expiry against the real one.
func oauthServerFor(t *testing.T) (*http.ServeMux, db.Store, *time.Time) {
	t.Helper()
	store := newTestStore(t)
	clock := time.Now().UTC().Truncate(time.Second)
	keys := newKeyset(testSessionKey)
	h := &handlers{
		store: store, files: testFiles(t), log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		keys: keys, now: func() time.Time { return clock },
		oauth: newOAuthServer(testPublicURL, keys),
	}
	mux := http.NewServeMux()
	register(mux, h)
	return mux, store, &clock
}

// sessionAt is a session cookie for email that is good at now.
func sessionAt(email string, now time.Time) *http.Cookie {
	return &http.Cookie{Name: sessionCookie, Value: newKeyset(testSessionKey).sign(email, now.Add(sessionLife))}
}

func postForm(t *testing.T, mux *http.ServeMux, path string, form url.Values, basic ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if len(basic) == 2 {
		req.SetBasicAuth(url.QueryEscape(basic[0]), url.QueryEscape(basic[1]))
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

type registered struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	Method       string `json:"token_endpoint_auth_method"`
}

func registerTestClient(t *testing.T, mux *http.ServeMux, method string) registered {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"redirect_uris": []string{testRedirectURI}, "client_name": "Test client",
		"token_endpoint_auth_method": method, "logo_uri": "https://client.example/logo.png",
	})
	rec := do(t, mux, http.MethodPost, oauthRegisterPath, string(body), nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("register = %d, want 201 (%s)", rec.Code, rec.Body)
	}
	return decodeInto[registered](t, rec)
}

// authorizeURL is the authorization request a client opens in a browser.
func authorizeURL(clientID, challenge string, extra url.Values) string {
	q := url.Values{
		"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {testRedirectURI},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"}, "state": {"st4te"},
		"resource": {testPublicURL + mcpPath}, "scope": {mcpScope},
	}
	for k, v := range extra {
		q[k] = v
	}
	return oauthAuthorizePath + "?" + q.Encode()
}

// connectAs runs the browser's half: the authorization request, then Allow
// on the consent page as email. It returns the code the client is sent back
// with, and its PKCE verifier.
func connectAs(t *testing.T, mux *http.ServeMux, now time.Time, clientID, email string) (code, verifier string) {
	t.Helper()
	verifier = oauth2.GenerateVerifier()
	rec := do(t, mux, http.MethodGet, authorizeURL(clientID, oauth2.S256ChallengeFromVerifier(verifier), nil), "", nil)
	consent, _ := url.Parse(rec.Header().Get("Location"))
	if rec.Code != http.StatusFound || consent.Path != connectPage || consent.Query().Get("request") == "" {
		t.Fatalf("authorize = %d to %q, want a redirect to the consent page with a request", rec.Code, rec.Header().Get("Location"))
	}
	request := consent.Query().Get("request")

	rec = do(t, mux, http.MethodGet, "/api/v1/oauth/request?request="+url.QueryEscape(request), "", sessionAt(email, now))
	described := decodeInto[struct {
		Client       string `json:"client"`
		RedirectHost string `json:"redirectHost"`
	}](t, rec)
	if rec.Code != http.StatusOK || described.Client != "Test client" || described.RedirectHost != "client.example" {
		t.Fatalf("describe = %d %+v, want the client's name and where it goes back to", rec.Code, described)
	}

	body, _ := json.Marshal(map[string]any{"request": request, "approve": true})
	rec = do(t, mux, http.MethodPost, oauthAuthorizePath, string(body), sessionAt(email, now))
	if rec.Code != http.StatusOK {
		t.Fatalf("allow = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	back, _ := url.Parse(decodeInto[map[string]string](t, rec)["redirect"])
	q := back.Query()
	if back.Host != "client.example" || q.Get("state") != "st4te" || q.Get("iss") != testPublicURL || q.Get("code") == "" {
		t.Fatalf("allow sends the browser to %s, want the client's redirect URI with a code, its state and iss", back)
	}
	return q.Get("code"), verifier
}

type tokens struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Error        string `json:"error"`
}

func exchange(t *testing.T, mux *http.ServeMux, clientID, code, verifier string) (*httptest.ResponseRecorder, tokens) {
	t.Helper()
	rec := postForm(t, mux, oauthTokenPath, url.Values{
		"grant_type": {grantCode}, "code": {code}, "redirect_uri": {testRedirectURI},
		"client_id": {clientID}, "code_verifier": {verifier}, "resource": {testPublicURL + mcpPath},
	})
	var tok tokens
	json.Unmarshal(rec.Body.Bytes(), &tok)
	return rec, tok
}

// mcpCall posts one JSON-RPC request to the MCP endpoint with a bearer token.
func mcpCall(t *testing.T, mux *http.ServeMux, token, method, params string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"` + method + `"`
	if params != "" {
		body += `,"params":` + params
	}
	req := httptest.NewRequest(http.MethodPost, mcpPath, strings.NewReader(body+"}"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestOAuthMetadata(t *testing.T) {
	mux, _, _ := oauthServerFor(t)

	rec := do(t, mux, http.MethodGet, "/.well-known/oauth-authorization-server", "", nil)
	meta := decodeInto[map[string]any](t, rec)
	if rec.Code != http.StatusOK || meta["issuer"] != testPublicURL ||
		meta["authorization_endpoint"] != testPublicURL+oauthAuthorizePath ||
		meta["token_endpoint"] != testPublicURL+oauthTokenPath ||
		meta["registration_endpoint"] != testPublicURL+oauthRegisterPath {
		t.Errorf("authorization server metadata = %d %v, want every endpoint under the public URL", rec.Code, meta)
	}
	if methods, _ := meta["code_challenge_methods_supported"].([]any); len(methods) != 1 || methods[0] != "S256" {
		t.Errorf("code_challenge_methods_supported = %v, want S256 alone", meta["code_challenge_methods_supported"])
	}

	// Clients look for the resource's metadata at its own path and at the
	// root; both answer.
	for _, path := range []string{"/.well-known/oauth-protected-resource" + mcpPath, "/.well-known/oauth-protected-resource"} {
		rec := do(t, mux, http.MethodGet, path, "", nil)
		prm := decodeInto[map[string]any](t, rec)
		servers, _ := prm["authorization_servers"].([]any)
		if rec.Code != http.StatusOK || prm["resource"] != testPublicURL+mcpPath || len(servers) != 1 || servers[0] != testPublicURL {
			t.Errorf("GET %s = %d %v, want the MCP resource and this server as its authorization server", path, rec.Code, prm)
		}
	}

	// Without a public URL there is no issuer to be, so none of it exists.
	plain, _ := newTestMux(t)
	for _, path := range []string{"/.well-known/oauth-authorization-server", mcpPath, oauthTokenPath} {
		if rec := do(t, plain, http.MethodPost, path, "", nil); rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("without a public URL, %s = %d, want it unregistered", path, rec.Code)
		}
	}
}

func TestMCPEndpointNeedsAToken(t *testing.T) {
	mux, _, _ := oauthServerFor(t)
	rec := mcpCall(t, mux, "", "tools/list", "")
	challenge := rec.Header().Get("WWW-Authenticate")
	if rec.Code != http.StatusUnauthorized || !strings.Contains(challenge, `resource_metadata="`+testPublicURL+"/.well-known/oauth-protected-resource"+mcpPath+`"`) {
		t.Errorf("MCP without a token = %d, WWW-Authenticate %q; want 401 naming the resource metadata", rec.Code, challenge)
	}
	if rec := mcpCall(t, mux, "not-a-token", "tools/list", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("MCP with a made-up token = %d, want 401", rec.Code)
	}
}

func TestRegisterClient(t *testing.T) {
	mux, _, _ := oauthServerFor(t)

	none := registerTestClient(t, mux, authMethodNone)
	if none.ClientID == "" || none.ClientSecret != "" || none.Method != authMethodNone {
		t.Errorf("a public client = %+v, want a client_id and no secret", none)
	}
	// RFC 7591's default is a secret sent with Basic auth.
	basic := registerTestClient(t, mux, "")
	if basic.ClientSecret == "" || basic.Method != authMethodBasic {
		t.Errorf("a client naming no method = %+v, want client_secret_basic, with a secret", basic)
	}

	for _, tc := range []struct {
		name string
		body string
	}{
		{"no redirect", `{"client_name":"x"}`},
		{"plain http", `{"redirect_uris":["http://client.example/cb"]}`},
		{"custom scheme", `{"redirect_uris":["myapp://callback"]}`},
		{"fragment", `{"redirect_uris":["https://client.example/cb#x"]}`},
		{"relative", `{"redirect_uris":["/cb"]}`},
		{"credentials", `{"redirect_uris":["https://me:pw@client.example/cb"]}`},
		{"too many", `{"redirect_uris":["https://a.example/1","https://a.example/2","https://a.example/3","https://a.example/4","https://a.example/5","https://a.example/6"]}`},
		{"unknown method", `{"redirect_uris":["https://client.example/cb"],"token_endpoint_auth_method":"private_key_jwt"}`},
		{"implicit", `{"redirect_uris":["https://client.example/cb"],"response_types":["token"]}`},
		{"not JSON", `redirect_uris=x`},
	} {
		if rec := do(t, mux, http.MethodPost, oauthRegisterPath, tc.body, nil); rec.Code != http.StatusBadRequest {
			t.Errorf("register with %s = %d, want 400 (%s)", tc.name, rec.Code, rec.Body)
		}
	}

	// A local client's loopback redirect is fine, on whatever port it gets.
	loop := do(t, mux, http.MethodPost, oauthRegisterPath, `{"redirect_uris":["http://127.0.0.1:33418/callback"],"token_endpoint_auth_method":"none"}`, nil)
	if loop.Code != http.StatusCreated {
		t.Fatalf("register a loopback redirect = %d, want 201 (%s)", loop.Code, loop.Body)
	}
	id := decodeInto[registered](t, loop).ClientID
	rec := do(t, mux, http.MethodGet, authorizeURL(id, "challenge", url.Values{"redirect_uri": {"http://127.0.0.1:51000/callback"}}), "", nil)
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, connectPage+"?request=") {
		t.Errorf("authorize on another loopback port = %q, want the consent page", loc)
	}
}

func TestClientName(t *testing.T) {
	for in, want := range map[string]string{
		"":                       defaultAppName,
		"  Claude  ":             "Claude",
		"Evil\napp\u202e\x00now": "Evil app now",
		strings.Repeat("a", 150): strings.Repeat("a", maxClientNameLen) + "…",
	} {
		if got := clientName(in); got != want {
			t.Errorf("clientName(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestAuthorizeChecksTheClientFirst holds the order RFC 6749 asks for: until
// the client and its redirect URI check out, a bad request is shown on the
// consent page, because sending it to an unchecked redirect URI would make
// this an open redirect. After that, errors go back to the client.
func TestAuthorizeChecksTheClientFirst(t *testing.T) {
	mux, _, _ := oauthServerFor(t)
	c := registerTestClient(t, mux, authMethodNone)

	for _, tc := range []struct {
		name  string
		url   string
		error string
	}{
		{"made-up client", authorizeURL("not-a-client", "c", nil), "unknown-client"},
		{"unregistered redirect", authorizeURL(c.ClientID, "c", url.Values{"redirect_uri": {"https://attacker.example/cb"}}), "redirect-uri"},
	} {
		rec := do(t, mux, http.MethodGet, tc.url, "", nil)
		if loc := rec.Header().Get("Location"); rec.Code != http.StatusFound || loc != connectPage+"?error="+tc.error {
			t.Errorf("authorize with a %s = %d to %q, want the consent page saying %s", tc.name, rec.Code, loc, tc.error)
		}
	}

	for _, tc := range []struct {
		name  string
		extra url.Values
		error string
	}{
		{"implicit grant", url.Values{"response_type": {"token"}}, "unsupported_response_type"},
		{"plain PKCE", url.Values{"code_challenge_method": {"plain"}}, "invalid_request"},
		{"no PKCE", url.Values{"code_challenge": {""}}, "invalid_request"},
		{"another resource", url.Values{"resource": {"https://elsewhere.example/mcp"}}, "invalid_target"},
	} {
		rec := do(t, mux, http.MethodGet, authorizeURL(c.ClientID, "c", tc.extra), "", nil)
		back, _ := url.Parse(rec.Header().Get("Location"))
		q := back.Query()
		if back.Host != "client.example" || q.Get("error") != tc.error || q.Get("state") != "st4te" || q.Get("iss") != testPublicURL {
			t.Errorf("authorize with %s goes to %s, want the client told %s, with its state", tc.name, back, tc.error)
		}
	}

	// A registration with one redirect URI needn't repeat it.
	rec := do(t, mux, http.MethodGet, authorizeURL(c.ClientID, "c", url.Values{"redirect_uri": {""}}), "", nil)
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, connectPage+"?request=") {
		t.Errorf("authorize without a redirect_uri = %q, want the consent page", loc)
	}
}

func TestConsentNeedsASession(t *testing.T) {
	mux, _, clock := oauthServerFor(t)
	c := registerTestClient(t, mux, authMethodNone)
	rec := do(t, mux, http.MethodGet, authorizeURL(c.ClientID, "c", nil), "", nil)
	consent, _ := url.Parse(rec.Header().Get("Location"))
	request := consent.Query().Get("request")

	body, _ := json.Marshal(map[string]any{"request": request, "approve": true})
	if rec := do(t, mux, http.MethodPost, oauthAuthorizePath, string(body), nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("allow without a session = %d, want 401", rec.Code)
	}
	if rec := do(t, mux, http.MethodGet, "/api/v1/oauth/request?request="+url.QueryEscape(request), "", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("describe without a session = %d, want 401", rec.Code)
	}

	// Cancel goes back to the client too, as access_denied.
	body, _ = json.Marshal(map[string]any{"request": request, "approve": false})
	rec = do(t, mux, http.MethodPost, oauthAuthorizePath, string(body), sessionAt("jane@example.com", *clock))
	back, _ := url.Parse(decodeInto[map[string]string](t, rec)["redirect"])
	if back.Query().Get("error") != "access_denied" || back.Query().Get("code") != "" || back.Query().Get("state") != "st4te" {
		t.Errorf("cancel sends the browser to %s, want access_denied and no code", back)
	}

	// A request left on the page past its life has to be started again, and
	// one that was never signed here was never a request.
	*clock = clock.Add(oauthRequestLife + time.Second)
	body, _ = json.Marshal(map[string]any{"request": request, "approve": true})
	if rec := do(t, mux, http.MethodPost, oauthAuthorizePath, string(body), sessionAt("jane@example.com", *clock)); rec.Code != http.StatusBadRequest {
		t.Errorf("allow on an expired request = %d, want 400", rec.Code)
	}
	body, _ = json.Marshal(map[string]any{"request": c.ClientID, "approve": true})
	if rec := do(t, mux, http.MethodPost, oauthAuthorizePath, string(body), sessionAt("jane@example.com", *clock)); rec.Code != http.StatusBadRequest {
		t.Errorf("allow on a client_id passed off as a request = %d, want 400", rec.Code)
	}
}

func TestCodeForTokens(t *testing.T) {
	mux, _, clock := oauthServerFor(t)
	c := registerTestClient(t, mux, authMethodNone)
	other := registerTestClient(t, mux, authMethodNone)

	code, verifier := connectAs(t, mux, *clock, c.ClientID, "jane@example.com")
	if rec, _ := exchange(t, mux, c.ClientID, code, oauth2.GenerateVerifier()); rec.Code != http.StatusBadRequest {
		t.Errorf("a code with the wrong verifier = %d, want 400", rec.Code)
	}
	if rec, _ := exchange(t, mux, other.ClientID, code, verifier); rec.Code != http.StatusBadRequest {
		t.Errorf("a code swapped by another client = %d, want 400", rec.Code)
	}
	rec, tok := exchange(t, mux, c.ClientID, code, verifier)
	if rec.Code != http.StatusOK || tok.TokenType != "Bearer" || tok.AccessToken == "" || tok.RefreshToken == "" || tok.ExpiresIn != int(oauthAccessLife/time.Second) {
		t.Fatalf("exchange = %d %+v, want a bearer token for an hour and a refresh token", rec.Code, tok)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("token response Cache-Control = %q, want no-store", rec.Header().Get("Cache-Control"))
	}
	if rec, _ := exchange(t, mux, c.ClientID, code, verifier); rec.Code != http.StatusBadRequest {
		t.Errorf("a code used twice = %d, want 400", rec.Code)
	}
	if rec := mcpCall(t, mux, tok.AccessToken, "tools/list", ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "detection_summary") {
		t.Errorf("tools/list with the access token = %d, want the tools (%s)", rec.Code, rec.Body)
	}

	// A code is good for a minute.
	code, verifier = connectAs(t, mux, *clock, c.ClientID, "jane@example.com")
	*clock = clock.Add(oauthCodeLife + time.Second)
	if rec, _ := exchange(t, mux, c.ClientID, code, verifier); rec.Code != http.StatusBadRequest {
		t.Errorf("a code past its minute = %d, want 400", rec.Code)
	}

	for _, tc := range []struct {
		name string
		form url.Values
	}{
		{"no grant type", url.Values{"client_id": {c.ClientID}}},
		{"a password grant", url.Values{"client_id": {c.ClientID}, "grant_type": {"password"}}},
		{"no client", url.Values{"grant_type": {grantCode}}},
	} {
		if rec := postForm(t, mux, oauthTokenPath, tc.form); rec.Code < 400 || rec.Code > 401 {
			t.Errorf("token with %s = %d, want 400 or 401", tc.name, rec.Code)
		}
	}
}

func TestClientSecretIsChecked(t *testing.T) {
	mux, _, clock := oauthServerFor(t)
	c := registerTestClient(t, mux, authMethodBasic)
	code, verifier := connectAs(t, mux, *clock, c.ClientID, "jane@example.com")
	form := url.Values{"grant_type": {grantCode}, "code": {code}, "code_verifier": {verifier}, "redirect_uri": {testRedirectURI}}

	if rec := postForm(t, mux, oauthTokenPath, form, c.ClientID, "wrong"); rec.Code != http.StatusUnauthorized {
		t.Errorf("token with the wrong secret = %d, want 401", rec.Code)
	}
	withID := url.Values{"client_id": {c.ClientID}}
	for k, v := range form {
		withID[k] = v
	}
	if rec := postForm(t, mux, oauthTokenPath, withID); rec.Code != http.StatusUnauthorized {
		t.Errorf("token with no secret = %d, want 401", rec.Code)
	}
	if rec := postForm(t, mux, oauthTokenPath, form, c.ClientID, c.ClientSecret); rec.Code != http.StatusOK {
		t.Errorf("token with the secret = %d, want 200 (%s)", rec.Code, rec.Body)
	}
}

func TestRefresh(t *testing.T) {
	mux, store, clock := oauthServerFor(t)
	c := registerTestClient(t, mux, authMethodNone)
	other := registerTestClient(t, mux, authMethodNone)
	code, verifier := connectAs(t, mux, *clock, c.ClientID, "jane@example.com")
	_, first := exchange(t, mux, c.ClientID, code, verifier)

	refresh := func(clientID, token string) (*httptest.ResponseRecorder, tokens) {
		rec := postForm(t, mux, oauthTokenPath, url.Values{"grant_type": {grantRefresh}, "refresh_token": {token}, "client_id": {clientID}})
		var tok tokens
		json.Unmarshal(rec.Body.Bytes(), &tok)
		return rec, tok
	}

	// An hour on, the access token has gone and the refresh token gets another.
	*clock = clock.Add(oauthAccessLife + time.Minute)
	if rec := mcpCall(t, mux, first.AccessToken, "tools/list", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("an access token past its hour = %d, want 401", rec.Code)
	}
	if rec, _ := refresh(other.ClientID, first.RefreshToken); rec.Code != http.StatusBadRequest {
		t.Errorf("a refresh token used by another client = %d, want 400", rec.Code)
	}
	if rec, _ := refresh(c.ClientID, first.AccessToken); rec.Code != http.StatusBadRequest {
		t.Errorf("an access token passed off as a refresh token = %d, want 400", rec.Code)
	}
	rec, second := refresh(c.ClientID, first.RefreshToken)
	if rec.Code != http.StatusOK || second.AccessToken == "" || second.RefreshToken == "" {
		t.Fatalf("refresh = %d %+v, want new tokens", rec.Code, second)
	}

	// Refreshing doesn't extend the grant: it ends when the session would
	// have, and the last access token runs out with it.
	*clock = clock.Add(oauthGrantLife - oauthAccessLife - 30*time.Minute)
	rec, last := refresh(c.ClientID, second.RefreshToken)
	if rec.Code != http.StatusOK || last.ExpiresIn > int((30*time.Minute)/time.Second) {
		t.Errorf("a refresh half an hour before the grant ends = %d, expires_in %d; want an access token for that half hour", rec.Code, last.ExpiresIn)
	}
	*clock = clock.Add(time.Hour)
	if rec, _ := refresh(c.ClientID, last.RefreshToken); rec.Code != http.StatusBadRequest {
		t.Errorf("a refresh after the grant ended = %d, want 400", rec.Code)
	}

	// Taking someone off the roster ends their connections: no refresh, and
	// no use of an access token they already hold.
	*clock = time.Now().UTC().Truncate(time.Second)
	code, verifier = connectAs(t, mux, *clock, c.ClientID, "jane@example.com")
	_, live := exchange(t, mux, c.ClientID, code, verifier)
	jane := userByEmail(t, store, "jane@example.com")
	if _, err := store.UpdateUser(t.Context(), jane.ID, func(u *db.User) error { u.RemovedAt = clock; return nil }); err != nil {
		t.Fatal(err)
	}
	if rec := mcpCall(t, mux, live.AccessToken, "tools/list", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("an access token after its person was removed = %d, want 401", rec.Code)
	}
	if rec, _ := refresh(c.ClientID, live.RefreshToken); rec.Code != http.StatusBadRequest {
		t.Errorf("a refresh after its person was removed = %d, want 400", rec.Code)
	}
}

// TestTokensAreNotCookies holds the point of deriving a key per purpose: an
// access token can't be a session cookie, nor a cookie a token, though one
// key is behind both.
func TestTokensAreNotCookies(t *testing.T) {
	mux, _, clock := oauthServerFor(t)
	c := registerTestClient(t, mux, authMethodNone)
	code, verifier := connectAs(t, mux, *clock, c.ClientID, "dana@eastsideaudubon.org")
	_, tok := exchange(t, mux, c.ClientID, code, verifier)

	asCookie := &http.Cookie{Name: sessionCookie, Value: tok.AccessToken}
	if rec := do(t, mux, http.MethodGet, "/api/v1/admin/people", "", asCookie); rec.Code != http.StatusUnauthorized {
		t.Errorf("an access token as a session cookie = %d, want 401", rec.Code)
	}
	cookie := sessionAt("dana@eastsideaudubon.org", *clock)
	if rec := mcpCall(t, mux, cookie.Value, "tools/list", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("a session cookie as an access token = %d, want 401", rec.Code)
	}
	if rec := mcpCall(t, mux, c.ClientID, "tools/list", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("a client_id as an access token = %d, want 401", rec.Code)
	}
}

func TestLocalPath(t *testing.T) {
	for next, want := range map[string]bool{
		"/connect?request=abc":    true,
		"/app/detections?x=1":     true,
		"":                        false,
		"connect":                 false,
		"//evil.example/":         false,
		"/\\evil.example/":        false,
		"https://evil.example/":   false,
		"/app\n/x":                false,
		"javascript:alert(1)":     false,
		"/%2F%2Fevil.example/a/b": true, // a path on this site, oddly spelt
	} {
		if got := localPath(next); got != want {
			t.Errorf("localPath(%q) = %v, want %v", next, got, want)
		}
	}
}
