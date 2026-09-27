package api

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"time"
)

// Cookies Birdsense sets are signed with a key the server keeps, so what comes
// back is what went out. The value itself is readable -- an address, not a
// secret -- but it can't be edited without the key, which is the whole
// difference from the placeholder cookie this replaced.
//
// The key is configuration (BIRDSENSE_SESSION_KEY), not something generated at
// startup: a new key signs everyone out, and the season's volunteers sign in
// once. Rotating it deliberately is how you sign everyone out at once.
type keyset struct{ key []byte }

// MinSessionKeyLen is the shortest session key a deployment may set. Hashing
// the passphrase fixes its length, not its strength: a guessable key is a
// forgeable cookie, and the session cookie *is* the identity, so guessing one
// is being signed in as anyone on the roster, coordinators included. 32 bytes
// is what `openssl rand -base64 32` gives. cmd/server enforces it when it
// reads the configuration, and infra/variables.tf checks the same length at
// plan time; newKeyset itself only refuses an empty key, so a test can sign
// with a short one.
const MinSessionKeyLen = 32

func newKeyset(secret string) *keyset {
	if secret == "" {
		// Register has nowhere to return an error, and a keyset over the hash
		// of "" would sign cookies anyone could forge, so this fails the
		// process at startup rather than serving forgeable sessions.
		panic("api: the session cookie needs a key, and Options.SessionKey is empty")
	}
	// The key is a passphrase, so hash it to a fixed size rather than asking
	// whoever sets it for exactly 32 bytes.
	sum := sha256.Sum256([]byte(secret))
	return &keyset{key: sum[:]}
}

// randomToken is an unguessable value: the OAuth state, the OIDC nonce, dev's
// session key, and anything else that has to be unpredictable.
func randomToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// sign wraps value with its expiry and a signature over both, as
// "<expiry>.<value>.<mac>". The expiry is inside the signature, so it can't be
// pushed out by editing the cookie.
func (k *keyset) sign(value string, expires time.Time) string {
	body := strconv.FormatInt(expires.Unix(), 10) + "." + base64.RawURLEncoding.EncodeToString([]byte(value))
	return body + "." + k.mac(body)
}

// verify returns the signed value, if the signature is ours and the expiry has
// not passed.
func (k *keyset) verify(cookie string, now time.Time) (string, error) {
	body, mac, ok := reverseCut(cookie)
	if !ok {
		return "", errors.New("api: malformed cookie")
	}
	if !hmac.Equal([]byte(mac), []byte(k.mac(body))) {
		return "", errors.New("api: bad cookie signature")
	}
	expiry, encoded, ok := strings.Cut(body, ".")
	if !ok {
		return "", errors.New("api: malformed cookie")
	}
	unix, err := strconv.ParseInt(expiry, 10, 64)
	if err != nil {
		return "", errors.New("api: malformed cookie expiry")
	}
	if now.After(time.Unix(unix, 0)) {
		return "", errors.New("api: the cookie has expired")
	}
	value, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return "", errors.New("api: malformed cookie value")
	}
	return string(value), nil
}

// reverseCut moves the last "." to the front of the string, so strings.Cut
// splits the signature off a body that has dots of its own.
func reverseCut(s string) (string, string, bool) {
	i := strings.LastIndex(s, ".")
	if i < 0 {
		return s, "", false
	}
	return s[:i], s[i+1:], true
}

func (k *keyset) mac(body string) string {
	m := hmac.New(sha256.New, k.key)
	m.Write([]byte(body))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// RandomSessionKey is a session key for a server that was given none: dev,
// where signing everyone out on restart costs nothing. A deployment sets
// BIRDSENSE_SESSION_KEY instead, so a new revision doesn't sign everyone out.
func RandomSessionKey() string { return randomToken() }
