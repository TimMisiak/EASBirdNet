// Package cosmos holds the Azure Cosmos DB connection: where the account is,
// how the app authenticates to it, and a read-only reachability check the
// health endpoint reports on.
//
// There is no schema and no data access here yet. DATA-MODEL.md defines the
// containers and their partition keys; nothing in this package creates them,
// and the API still answers from the in-memory placeholder store.
package cosmos

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/data/azcosmos"
)

// Config is where the account is and how we authenticate to it.
//
// Locally this points at the Cosmos DB emulator in docker-compose over plain
// HTTP with the emulator's well-known key. In Azure it is the account's HTTPS
// gateway; the key there belongs in Key Vault, and should give way to a managed
// identity (azidentity + azcosmos.NewClient) as soon as we have one to use.
type Config struct {
	// Endpoint is the account gateway URL, e.g. http://cosmos:8081 for the
	// emulator or https://<account>.documents.azure.com for a real account.
	// Empty means "no database configured": the app still serves the frontend,
	// the public page and the placeholder store.
	Endpoint string
	// Key is the account key. Required while key auth is the only auth.
	Key string
	// Database is the database every container lives in.
	Database string
}

// Configured reports whether there is an account to talk to at all.
func (c Config) Configured() bool { return c.Endpoint != "" }

// Client is a Cosmos DB account handle. It performs no I/O until something
// calls it, so constructing one during startup never blocks on the emulator
// still coming up.
type Client struct {
	cfg Config
	az  *azcosmos.Client
}

// New validates the configuration and builds the account client. It fails only
// on configuration a person got wrong -- an unusable endpoint or a key that is
// not base64 -- so a caller can treat an error as fatal and a network problem
// as something Check reports later.
func New(cfg Config) (*Client, error) {
	if !cfg.Configured() {
		return nil, errors.New("no endpoint configured")
	}
	u, err := url.Parse(cfg.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("endpoint %q: %w", cfg.Endpoint, err)
	}
	if u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("endpoint %q: want an http(s) URL like http://cosmos:8081", cfg.Endpoint)
	}
	if strings.TrimSpace(cfg.Key) == "" {
		return nil, errors.New("no account key configured")
	}
	if cfg.Database == "" {
		return nil, errors.New("no database name configured")
	}

	// The SDK signs shared-key requests itself and does not insist on TLS, so
	// the emulator's plain-HTTP endpoint works without a certificate dance.
	// A managed identity would: OAuth over HTTP is refused, by design.
	cred, err := azcosmos.NewKeyCredential(cfg.Key)
	if err != nil {
		return nil, fmt.Errorf("account key: %w", err)
	}
	az, err := azcosmos.NewClientWithKey(cfg.Endpoint, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("cosmos client: %w", err)
	}
	return &Client{cfg: cfg, az: az}, nil
}

// Endpoint is the account URL, for logging. It carries no credential.
func (c *Client) Endpoint() string { return c.cfg.Endpoint }

// Database is the configured database name, for logging.
func (c *Client) Database() string { return c.cfg.Database }

// Name identifies this dependency in the health response.
func (c *Client) Name() string { return "cosmos" }

// Check confirms the account answers an authenticated read. It reads the
// configured database's metadata, and "not found" counts as success: the
// account answered and accepted the key, and the database has simply not been
// created yet -- which is the state of things until DATA-MODEL.md is built.
//
// It is a point read rather than a query on purpose. The emulator this project
// is pinned to answers a query over /dbs with "Have not implemented Query on
// Database", while reading one database works there and on Azure alike. Once
// the containers exist, this should read one of them instead, so a missing
// schema shows up here rather than on the first real request.
func (c *Client) Check(ctx context.Context) error {
	// A probe wants a verdict, not persistence. This turns off azcore's generic
	// retries (three, with backoff), so an endpoint with nothing listening says
	// so at once instead of after about ten seconds.
	//
	// It does not turn off azcosmos's own failover retries. The SDK reads the
	// account first and then sends every request to the address the account
	// advertises; if this process cannot reach that address -- the emulator
	// advertising "localhost" is the usual way -- it retries there until ctx
	// expires, and all that surfaces is "context deadline exceeded". See
	// GATEWAY_PUBLIC_ENDPOINT in docker-compose.yml.
	ctx = policy.WithRetryOptions(ctx, policy.RetryOptions{MaxRetries: -1})

	db, err := c.az.NewDatabase(c.cfg.Database)
	if err != nil {
		return fmt.Errorf("database %q: %w", c.cfg.Database, err)
	}
	_, err = db.Read(ctx, nil)
	if err == nil || isDatabaseNotFound(err, c.cfg.Database) {
		return nil
	}
	return fmt.Errorf("read database %q: %w", c.cfg.Database, err)
}

// isDatabaseNotFound reports whether err is a 404 for the database itself.
// The path matters: the SDK reads the account at "/" before anything else, and
// an endpoint that is not a Cosmos gateway at all can 404 that request too.
// Only a 404 for /dbs/<name> means "reachable, just not created yet".
func isDatabaseNotFound(err error, database string) bool {
	var respErr *azcore.ResponseError
	if !errors.As(err, &respErr) || respErr.StatusCode != http.StatusNotFound || respErr.RawResponse == nil {
		return false
	}
	req := respErr.RawResponse.Request
	return req != nil && strings.TrimSuffix(req.URL.Path, "/") == "/dbs/"+database
}
