package email

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The signature is worked out here the way Microsoft's HMAC tutorial does,
// from what actually arrived, so the two have to agree on every part of it.
func TestACSSendsASignedRequest(t *testing.T) {
	key := []byte("a key of no particular length")
	var got acsRequest
	var problem string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			problem = err.Error()
		}
		if r.Method != http.MethodPost || r.URL.RequestURI() != "/emails:send?api-version="+acsAPIVersion {
			problem = "asked for " + r.Method + " " + r.URL.RequestURI()
		}
		sum := sha256.Sum256(body)
		hash := base64.StdEncoding.EncodeToString(sum[:])
		if r.Header.Get("x-ms-content-sha256") != hash {
			problem = "the body hash doesn't match the body"
		}
		mac := hmac.New(sha256.New, key)
		io.WriteString(mac, "POST\n"+r.URL.RequestURI()+"\n"+r.Header.Get("x-ms-date")+";"+r.Host+";"+hash)
		want := "HMAC-SHA256 SignedHeaders=x-ms-date;host;x-ms-content-sha256&Signature=" + base64.StdEncoding.EncodeToString(mac.Sum(nil))
		if r.Header.Get("Authorization") != want {
			problem = "signature: got " + r.Header.Get("Authorization") + ", want " + want
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	a, err := NewACS(srv.URL, base64.StdEncoding.EncodeToString(key), "DoNotReply@mail.example.org")
	if err != nil {
		t.Fatal(err)
	}
	a.client = srv.Client()
	a.now = func() time.Time { return time.Date(2026, 11, 5, 18, 4, 5, 0, time.UTC) }

	if err := a.Send(context.Background(), Message{To: "jane@example.com", Subject: "Hello", Text: "Plain", HTML: "<p>Rich</p>"}); err != nil {
		t.Fatal(err)
	}
	if problem != "" {
		t.Fatal(problem)
	}
	if got.SenderAddress != "DoNotReply@mail.example.org" || got.Recipients.To[0].Address != "jane@example.com" ||
		got.Content.Subject != "Hello" || got.Content.PlainText != "Plain" || got.Content.HTML != "<p>Rich</p>" {
		t.Errorf("sent %+v", got)
	}
	if !got.UserEngagementTrackingDisabled {
		t.Error("a sign-in link must not go through a click tracker")
	}
}

func TestACSReportsARefusal(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":{"code":"TooManyRequests"}}`)
	}))
	defer srv.Close()
	a, err := NewACS(srv.URL, base64.StdEncoding.EncodeToString([]byte("k")), "a@example.org")
	if err != nil {
		t.Fatal(err)
	}
	a.client = srv.Client()
	err = a.Send(context.Background(), Message{To: "b@example.org"})
	if err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("got %v, want it to say 429", err)
	}
}

func TestNewACSRefusesBadConfiguration(t *testing.T) {
	good := base64.StdEncoding.EncodeToString([]byte("k"))
	for _, c := range []struct{ what, endpoint, key, sender string }{
		{"http", "http://x.communication.azure.com", good, "a@example.org"},
		{"no endpoint", "", good, "a@example.org"},
		{"a key that isn't base64", "https://x.communication.azure.com", "not base64!", "a@example.org"},
		{"no key", "https://x.communication.azure.com", "", "a@example.org"},
		{"a sender that isn't an address", "https://x.communication.azure.com", good, "nobody"},
	} {
		if _, err := NewACS(c.endpoint, c.key, c.sender); err == nil {
			t.Errorf("%s: no error", c.what)
		}
	}
}
