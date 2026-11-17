// Package email sends the mail Birdsense sends: today, only the link that sets
// or resets a password. One interface and two backends, as with internal/db:
// Azure Communication Services in a deployment, and the log in development,
// where nothing has a domain to send from.
//
// ACS Email is retiring on 2028-09-30 (DEPLOYMENT.md, Email), so another
// backend is expected here before then; nothing outside this package knows
// which one is running.
package email

import (
	"context"
	"log/slog"
)

// Message is one email to one person.
type Message struct {
	To      string
	Subject string
	Text    string
	HTML    string
}

// Sender hands a message to whatever delivers it. Success means it was
// accepted, not delivered.
type Sender interface {
	Send(ctx context.Context, m Message) error
}

// Log writes each message to the log, sign-in links and all, so a developer
// can follow one. It is for development: a deployment's logs mustn't hold them.
type Log struct{ Logger *slog.Logger }

func (l Log) Send(_ context.Context, m Message) error {
	l.Logger.Info("email (development: logged, not sent)", "to", m.To, "subject", m.Subject, "text", m.Text)
	return nil
}
