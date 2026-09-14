package cosmos

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// emulatorKey is the Cosmos DB emulator's well-known account key. It is
// published in Microsoft's own documentation and in the emulator image itself,
// and it unlocks nothing but a local container.
const emulatorKey = "C2y6yDjf5/R+ob0N8A7Cgv30VRDJIWEHLM+4QDU5DE2nQ9nDuVTqobD4b8mGGyPMbIZnqyMsEcaGQy67XIw/Jw=="

// fakeGateway stands in for the emulator: it answers the account read at "/"
// with accountStatus and the read of /dbs/birdsense with dbStatus. The point is
// not to reimplement Cosmos DB, it is to pin down that Check issues nothing but
// reads -- no query, no create -- and that it works over plain HTTP, which is
// how docker-compose reaches the emulator.
func fakeGateway(t *testing.T, accountStatus, dbStatus int) string {
	t.Helper()
	var self string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("Check sent %s %s; want reads only", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/":
			if accountStatus != http.StatusOK {
				w.WriteHeader(accountStatus)
				fmt.Fprint(w, `{"code":"Error","message":"account read refused"}`)
				return
			}
			// The gateway advertises where the account can be reached; the SDK
			// reads this before anything else and routes by it.
			fmt.Fprintf(w, `{"id":"localhost","_self":"","_rid":"localhost","_dbs":"//dbs/",`+
				`"writableLocations":[{"name":"local","databaseAccountEndpoint":%q}],`+
				`"readableLocations":[{"name":"local","databaseAccountEndpoint":%q}],`+
				`"userConsistencyPolicy":{"defaultConsistencyLevel":"Session"}}`, self, self)
		case "/dbs/birdsense":
			w.WriteHeader(dbStatus)
			if dbStatus == http.StatusOK {
				fmt.Fprint(w, `{"id":"birdsense","_rid":"AAAA==","_self":"dbs/AAAA==/","_etag":"\"0\"","_colls":"colls/","_users":"users/","_ts":1}`)
				return
			}
			fmt.Fprint(w, `{"code":"NotFound","message":"Resource Not Found"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)
	self = srv.URL + "/"
	return srv.URL
}

// check runs Check with a deadline, so an SDK retry loop fails the test
// instead of hanging it.
func check(t *testing.T, endpoint string) error {
	t.Helper()
	c, err := New(Config{Endpoint: endpoint, Key: emulatorKey, Database: "birdsense"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return c.Check(ctx)
}

func TestCheckWhenDatabaseExists(t *testing.T) {
	if err := check(t, fakeGateway(t, http.StatusOK, http.StatusOK)); err != nil {
		t.Fatalf("Check: %v", err)
	}
}

// Nothing creates the database yet, so "not found" is the normal answer from
// a healthy emulator.
func TestCheckWhenDatabaseNotCreatedYet(t *testing.T) {
	if err := check(t, fakeGateway(t, http.StatusOK, http.StatusNotFound)); err != nil {
		t.Fatalf("Check: %v, want nil for a database that does not exist yet", err)
	}
}

func TestCheckReportsRejection(t *testing.T) {
	err := check(t, fakeGateway(t, http.StatusUnauthorized, http.StatusUnauthorized))
	if err == nil {
		t.Fatal("Check on a rejecting account = nil, want an error")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("Check error = %v, want it to mention the 401", err)
	}
}

// An endpoint that 404s everything is not a Cosmos gateway, and its 404 must
// not pass for "database not created yet".
func TestCheckRejectsEndpointThatIsNotCosmos(t *testing.T) {
	if err := check(t, fakeGateway(t, http.StatusNotFound, http.StatusNotFound)); err == nil {
		t.Fatal("Check against an endpoint whose account read 404s = nil, want an error")
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
