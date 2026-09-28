// Package azjob starts executions of a Container Apps job, and counts the
// running ones, through Azure Resource Manager -- what analysis runs as when
// it runs as a job (ANALYSIS.md, *Starting the job*). It signs in the way the
// rest of the server does, with DefaultAzureCredential: the app's managed
// identity in Azure, which needs the role infra/job.tf defines.
//
// It is the two REST calls it needs, not the Container Apps SDK:
//
//	POST {job}/start                start an execution (one worker replica)
//	GET  {job}/executions           its executions, each with a status
package azjob

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
)

// apiVersion is the Microsoft.App API version the calls are made against.
const apiVersion = "2024-03-01"

// Job is one Container Apps job.
type Job struct {
	// id is the job's resource id,
	// /subscriptions/…/resourceGroups/…/providers/Microsoft.App/jobs/….
	id       string
	cred     azcore.TokenCredential
	client   *http.Client
	endpoint string
}

// New returns the job with that resource id, signing in with
// DefaultAzureCredential.
func New(id string) (*Job, error) {
	if err := validID(id); err != nil {
		return nil, err
	}
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("azjob: credential: %w", err)
	}
	return &Job{id: id, cred: cred, client: &http.Client{Timeout: 30 * time.Second}, endpoint: "https://management.azure.com"}, nil
}

func validID(id string) error {
	parts := strings.Split(strings.Trim(id, "/"), "/")
	if len(parts) != 8 || parts[0] != "subscriptions" || parts[2] != "resourceGroups" ||
		!strings.EqualFold(parts[4], "providers") || !strings.EqualFold(parts[5], "Microsoft.App") || parts[6] != "jobs" {
		return fmt.Errorf("azjob: %q is not a Container Apps job's resource id (/subscriptions/…/resourceGroups/…/providers/Microsoft.App/jobs/…)", id)
	}
	return nil
}

// Start starts one execution of the job.
func (j *Job) Start(ctx context.Context) error {
	// The body can override the execution's template; empty, it runs the
	// job as defined.
	_, err := j.do(ctx, http.MethodPost, j.endpoint+j.id+"/start?api-version="+apiVersion, []byte("{}"))
	return err
}

// running are the execution statuses that mean a worker is, or is about to
// be, at work.
var running = map[string]bool{"running": true, "processing": true}

// Running counts the job's executions that are running or starting.
func (j *Job) Running(ctx context.Context) (int, error) {
	next := j.endpoint + j.id + "/executions?api-version=" + apiVersion
	n := 0
	// The list is the job's recent history, newest first, a page at a time;
	// a running execution is a recent one, so a few pages are plenty.
	for page := 0; next != "" && page < 5; page++ {
		b, err := j.do(ctx, http.MethodGet, next, nil)
		if err != nil {
			return 0, err
		}
		var list struct {
			Value []struct {
				Properties struct {
					Status string `json:"status"`
				} `json:"properties"`
			} `json:"value"`
			NextLink string `json:"nextLink"`
		}
		if err := json.Unmarshal(b, &list); err != nil {
			return 0, fmt.Errorf("azjob: reading the executions: %w", err)
		}
		for _, e := range list.Value {
			if running[strings.ToLower(e.Properties.Status)] {
				n++
			}
		}
		next = list.NextLink
	}
	return n, nil
}

func (j *Job) do(ctx context.Context, method, url string, body []byte) ([]byte, error) {
	tok, err := j.cred.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{"https://management.azure.com/.default"}})
	if err != nil {
		return nil, fmt.Errorf("azjob: signing in: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := j.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("azjob: %s %s: %w", method, j.id, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("azjob: %s %s: %s: %s", method, j.id, resp.Status, firstLine(string(b)))
	}
	return b, nil
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}
