package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"

	"github.com/ngaitonde/EASBirdNet/backend/internal/db"
	"github.com/ngaitonde/EASBirdNet/backend/internal/email"
)

// Password sign-in, for someone who would rather not use Google or Microsoft.
// The roster is the allow-list here as well: nobody registers. An emailed link
// is the only way to set a password, first or forgotten, so holding the
// roster's address is proved before a password exists, and an address nobody
// on the roster has gets no mail and no different answer.
//
// What it ends in is the same session cookie a provider's sign-in does, so
// roles, route guards and MCP consent don't know which way someone came in.

const (
	// minPasswordLen is in characters. There are no rules about what kind:
	// length is what makes a password hard to guess.
	minPasswordLen = 12
	// maxPasswordBytes bounds what an anonymous request can make us hash.
	maxPasswordBytes = 256
	// passwordLinkLife is how long an emailed link works.
	passwordLinkLife = time.Hour

	// OWASP's Argon2id minimum (19 MiB, two passes, one thread). A stored hash
	// carries its own parameters, so raising these leaves old ones checkable.
	argonMemoryKiB = 19 * 1024
	argonPasses    = 2
	argonThreads   = 1
	argonSaltLen   = 16
	argonKeyLen    = 32

	// passwordHashes is how many hashes run at once. Each is ~19 MiB and takes
	// the CPU it can get, on a replica of half a vCPU that is also taking card
	// uploads, so an anonymous flood waits here rather than stopping the site.
	passwordHashes = 2
	// passwordHashWait is how long a request waits for one.
	passwordHashWait = 10 * time.Second
	// passwordMailers is how many link requests are in hand at once; beyond
	// that they are dropped, and the person can ask again.
	passwordMailers = 8
)

const (
	msgBadPassword   = "That address and password don't match."
	msgTooManyTries  = "Too many tries. Wait a few minutes, or email yourself a link to set a new password."
	msgPasswordBusy  = "The server is busy. Please try again in a moment."
	msgLinkSpent     = "That link has expired or has already been used. Ask for a new one from the sign-in page."
	msgPasswordShort = "Please choose a password of at least 12 characters."
	msgPasswordLong  = "That password is too long."
	msgPasswordEmail = "Your password can't be your email address."
)

// hashPassword is the argon2id hash of password, in the usual PHC form.
func hashPassword(password string) string {
	salt := make([]byte, argonSaltLen)
	rand.Read(salt)
	key := argon2.IDKey([]byte(password), salt, argonPasses, argonMemoryKiB, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, argonMemoryKiB, argonPasses, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key))
}

// passwordMatches checks password against a hash from hashPassword, by the
// parameters written in the hash.
func passwordMatches(password, stored string) bool {
	var version int
	var memory, passes uint32
	var threads uint8
	parts := strings.Split(stored, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &passes, &threads); err != nil || passes == 0 || threads == 0 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(want) == 0 {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, passes, memory, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// noOneHash is what a sign-in with no password to check is checked against, so
// taking a hash's time doesn't say the address has a password.
var noOneHash = sync.OnceValue(func() string { return hashPassword(randomToken()) })

// passwordLink is what an emailed link carries. Nothing is stored: the version
// spends it (setting a password moves it on), and the address kills it if a
// coordinator has since moved the person to another one.
type passwordLink struct {
	UserID  string `json:"u"`
	Email   string `json:"e"`
	Version int    `json:"v"`
}

// passwordGuard limits password sign-in. It is in memory, as tusd's locks and
// the spent OAuth codes are, because one replica is the whole deployment; a
// restart forgets it, which only ever gives someone more tries.
type passwordGuard struct {
	links  *keyset
	hashes chan struct{}
	mailer chan struct{}

	// failures counts wrong passwords by address, wherever they came from, so
	// many machines can't share out a guess at one person's. It locks out
	// passwords only: their provider's button still works, and so does a link.
	failures *limiter
	// attempts counts sign-ins by client.
	attempts *limiter
	// addressMail and clientMail stop the site being used to flood an inbox.
	addressMail, clientMail *limiter
	// allMail keeps a flood of them below what ACS allows the subscription in
	// an hour (100), so sign-in links still go out while the rest are refused.
	allMail *limiter
}

func newPasswordGuard(keys *keyset) *passwordGuard {
	return &passwordGuard{
		links:       keys.derive("password-link"),
		hashes:      make(chan struct{}, passwordHashes),
		mailer:      make(chan struct{}, passwordMailers),
		failures:    newLimiter(10, 15*time.Minute),
		attempts:    newLimiter(50, 15*time.Minute),
		addressMail: newLimiter(3, time.Hour),
		clientMail:  newLimiter(20, time.Hour),
		allMail:     newLimiter(60, time.Hour),
	}
}

// hash runs f in one of the hash slots, or says the server is too busy.
func (g *passwordGuard) hash(ctx context.Context, f func()) error {
	ctx, cancel := context.WithTimeout(ctx, passwordHashWait)
	defer cancel()
	select {
	case g.hashes <- struct{}{}:
		defer func() { <-g.hashes }()
		f()
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// limiterKeys bounds a limiter's memory, whatever anonymous requests name. A
// limiter that fills refuses keys it doesn't know until they expire.
const limiterKeys = 20000

// limiter allows max events per key in a fixed window from the key's first.
type limiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	keys   map[string]*limiterEntry
}

type limiterEntry struct {
	count int
	ends  time.Time
}

func newLimiter(max int, window time.Duration) *limiter {
	return &limiter{max: max, window: window, keys: map[string]*limiterEntry{}}
}

// over says whether key has used up its window, without counting anything.
func (l *limiter) over(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.overLocked(key, now)
}

func (l *limiter) overLocked(key string, now time.Time) bool {
	e, ok := l.keys[key]
	switch {
	case ok && now.Before(e.ends):
		return e.count >= l.max
	case ok:
		delete(l.keys, key)
	}
	if len(l.keys) >= limiterKeys {
		for k, e := range l.keys {
			if !now.Before(e.ends) {
				delete(l.keys, k)
			}
		}
		return len(l.keys) >= limiterKeys
	}
	return false
}

// hit counts an event against key, and says whether it was allowed.
func (l *limiter) hit(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.overLocked(key, now) {
		return false
	}
	e, ok := l.keys[key]
	if !ok {
		e = &limiterEntry{ends: now.Add(l.window)}
		l.keys[key] = e
	}
	e.count++
	return true
}

// clear forgets key.
func (l *limiter) clear(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.keys, key)
}

// clientKey is who a request came from, for limiting. Container Apps' ingress
// appends the address it saw to X-Forwarded-For, so only the rightmost entry is
// its word; anything to the left is the client's to say. Dev has no ingress, so
// it reads the socket. An IPv6 client is counted by its /64, the most anyone is
// likely to hold cheaply.
func (h *handlers) clientKey(r *http.Request) string {
	raw := ""
	if !h.dev {
		if forwarded := r.Header.Values("X-Forwarded-For"); len(forwarded) > 0 {
			last := forwarded[len(forwarded)-1]
			raw = strings.TrimSpace(last[strings.LastIndex(last, ",")+1:])
		}
	}
	if raw == "" {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		raw = host
	}
	addr, err := netip.ParseAddr(raw)
	if err != nil {
		return raw
	}
	if addr.Is6() && !addr.Is4In6() {
		if prefix, err := addr.Prefix(64); err == nil {
			return prefix.String()
		}
	}
	return addr.Unmap().String()
}

func normalizedAddress(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// passwordSignIn is the sign-in form. Every way of failing gets the same
// answer, in about the same time, so it doesn't say who is on the roster.
func (h *handlers) passwordSignIn(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := decode(r, &body); err != nil {
		h.problem(w, http.StatusBadRequest, err.Error())
		return
	}
	address, now := normalizedAddress(body.Email), h.now()
	g := h.passwords
	if !g.attempts.hit(h.clientKey(r), now) || g.failures.over(address, now) {
		h.log.Warn("password sign-in limited", "email", address)
		h.problem(w, http.StatusTooManyRequests, msgTooManyTries)
		return
	}
	if address == "" || len(body.Password) > maxPasswordBytes {
		g.failures.hit(address, now)
		h.problem(w, http.StatusUnauthorized, msgBadPassword)
		return
	}

	ctx := r.Context()
	me, known, err := h.onRoster(r, address)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	stored := noOneHash()
	if known {
		switch c, err := h.store.GetCredential(ctx, me.ID); {
		case errors.Is(err, db.ErrNotFound):
			known = false
		case err != nil:
			h.fail(w, r, err)
			return
		default:
			stored = c.PasswordHash
		}
	}
	var matches bool
	if err := g.hash(ctx, func() { matches = passwordMatches(body.Password, stored) }); err != nil {
		h.problem(w, http.StatusServiceUnavailable, msgPasswordBusy)
		return
	}
	if !known || !matches {
		g.failures.hit(address, now)
		h.log.Info("password sign-in refused", "email", address)
		h.problem(w, http.StatusUnauthorized, msgBadPassword)
		return
	}
	g.failures.clear(address)
	h.finishPasswordSignIn(w, r, me.ID)
}

// finishPasswordSignIn records the sign-in and sets the session.
func (h *handlers) finishPasswordSignIn(w http.ResponseWriter, r *http.Request, id string) {
	me, err := h.store.UpdateUser(r.Context(), id, func(u *db.User) error {
		if u.RemovedAt != nil {
			return db.ErrNotFound
		}
		u.Identity = &db.Identity{Provider: ProviderPassword, Subject: u.ID}
		t := h.stamp()
		u.LastSignInAt = &t
		return nil
	})
	switch {
	case errors.Is(err, db.ErrNotFound):
		h.problem(w, http.StatusUnauthorized, msgBadPassword)
		return
	case err != nil:
		h.fail(w, r, err)
		return
	}
	h.setSession(w, me)
	h.log.Info("signed in", "provider", ProviderPassword, "email", me.Email, "role", me.Role, "roles", me.Roles)
	h.json(w, http.StatusOK, map[string]any{"user": personOf(me)})
}

// requestPasswordLink emails a link to set a password. It answers before it
// looks at the roster, so neither its answer nor how long that took says
// whether the address is on it; the work goes on behind the response.
func (h *handlers) requestPasswordLink(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email string `json:"email"`
	}
	if err := decode(r, &body); err != nil {
		h.problem(w, http.StatusBadRequest, err.Error())
		return
	}
	address, now := normalizedAddress(body.Email), h.now()
	if !strings.Contains(address, "@") || len(address) > 320 {
		h.problem(w, http.StatusBadRequest, "a valid email address is required")
		return
	}
	g := h.passwords
	switch {
	case !g.clientMail.hit(h.clientKey(r), now), !g.addressMail.hit(address, now):
		h.log.Warn("password link limited", "email", address)
	default:
		select {
		case g.mailer <- struct{}{}:
			go func() {
				defer func() { <-g.mailer }()
				h.mailPasswordLink(address)
			}()
		default:
			h.log.Warn("password link dropped: too many in hand", "email", address)
		}
	}
	h.json(w, http.StatusAccepted, map[string]string{"status": "accepted"})
}

// mailPasswordLink sends the link, if the address is on the roster. It outlives
// the request, so it has its own deadline. A failure can only be logged: the
// person has already been told to look for the mail, and can ask again.
func (h *handlers) mailPasswordLink(address string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	u, err := h.store.GetUserByEmail(ctx, address)
	switch {
	case errors.Is(err, db.ErrNotFound):
		h.log.Info("password link not sent: not on the roster", "email", address)
		return
	case err != nil:
		h.log.Error("password link not sent", "email", address, "err", err)
		return
	case u.RemovedAt != nil:
		h.log.Info("password link not sent: removed from the roster", "email", address)
		return
	}
	version := 0
	switch c, err := h.store.GetCredential(ctx, u.ID); {
	case err == nil:
		version = c.Version
	case !errors.Is(err, db.ErrNotFound):
		h.log.Error("password link not sent", "email", address, "err", err)
		return
	}
	if !h.passwords.allMail.hit("", h.now()) {
		h.log.Error("password link not sent: the hourly limit on mail is used up", "email", address)
		return
	}
	token := signJSON(h.passwords.links, passwordLink{UserID: u.ID, Email: u.Email, Version: version}, h.now().Add(passwordLinkLife))
	message := passwordLinkMail(u.Email, h.publicURL+"/signin/password?"+url.Values{"token": {token}}.Encode(), version == 0)
	if err := h.mail.Send(ctx, message); err != nil {
		h.log.Error("password link not sent", "email", address, "err", err)
		return
	}
	h.log.Info("password link sent", "email", address)
}

func passwordLinkMail(to, link string, first bool) email.Message {
	action := "reset your Birdsense password"
	if first {
		action = "set a password for Birdsense"
	}
	subject := strings.ToUpper(action[:1]) + action[1:]
	const after = "The link works once, for an hour. If you didn't ask for this, you can ignore it: nothing has changed."
	return email.Message{
		To:      to,
		Subject: subject,
		Text:    fmt.Sprintf("Use this link to %s:\n\n%s\n\n%s\n\nEastside Audubon\n", action, link, after),
		HTML: fmt.Sprintf(`<p>Use this link to %s:</p><p><a href="%s">%s</a></p><p>%s</p><p>Eastside Audubon</p>`,
			action, html.EscapeString(link), html.EscapeString(subject), after),
	}
}

// checkPasswordLink is who a link is for, if it is still good.
func (h *handlers) checkPasswordLink(ctx context.Context, token string) (db.User, passwordLink, bool, error) {
	var link passwordLink
	if err := verifyJSON(h.passwords.links, token, h.now(), &link); err != nil {
		return db.User{}, link, false, nil
	}
	u, err := h.store.GetUser(ctx, link.UserID)
	switch {
	case errors.Is(err, db.ErrNotFound):
		return db.User{}, link, false, nil
	case err != nil:
		return db.User{}, link, false, err
	}
	if u.RemovedAt != nil || u.Email != link.Email {
		return db.User{}, link, false, nil
	}
	version := 0
	switch c, err := h.store.GetCredential(ctx, u.ID); {
	case err == nil:
		version = c.Version
	case !errors.Is(err, db.ErrNotFound):
		return db.User{}, link, false, err
	}
	return u, link, version == link.Version, nil
}

// readPasswordLink is what the set-a-password page shows: whose it is. Holding
// a good link proves the address, so saying who they are, and that they are an
// admin, tells nobody anything new. Opening it spends nothing, so a mail
// scanner following the link does no harm.
func (h *handlers) readPasswordLink(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	u, link, ok, err := h.checkPasswordLink(r.Context(), r.URL.Query().Get("token"))
	switch {
	case err != nil:
		h.fail(w, r, err)
	case !ok:
		h.problem(w, http.StatusGone, msgLinkSpent)
	default:
		h.json(w, http.StatusOK, map[string]any{"user": personOf(u), "first": link.Version == 0})
	}
}

// setPassword spends a link. It ends every session and MCP connection begun
// before it, then signs this browser in: a reset is often because someone else
// may have the old password, and they mustn't stay signed in with it.
func (h *handlers) setPassword(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token    string `json:"token"`
		Password string `json:"password"`
	}
	if err := decode(r, &body); err != nil {
		h.problem(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx := r.Context()
	u, link, ok, err := h.checkPasswordLink(ctx, body.Token)
	switch {
	case err != nil:
		h.fail(w, r, err)
		return
	case !ok:
		h.problem(w, http.StatusGone, msgLinkSpent)
		return
	}
	switch {
	case utf8.RuneCountInString(body.Password) < minPasswordLen:
		h.problem(w, http.StatusBadRequest, msgPasswordShort)
		return
	case len(body.Password) > maxPasswordBytes:
		h.problem(w, http.StatusBadRequest, msgPasswordLong)
		return
	case normalizedAddress(body.Password) == u.Email:
		h.problem(w, http.StatusBadRequest, msgPasswordEmail)
		return
	}
	var stored string
	if err := h.passwords.hash(ctx, func() { stored = hashPassword(body.Password) }); err != nil {
		h.problem(w, http.StatusServiceUnavailable, msgPasswordBusy)
		return
	}

	// The cutoff goes first: if the password then fails to store, the worst of
	// it is being signed out with the link still good, where the other order
	// could leave a new password beside sessions it was meant to end.
	cutoff := h.stamp()
	if _, err := h.store.UpdateUser(ctx, u.ID, func(u *db.User) error {
		u.SessionsValidFrom = &cutoff
		return nil
	}); err != nil {
		h.fail(w, r, err)
		return
	}
	switch _, err := h.store.SetCredential(ctx, u.ID, link.Version, stored); {
	case errors.Is(err, db.ErrConflict):
		h.problem(w, http.StatusGone, msgLinkSpent)
		return
	case err != nil:
		h.fail(w, r, err)
		return
	}
	h.passwords.failures.clear(u.Email)
	h.finishPasswordSignIn(w, r, u.ID)
}
