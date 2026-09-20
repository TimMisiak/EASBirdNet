package api

import (
	"testing"
	"time"
)

func ptr[T any](v T) *T { return &v }

// Sign-in accepts any Microsoft account, which includes every organization's
// directory -- and a directory's administrators choose what their users' email
// claims say. So an email claim is only the person's address when the provider
// is in a position to know it is.
func TestTrustedEmail(t *testing.T) {
	multiTenant := &authProvider{
		cfg:            ProviderConfig{Name: ProviderMicrosoft},
		issuerTemplate: "https://login.microsoftonline.com/{tenantid}/v2.0",
	}
	oneTenant := &authProvider{cfg: ProviderConfig{Name: ProviderMicrosoft, Tenant: "a-tenant-id"}}
	google := &authProvider{cfg: ProviderConfig{Name: ProviderGoogle}}

	for _, c := range []struct {
		what     string
		provider *authProvider
		claims   idClaims
		want     string
	}{
		{"a personal Microsoft account", multiTenant,
			idClaims{Email: "Volunteer@outlook.com", Tenant: microsoftConsumerTenant}, "volunteer@outlook.com"},
		{"an organization that has proved it owns the domain", multiTenant,
			idClaims{Email: "dana@eastsideaudubon.org", Tenant: "some-tenant", EmailDomainOwnerVerified: ptr(true)}, "dana@eastsideaudubon.org"},
		{"an organization that has not", multiTenant,
			idClaims{Email: "dana@eastsideaudubon.org", Tenant: "an-impostor-tenant"}, ""},
		{"an organization that says the domain isn't verified", multiTenant,
			idClaims{Email: "dana@eastsideaudubon.org", Tenant: "an-impostor-tenant", EmailDomainOwnerVerified: ptr(false)}, ""},
		{"sign-in restricted to one directory", oneTenant,
			idClaims{Email: "dana@eastsideaudubon.org", Tenant: "a-tenant-id"}, "dana@eastsideaudubon.org"},
		{"the address only in preferred_username", multiTenant,
			idClaims{PreferredUsername: "volunteer@hotmail.com", Tenant: microsoftConsumerTenant}, "volunteer@hotmail.com"},
		{"no address at all", multiTenant,
			idClaims{Tenant: microsoftConsumerTenant}, ""},
		{"Google, verified", google,
			idClaims{Email: "Jane@gmail.com", EmailVerified: ptr(true)}, "jane@gmail.com"},
		{"Google, unverified", google,
			idClaims{Email: "jane@gmail.com", EmailVerified: ptr(false)}, ""},
		{"Google, silent about it", google,
			idClaims{Email: "jane@gmail.com"}, ""},
	} {
		got, err := trustedEmail(c.provider, c.claims)
		switch {
		case c.want == "" && err == nil:
			t.Errorf("%s: trusted %q, want it refused", c.what, got)
		case c.want != "" && err != nil:
			t.Errorf("%s: %v, want %q", c.what, err, c.want)
		case c.want != "" && got != c.want:
			t.Errorf("%s: got %q, want %q", c.what, got, c.want)
		}
	}
}

// A multi-tenant endpoint signs with a per-tenant issuer, so go-oidc is told
// to skip the issuer check and this does it instead.
func TestVerifyIssuer(t *testing.T) {
	p := &authProvider{issuerTemplate: "https://login.microsoftonline.com/{tenantid}/v2.0"}
	if err := p.verifyIssuer("https://login.microsoftonline.com/abc-123/v2.0", "abc-123"); err != nil {
		t.Errorf("the tenant's own issuer: %v", err)
	}
	if err := p.verifyIssuer("https://login.microsoftonline.com/abc-123/v2.0", "another-tenant"); err == nil {
		t.Error("an issuer from a tenant other than the token's was accepted")
	}
	if err := p.verifyIssuer("https://login.example.test/abc-123/v2.0", "abc-123"); err == nil {
		t.Error("an issuer somewhere else entirely was accepted")
	}
	if err := p.verifyIssuer("https://login.microsoftonline.com/abc-123/v2.0", ""); err == nil {
		t.Error("a token with no tenant claim was accepted")
	}

	// A single-tenant provider has already had its issuer checked by go-oidc.
	single := &authProvider{}
	if err := single.verifyIssuer("https://login.microsoftonline.com/abc-123/v2.0", ""); err != nil {
		t.Errorf("single tenant: %v", err)
	}
}

// The state, nonce and PKCE verifier ride in a signed cookie between the two
// halves of the flow, so neither can be chosen by whoever comes back.
func TestAuthStateCookie(t *testing.T) {
	now := time.Date(2026, time.September, 19, 12, 0, 0, 0, time.UTC)
	keys := newKeyset("a key")
	state := authState{Provider: ProviderMicrosoft, State: randomToken(), Nonce: randomToken(), Verifier: "a-verifier"}

	signed := keys.sign(`{"p":"microsoft","s":"`+state.State+`","n":"`+state.Nonce+`","v":"a-verifier"}`, now.Add(authStateLife))
	got, err := keys.verify(signed, now)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got == "" {
		t.Fatal("the signed state came back empty")
	}
	if _, err := keys.verify(signed, now.Add(authStateLife+time.Second)); err == nil {
		t.Error("an expired sign-in state was accepted")
	}
	if _, err := newKeyset("another key").verify(signed, now); err == nil {
		t.Error("a sign-in state signed with another key was accepted")
	}
}

// Two sign-ins never share a state or a nonce.
func TestRandomTokensDiffer(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		tok := randomToken()
		if tok == "" || seen[tok] {
			t.Fatalf("randomToken repeated or returned empty: %q", tok)
		}
		seen[tok] = true
	}
}
