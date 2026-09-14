package cosmos

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// emulatorKey is the Cosmos DB emulator's well-known account key. It is
// published in Microsoft's own documentation and in the emulator image itself,
// and it unlocks nothing but a local container.
const emulatorKey = "C2y6yDjf5/R+ob0N8A7Cgv30VRDJIWEHLM+4QDU5DE2nQ9nDuVTqobD4b8mGGyPMbIZnqyMsEcaGQy67XIw/Jw=="

// fakeGateway stands in for the emulator: it answers the two requests the SDK
// makes for Check -- the account read at "/" and the database query at "/dbs".
// The point is not to reimplement Cosmos DB, it is to pin down that Check
// issues read-only requests and that they work over plain HTTP, which is how
// docker-compose reaches the emulator.
func fakeGateway(t *testing.T, status int) string {
	t.Helper()
	var self string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.URL.Path != "/dbs" {
			t.Errorf("unexpected write: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		if status != http.StatusOK {
			w.WriteHeader(status)
			fmt.Fprint(w, `{"message":"denied"}`)
			return
		}
		if r.URL.Path == "/" {
			// The gateway advertises where the account can be reached; the SDK
			// reads this before anything else and routes by it.
			fmt.Fprintf(w, `{"id":"localhost","_self":"","_rid":"localhost","_dbs":"//dbs/",`+
				`"writableLocations":[{"name":"local","databaseAccountEndpoint":%q}],`+
				`"readableLocations":[{"name":"local","databaseAccountEndpoint":%q}],`+
				`"userConsistencyPolicy":{"defaultConsistencyLevel":"Session"}}`, self, self)
			return
		}
		fmt.Fprint(w, `{"_rid":"","Databases":[],"_count":0}`)
	}))
	t.Cleanup(srv.Close)
	self = srv.URL + "/"
	return srv.URL
}

func TestCheckOverPlainHTTP(t *testing.T) {
	c, err := New(Config{Endpoint: fakeGateway(t, http.StatusOK), Key: emulatorKey, Database: "birdsense"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := c.Check(context.Background()); err != nil {
		t.Fatalf("Check: %v", err)
	}
}

func TestCheckReportsRejection(t *testing.T) {
	c, err := New(Config{Endpoint: fakeGateway(t, http.StatusUnauthorized), Key: emulatorKey, Database: "birdsense"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = c.Check(context.Background())
	if err == nil {
		t.Fatal("Check on a rejecting account = nil, want an error")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("Check error = %v, want it to mention the 401", err)
	}
}

func TestNewRejectsUnusableConfig(t *testing.T) {
	cases := map[string]Config{
		"no endpoint": {Key: emulatorKey, Database: "birdsense"},
		"not a URL":   {Endpoint: "cosmos:8081", Key: emulatorKey, Database: "birdsense"},
		"no key":      {Endpoint: "http://cosmos:8081", Database: "birdsense"},
		"key is not base64": {
			Endpoint: "http://cosmos:8081", Key: "not-a-key", Database: "birdsense",
		},
		"no database": {Endpoint: "http://cosmos:8081", Key: emulatorKey},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := New(cfg); err == nil {
				t.Error("New = nil error, want a refusal")
			}
		})
	}
}
