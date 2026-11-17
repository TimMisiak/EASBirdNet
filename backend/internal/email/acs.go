package email

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// acsAPIVersion is the Email API version this was written against.
const acsAPIVersion = "2025-09-01"

// ACS sends through Azure Communication Services' REST API, signing each
// request with the resource's access key. That key is the one data-plane
// secret in the stack: the REST reference documents only this, and Entra ID
// only through ACS's SDKs (DEPLOYMENT.md, Email).
type ACS struct {
	endpoint *url.URL
	key      []byte
	sender   string
	client   *http.Client
	now      func() time.Time
}

// NewACS checks its configuration, so a bad value stops the server at startup
// rather than failing the first volunteer who asks for a link.
func NewACS(endpoint, accessKey, sender string) (*ACS, error) {
	u, err := url.Parse(strings.TrimSuffix(endpoint, "/"))
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("email: the endpoint must be https and a host, like https://x.communication.azure.com, not %q", endpoint)
	}
	key, err := base64.StdEncoding.DecodeString(accessKey)
	if err != nil || len(key) == 0 {
		return nil, errors.New("email: the access key must be the resource's base64 key")
	}
	if !strings.Contains(sender, "@") {
		return nil, fmt.Errorf("email: the sender must be an address, not %q", sender)
	}
	return &ACS{
		endpoint: u, key: key, sender: sender,
		client: &http.Client{Timeout: 30 * time.Second},
		now:    time.Now,
	}, nil
}

type acsAddress struct {
	Address string `json:"address"`
}

type acsRequest struct {
	SenderAddress string `json:"senderAddress"`
	Content       struct {
		Subject   string `json:"subject"`
		PlainText string `json:"plainText,omitempty"`
		HTML      string `json:"html,omitempty"`
	} `json:"content"`
	Recipients struct {
		To []acsAddress `json:"to"`
	} `json:"recipients"`
	// A sign-in link has to reach the person as it was written, not rewritten
	// through a click tracker.
	UserEngagementTrackingDisabled bool `json:"userEngagementTrackingDisabled"`
}

// Send submits the message. ACS answers 202 and delivers later, so a bad
// address or a bounce shows up only in ACS's own logs, not here.
func (a *ACS) Send(ctx context.Context, m Message) error {
	body := acsRequest{SenderAddress: a.sender, UserEngagementTrackingDisabled: true}
	body.Content.Subject, body.Content.PlainText, body.Content.HTML = m.Subject, m.Text, m.HTML
	body.Recipients.To = []acsAddress{{Address: m.To}}
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}

	u := *a.endpoint
	u.Path = "/emails:send"
	u.RawQuery = "api-version=" + acsAPIVersion
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	a.sign(req, b)

	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("email: sending: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("email: ACS answered %s: %s", resp.Status, detail)
	}
	return nil
}

// sign adds ACS's HMAC headers. The signature covers the method, the path and
// query, and the date, host and body hash, as
// "POST\n<path?query>\n<date>;<host>;<hash>".
func (a *ACS) sign(req *http.Request, body []byte) {
	sum := sha256.Sum256(body)
	hash := base64.StdEncoding.EncodeToString(sum[:])
	date := a.now().UTC().Format(http.TimeFormat)
	mac := hmac.New(sha256.New, a.key)
	fmt.Fprintf(mac, "%s\n%s\n%s;%s;%s", req.Method, req.URL.RequestURI(), date, req.URL.Host, hash)
	req.Header.Set("x-ms-date", date)
	req.Header.Set("x-ms-content-sha256", hash)
	req.Header.Set("Authorization", "HMAC-SHA256 SignedHeaders=x-ms-date;host;x-ms-content-sha256&Signature="+
		base64.StdEncoding.EncodeToString(mac.Sum(nil)))
}
