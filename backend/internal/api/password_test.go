package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ngaitonde/EASBirdNet/backend/internal/db"
	"github.com/ngaitonde/EASBirdNet/backend/internal/email"
)

const testPassword = "a long enough password"

type mailbox chan email.Message

func (m mailbox) Send(_ context.Context, msg email.Message) error {
	m <- msg
	return nil
}

type passwordServer struct {
	*http.ServeMux
	h     *handlers
	store db.Store
	mail  mailbox
	clock *time.Time
}

// newPasswordServer is a test server with mail, and so password sign-in, on.
// The clock is the real time for the same reason oauthServerFor's is.
func newPasswordServer(t *testing.T) *passwordServer {
	t.Helper()
	store := newTestStore(t)
	clock := time.Now().UTC().Truncate(time.Second)
	keys := newKeyset(testSessionKey)
	mail := make(mailbox, 16)
	h := &handlers{
		store: store, files: testFiles(t), log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		keys: keys, now: func() time.Time { return clock },
		oauth: newOAuthServer(testPublicURL, keys),
		mail:  mail, publicURL: testPublicURL,
	}
	mux := http.NewServeMux()
	register(mux, h)
	return &passwordServer{ServeMux: mux, h: h, store: store, mail: mail, clock: &clock}
}

func jsonBody(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// settle waits for link requests in hand to finish, so a test can say that no
// mail went out.
func (s *passwordServer) settle(t *testing.T) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); len(s.h.passwords.mailer) > 0; {
		if time.Now().After(deadline) {
			t.Fatal("a password link is still in hand after 5 s")
		}
		time.Sleep(time.Millisecond)
	}
}

func (s *passwordServer) requestLink(t *testing.T, address string) {
	t.Helper()
	rec := do(t, s.ServeMux, http.MethodPost, "/api/v1/auth/password/link", jsonBody(t, map[string]string{"email": address}), nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("request a link = %d, want 202 (%s)", rec.Code, rec.Body)
	}
	s.settle(t)
}

var linkInMail = regexp.MustCompile(`https://\S+`)

// linkToken asks for a link for address, and reads the token out of the mail.
func (s *passwordServer) linkToken(t *testing.T, address string) string {
	t.Helper()
	s.requestLink(t, address)
	select {
	case m := <-s.mail:
		if m.To != address {
			t.Fatalf("mail went to %s, not %s", m.To, address)
		}
		link, err := url.Parse(linkInMail.FindString(m.Text))
		if err != nil || link.Path != "/signin/password" || !strings.HasPrefix(link.String(), testPublicURL) {
			t.Fatalf("no good link in the mail: %q", m.Text)
		}
		if !strings.Contains(m.HTML, `<a href="`+strings.ReplaceAll(link.String(), "&", "&amp;")+`"`) {
			t.Fatalf("the HTML doesn't carry the same link: %q", m.HTML)
		}
		return link.Query().Get("token")
	default:
		t.Fatalf("no mail for %s", address)
		return ""
	}
}

func (s *passwordServer) set(t *testing.T, token, password string) *httptest.ResponseRecorder {
	t.Helper()
	return do(t, s.ServeMux, http.MethodPut, "/api/v1/auth/password", jsonBody(t, map[string]string{"token": token, "password": password}), nil)
}

func (s *passwordServer) signIn(t *testing.T, address, password string) *httptest.ResponseRecorder {
	t.Helper()
	return do(t, s.ServeMux, http.MethodPost, "/api/v1/auth/password", jsonBody(t, map[string]string{"email": address, "password": password}), nil)
}

// givePassword takes someone through a link, and returns the session it set.
func (s *passwordServer) givePassword(t *testing.T, address, password string) *http.Cookie {
	t.Helper()
	rec := s.set(t, s.linkToken(t, address), password)
	if rec.Code != http.StatusOK {
		t.Fatalf("set a password = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	c := sessionFrom(rec)
	if c == nil {
		t.Fatal("setting a password set no session")
	}
	return c
}

func TestPasswordFirstTime(t *testing.T) {
	s := newPasswordServer(t)
	const address = "jane@example.com"

	if rec := s.signIn(t, address, testPassword); rec.Code != http.StatusUnauthorized {
		t.Fatalf("sign in before a password is set = %d, want 401", rec.Code)
	}

	token := s.linkToken(t, address)
	rec := do(t, s.ServeMux, http.MethodGet, "/api/v1/auth/password/link?token="+url.QueryEscape(token), "", nil)
	opened := decodeInto[struct {
		User  Person `json:"user"`
		First bool   `json:"first"`
	}](t, rec)
	if rec.Code != http.StatusOK || opened.User.Email != address || !opened.First {
		t.Fatalf("open the link = %d %+v", rec.Code, opened)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Error("what a link says about someone shouldn't be cached")
	}

	rec = s.set(t, token, testPassword)
	if rec.Code != http.StatusOK || sessionFrom(rec) == nil {
		t.Fatalf("set = %d (%s), want 200 and a session", rec.Code, rec.Body)
	}

	// The sign-in form works from here on, in whatever case the address is typed.
	rec = s.signIn(t, " JANE@Example.com ", testPassword)
	if rec.Code != http.StatusOK {
		t.Fatalf("sign in = %d (%s)", rec.Code, rec.Body)
	}
	c := sessionFrom(rec)
	if c == nil {
		t.Fatal("signing in set no session")
	}
	session := decodeInto[struct {
		User     Person `json:"user"`
		Password bool   `json:"password"`
	}](t, do(t, s.ServeMux, http.MethodGet, "/api/v1/session", "", c))
	if session.User.Email != address || session.User.Provider != "Password" || !session.Password {
		t.Errorf("session = %+v", session)
	}

	// A hash is all that is stored.
	stored, err := s.store.GetCredential(t.Context(), userByEmail(t, s.store, address).ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored.PasswordHash, testPassword) || !strings.HasPrefix(stored.PasswordHash, "$argon2id$") {
		t.Errorf("stored %q", stored.PasswordHash)
	}
}

// An admin's link says so, because the page warns them on the strength of it:
// and only a link says so, since a stranger can't be told who is an admin.
func TestPasswordLinkSaysWhoItIsFor(t *testing.T) {
	s := newPasswordServer(t)
	token := s.linkToken(t, "dana@eastsideaudubon.org")
	rec := do(t, s.ServeMux, http.MethodGet, "/api/v1/auth/password/link?token="+url.QueryEscape(token), "", nil)
	if got := decodeInto[struct {
		User Person `json:"user"`
	}](t, rec); got.User.Role != db.RoleAdmin {
		t.Errorf("role = %q, want admin", got.User.Role)
	}
	wrong := do(t, s.ServeMux, http.MethodGet, "/api/v1/auth/password/link?token=nonsense", "", nil)
	if wrong.Code != http.StatusGone || strings.Contains(wrong.Body.String(), "role") {
		t.Errorf("a bad token = %d %s", wrong.Code, wrong.Body)
	}
}

func TestPasswordSignInRefusalsAreAlike(t *testing.T) {
	s := newPasswordServer(t)
	s.givePassword(t, "jane@example.com", testPassword)
	s.givePassword(t, "m.lee@example.com", testPassword)
	marcus := userByEmail(t, s.store, "m.lee@example.com")
	rec := do(t, s.ServeMux, http.MethodDelete, "/api/v1/admin/people/"+marcus.ID, "", sessionAt("dana@eastsideaudubon.org", *s.clock))
	if rec.Code != http.StatusOK {
		t.Fatalf("remove = %d (%s)", rec.Code, rec.Body)
	}

	want := s.signIn(t, "jane@example.com", "not the password at all")
	if want.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password = %d, want 401", want.Code)
	}
	for what, address := range map[string]string{
		"not on the roster":     "nobody@example.com",
		"has no password":       "ellen.park@example.com",
		"removed":               "m.lee@example.com",
		"an address of nothing": "",
	} {
		rec := s.signIn(t, address, testPassword)
		if rec.Code != want.Code || rec.Body.String() != want.Body.String() || sessionFrom(rec) != nil {
			t.Errorf("%s = %d %s, want the wrong password's %d %s", what, rec.Code, rec.Body, want.Code, want.Body)
		}
	}
}

func TestPasswordLinkOnlyGoesToTheRoster(t *testing.T) {
	s := newPasswordServer(t)
	marcus := userByEmail(t, s.store, "m.lee@example.com")
	removed := time.Now()
	if _, err := s.store.UpdateUser(t.Context(), marcus.ID, func(u *db.User) error { u.RemovedAt = &removed; return nil }); err != nil {
		t.Fatal(err)
	}
	s.requestLink(t, "nobody@example.com")
	s.requestLink(t, "m.lee@example.com")
	select {
	case m := <-s.mail:
		t.Errorf("mail went to %s", m.To)
	default:
	}
	rec := do(t, s.ServeMux, http.MethodPost, "/api/v1/auth/password/link", `{"email":"no at sign"}`, nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("not an address = %d, want 400", rec.Code)
	}
}

func TestPasswordLinkIsGoodOnce(t *testing.T) {
	s := newPasswordServer(t)
	const address = "jane@example.com"
	first := s.linkToken(t, address)
	second := s.linkToken(t, address)
	if rec := s.set(t, first, testPassword); rec.Code != http.StatusOK {
		t.Fatalf("set = %d (%s)", rec.Code, rec.Body)
	}
	for what, token := range map[string]string{"used again": first, "issued beside it": second} {
		if rec := s.set(t, token, "another good password"); rec.Code != http.StatusGone {
			t.Errorf("a link %s = %d, want 410", what, rec.Code)
		}
		// The page says so on opening it, rather than after they've typed one.
		if rec := do(t, s.ServeMux, http.MethodGet, "/api/v1/auth/password/link?token="+url.QueryEscape(token), "", nil); rec.Code != http.StatusGone {
			t.Errorf("opening a link %s = %d, want 410", what, rec.Code)
		}
	}
	if rec := s.signIn(t, address, testPassword); rec.Code != http.StatusOK {
		t.Errorf("the first password no longer works: %d", rec.Code)
	}

	// A later link replaces the password, and then the old one is no good.
	if rec := s.set(t, s.linkToken(t, address), "another good password"); rec.Code != http.StatusOK {
		t.Fatalf("reset = %d (%s)", rec.Code, rec.Body)
	}
	if rec := s.signIn(t, address, testPassword); rec.Code != http.StatusUnauthorized {
		t.Errorf("the old password after a reset = %d, want 401", rec.Code)
	}
}

func TestPasswordLinkGoesBad(t *testing.T) {
	s := newPasswordServer(t)
	const address = "jane@example.com"
	jane := userByEmail(t, s.store, address)
	token := s.linkToken(t, address)

	if rec := s.set(t, token+"x", testPassword); rec.Code != http.StatusGone {
		t.Errorf("an edited link = %d, want 410", rec.Code)
	}
	// Signed by the session key itself, rather than the link's own derived key.
	forged := signJSON(s.h.keys, passwordLink{UserID: jane.ID, Email: address}, s.clock.Add(time.Hour))
	if rec := s.set(t, forged, testPassword); rec.Code != http.StatusGone {
		t.Errorf("a link signed under another key = %d, want 410", rec.Code)
	}

	// Moved to another address: the link went to the old one.
	moved, err := s.store.UpdateUser(t.Context(), jane.ID, func(u *db.User) error { u.Email = "jane@elsewhere.example"; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if rec := s.set(t, token, testPassword); rec.Code != http.StatusGone {
		t.Errorf("a link to an address they've moved from = %d, want 410", rec.Code)
	}

	// Out of time.
	late := s.linkToken(t, moved.Email)
	*s.clock = s.clock.Add(passwordLinkLife + time.Second)
	if rec := s.set(t, late, testPassword); rec.Code != http.StatusGone {
		t.Errorf("an expired link = %d, want 410", rec.Code)
	}
}

func TestPasswordRules(t *testing.T) {
	s := newPasswordServer(t)
	const address = "jane@example.com"
	token := s.linkToken(t, address)
	for what, password := range map[string]string{
		"too short":   "eleven char",
		"too long":    strings.Repeat("x", maxPasswordBytes+1),
		"the address": "JANE@example.com ",
	} {
		if rec := s.set(t, token, password); rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", what, rec.Code)
		}
	}
	// None of those spent the link, and twelve characters are enough, counted
	// as characters rather than bytes.
	if rec := s.set(t, token, "ééééééééééé1"); rec.Code != http.StatusOK {
		t.Errorf("twelve characters = %d (%s)", rec.Code, rec.Body)
	}
}

// A reset is often because someone else may have the password, so everything
// begun before it stops working, and this browser's new session doesn't.
func TestSettingAPasswordEndsWhatCameBefore(t *testing.T) {
	s := newPasswordServer(t)
	const address = "jane@example.com"
	before := sessionAt(address, *s.clock)
	whoAmI := func(c *http.Cookie) string {
		t.Helper()
		got := decodeInto[struct {
			User *Person `json:"user"`
		}](t, do(t, s.ServeMux, http.MethodGet, "/api/v1/session", "", c))
		if got.User == nil {
			return ""
		}
		return got.User.Email
	}
	if whoAmI(before) != address {
		t.Fatal("the old session didn't work to begin with")
	}

	client := registerTestClient(t, s.ServeMux, authMethodNone)
	code, verifier := connectAs(t, s.ServeMux, *s.clock, client.ClientID, address)
	rec, connected := exchange(t, s.ServeMux, client.ClientID, code, verifier)
	if rec.Code != http.StatusOK {
		t.Fatalf("connect = %d (%s)", rec.Code, rec.Body)
	}
	refresh := func() int {
		t.Helper()
		return postForm(t, s.ServeMux, oauthTokenPath, url.Values{"grant_type": {grantRefresh}, "refresh_token": {connected.RefreshToken}, "client_id": {client.ClientID}}).Code
	}
	access := func() int {
		t.Helper()
		return mcpCall(t, s.ServeMux, connected.AccessToken, "tools/list", `{}`).Code
	}
	if refresh() != http.StatusOK || access() != http.StatusOK {
		t.Fatal("the connection didn't work to begin with")
	}
	export := exportLink{Email: address, Issued: s.clock.Unix()}
	exportURL := "/api/v1/mcp/export?token=" + url.QueryEscape(signJSON(s.h.oauth.exports, export, s.clock.Add(exportLinkLife)))

	*s.clock = s.clock.Add(time.Minute)
	after := s.givePassword(t, address, testPassword)

	if got := whoAmI(before); got != "" {
		t.Errorf("the session from before is still signed in as %q", got)
	}
	if got := whoAmI(after); got != address {
		t.Errorf("the new session is %q", got)
	}
	if got := refresh(); got != http.StatusBadRequest {
		t.Errorf("refreshing the connection from before = %d, want 400", got)
	}
	if got := access(); got != http.StatusUnauthorized {
		t.Errorf("the access token from before = %d, want 401", got)
	}
	if rec := do(t, s.ServeMux, http.MethodGet, exportURL, "", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("the export link from before = %d, want 401", rec.Code)
	}

	// Someone else, signed in the whole time, is none the wiser.
	if whoAmI(sessionAt("ellen.park@example.com", s.clock.Add(-time.Hour))) != "ellen.park@example.com" {
		t.Error("another person's session ended too")
	}
}

func TestRemovingSomeoneEndsTheirPasswordForGood(t *testing.T) {
	s := newPasswordServer(t)
	const address = "jane@example.com"
	jane := userByEmail(t, s.store, address)
	session := s.givePassword(t, address, testPassword)
	admin := sessionAt("dana@eastsideaudubon.org", *s.clock)

	*s.clock = s.clock.Add(time.Minute)
	if rec := do(t, s.ServeMux, http.MethodDelete, "/api/v1/admin/people/"+jane.ID, "", admin); rec.Code != http.StatusOK {
		t.Fatalf("remove = %d (%s)", rec.Code, rec.Body)
	}
	*s.clock = s.clock.Add(time.Minute)
	rec := do(t, s.ServeMux, http.MethodPost, "/api/v1/admin/people", jsonBody(t, map[string]any{
		"name": "Jane Volunteer", "email": address, "role": "volunteer", "roles": []string{db.RoleReviewer},
	}), sessionAt("dana@eastsideaudubon.org", *s.clock))
	if rec.Code != http.StatusCreated {
		t.Fatalf("add back = %d (%s)", rec.Code, rec.Body)
	}

	if rec := s.signIn(t, address, testPassword); rec.Code != http.StatusUnauthorized {
		t.Errorf("the old password = %d, want 401", rec.Code)
	}
	if rec := do(t, s.ServeMux, http.MethodGet, "/api/v1/stations", "", session); rec.Code != http.StatusUnauthorized {
		t.Errorf("the old session = %d, want 401", rec.Code)
	}
}

func TestPasswordSignInIsLimited(t *testing.T) {
	s := newPasswordServer(t)
	const address = "jane@example.com"
	s.givePassword(t, address, testPassword)

	// From many clients, as a guess at one person's password would be.
	try := func(client, password string) int {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/password", strings.NewReader(jsonBody(t, map[string]string{"email": address, "password": password})))
		req.Header.Set("X-Forwarded-For", client)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		return rec.Code
	}
	for i := range 10 {
		if got := try(fmt.Sprintf("203.0.113.%d", i), "not the password at all"); got != http.StatusUnauthorized {
			t.Fatalf("wrong guess %d = %d, want 401", i, got)
		}
	}
	if got := try("203.0.113.200", testPassword); got != http.StatusTooManyRequests {
		t.Errorf("the right password once locked = %d, want 429", got)
	}
	// Only passwords are locked, and a link is the way back.
	if rec := s.set(t, s.linkToken(t, address), "another good password"); rec.Code != http.StatusOK {
		t.Fatalf("set = %d (%s)", rec.Code, rec.Body)
	}
	if got := try("203.0.113.201", "another good password"); got != http.StatusOK {
		t.Errorf("after setting a new one = %d, want 200", got)
	}

	// And it lapses by itself.
	for range 10 {
		try("203.0.113.202", "not the password at all")
	}
	*s.clock = s.clock.Add(16 * time.Minute)
	if got := try("203.0.113.202", "another good password"); got != http.StatusOK {
		t.Errorf("after the window = %d, want 200", got)
	}
}

func TestOneClientCantGuessAtEveryone(t *testing.T) {
	s := newPasswordServer(t)
	limited := false
	for i := range 51 {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/password", strings.NewReader(jsonBody(t, map[string]string{"email": fmt.Sprintf("p%d@example.com", i), "password": "x"})))
		req.Header.Set("X-Forwarded-For", "203.0.113.9")
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		limited = rec.Code == http.StatusTooManyRequests
	}
	if !limited {
		t.Error("the 51st attempt from one client wasn't limited")
	}
}

func TestPasswordLinksAreLimited(t *testing.T) {
	s := newPasswordServer(t)
	const address = "jane@example.com"
	for range 5 {
		s.requestLink(t, address) // answers the same every time
	}
	if got := len(s.mail); got != 3 {
		t.Errorf("%d mails in a row, want 3", got)
	}
	for len(s.mail) > 0 {
		<-s.mail
	}
	*s.clock = s.clock.Add(time.Hour + time.Second)
	s.requestLink(t, address)
	if got := len(s.mail); got != 1 {
		t.Errorf("%d mails an hour on, want 1", got)
	}
}

func TestPasswordRoutesRefuseOtherSites(t *testing.T) {
	s := newPasswordServer(t)
	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/api/v1/auth/password"},
		{http.MethodPost, "/api/v1/auth/password/link"},
		{http.MethodPut, "/api/v1/auth/password"},
	} {
		req := httptest.NewRequest(c.method, c.path, strings.NewReader(`{}`))
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s from another site = %d, want 403", c.method, c.path, rec.Code)
		}
	}
}

func TestNoMailNoPasswordSignIn(t *testing.T) {
	mux, _ := newTestMux(t)
	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/api/v1/auth/password"},
		{http.MethodPost, "/api/v1/auth/password/link"},
		{http.MethodGet, "/api/v1/auth/password/link"},
		{http.MethodPut, "/api/v1/auth/password"},
	} {
		if rec := do(t, mux, c.method, c.path, `{}`, nil); rec.Code != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404", c.method, c.path, rec.Code)
		}
	}
	got := decodeInto[struct {
		Password *bool `json:"password"`
	}](t, do(t, mux, http.MethodGet, "/api/v1/session", "", nil))
	if got.Password == nil || *got.Password {
		t.Errorf("session says password = %v, want false", got.Password)
	}
}

func TestClientKey(t *testing.T) {
	for _, c := range []struct {
		what      string
		dev       bool
		forwarded []string
		remote    string
		want      string
	}{
		{"the ingress's entry", false, []string{"203.0.113.9"}, "10.0.0.1:443", "203.0.113.9"},
		{"not what the client wrote to its left", false, []string{"1.2.3.4, 203.0.113.9"}, "10.0.0.1:443", "203.0.113.9"},
		{"the last of several headers", false, []string{"1.2.3.4", "5.6.7.8, 203.0.113.9"}, "10.0.0.1:443", "203.0.113.9"},
		{"IPv6 by its /64", false, []string{"2001:db8:1:2:3:4:5:6"}, "10.0.0.1:443", "2001:db8:1:2::/64"},
		{"no header", false, nil, "198.51.100.7:5000", "198.51.100.7"},
		{"dev has no ingress to believe", true, []string{"203.0.113.9"}, "127.0.0.1:5000", "127.0.0.1"},
	} {
		h := &handlers{dev: c.dev}
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		r.RemoteAddr = c.remote
		for _, v := range c.forwarded {
			r.Header.Add("X-Forwarded-For", v)
		}
		if got := h.clientKey(r); got != c.want {
			t.Errorf("%s: %q, want %q", c.what, got, c.want)
		}
	}
}

func TestPasswordHash(t *testing.T) {
	stored := hashPassword(testPassword)
	if !passwordMatches(testPassword, stored) {
		t.Error("a password doesn't match its own hash")
	}
	if passwordMatches("something else", stored) {
		t.Error("another password matches")
	}
	if stored == hashPassword(testPassword) {
		t.Error("two hashes of one password are alike: no salt")
	}
	for _, bad := range []string{"", "plain", "$argon2id$v=19$m=1,t=0,p=1$AAAA$AAAA", "$argon2i$v=19$m=19456,t=2,p=1$AAAA$AAAA", "$argon2id$v=19$m=19456,t=2,p=1$!!$AAAA"} {
		if passwordMatches(testPassword, bad) {
			t.Errorf("matched the stored value %q", bad)
		}
	}
}

func TestLimiterStaysBounded(t *testing.T) {
	now := time.Unix(1000, 0)
	l := newLimiter(5, time.Minute)
	for i := range limiterKeys {
		if !l.hit(fmt.Sprint(i), now) {
			t.Fatalf("key %d refused", i)
		}
	}
	if l.hit("one more", now) {
		t.Error("a full limiter took another key")
	}
	if !l.hit("one more", now.Add(2*time.Minute)) {
		t.Error("a limiter whose keys have lapsed still refuses")
	}
	if len(l.keys) != 1 {
		t.Errorf("%d keys held, want the lapsed ones gone", len(l.keys))
	}
}
