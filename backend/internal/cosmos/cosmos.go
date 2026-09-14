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
	"net/url"
	"strings"

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

// Check confirms the account answers an authenticated read. Listing databases
// is the one useful read that works before anything has been created: it needs
// no database, no container, and creates nothing.
//
// Once the containers from DATA-MODEL.md exist, this should become a metadata
// read on one of them -- that also proves the schema we expect is there, which
// listing databases does not.
func (c *Client) Check(ctx context.Context) error {
	// A probe wants a verdict, not persistence. Left to the SDK's default three
	// retries with backoff, "nothing is listening" takes about ten seconds to
	// say, which is ten seconds of startup or of a held-open health request.
	ctx = policy.WithRetryOptions(ctx, policy.RetryOptions{MaxRetries: -1})

	// One page is enough; we care that the account answered, not what it said.
	pager := c.az.NewQueryDatabasesPager("SELECT * FROM root r", nil)
	if _, err := pager.NextPage(ctx); err != nil {
		return fmt.Errorf("list databases: %w", err)
	}
	return nil
}
