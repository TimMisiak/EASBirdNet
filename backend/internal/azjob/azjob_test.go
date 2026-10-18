package azjob

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
)

type fakeCred struct{}

func (fakeCred) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{Token: "t", ExpiresOn: time.Now().Add(time.Hour)}, nil
}

const id = "/subscriptions/s/resourceGroups/rg-birdsense-prod/providers/Microsoft.App/jobs/caj-birdsense-prod"

func TestStartAndRunning(t *testing.T) {
	var started int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer t" || r.URL.Query().Get("api-version") != apiVersion {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == id+"/start":
			b, _ := io.ReadAll(r.Body)
			if string(b) != "{}" {
				t.Errorf("start body = %q", b)
			}
			started++
			w.WriteHeader(http.StatusAccepted)
		case r.Method == http.MethodGet && r.URL.Path == id+"/executions" && r.URL.Query().Get("page") == "":
			io.WriteString(w, `{"value":[{"properties":{"status":"Running"}},{"properties":{"status":"Succeeded"}}],
				"nextLink":"`+"http://"+r.Host+id+`/executions?api-version=`+apiVersion+`&page=2"}`)
		case r.Method == http.MethodGet && r.URL.Path == id+"/executions":
			io.WriteString(w, `{"value":[{"properties":{"status":"Processing"}},{"properties":{"status":"Failed"}}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	j := &Job{id: id, cred: fakeCred{}, client: srv.Client(), endpoint: srv.URL}

	if err := j.Start(context.Background()); err != nil || started != 1 {
		t.Fatalf("Start = %v, %d started", err, started)
	}
	if n, err := j.Running(context.Background()); err != nil || n != 2 {
		t.Errorf("Running = %d, %v; want the running and the processing one, across both pages", n, err)
	}
}

func TestRefusalsAreErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"code":"AuthorizationFailed"}}`, http.StatusForbidden)
	}))
	defer srv.Close()
	j := &Job{id: id, cred: fakeCred{}, client: srv.Client(), endpoint: srv.URL}
	if err := j.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "AuthorizationFailed") {
		t.Errorf("Start = %v, want the refusal with Azure's reason", err)
	}
}

func TestValidID(t *testing.T) {
	if err := validID(id); err != nil {
		t.Error(err)
	}
	for _, bad := range []string{"", "caj-birdsense-prod", "/subscriptions/s/resourceGroups/rg/providers/Microsoft.App/containerApps/ca"} {
		if validID(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
