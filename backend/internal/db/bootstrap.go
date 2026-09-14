package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// BootstrapAdmin puts the first admin on an empty roster, so a new deployment
// has someone who can sign in and add everyone else.
//
// It does nothing once anyone is on the roster, whatever their address. That
// makes it safe to leave configured: it can never re-grant admin to someone
// who was later demoted, removed or given a new address. When it does nothing,
// u is the existing user with that email if there is one, and the zero User if
// not (the setting most likely has a typo, or names someone never added).
func BootstrapAdmin(ctx context.Context, s Store, name, email string) (u User, created bool, err error) {
	if normalizeEmail(email) == "" {
		return User{}, false, errors.New("db: the bootstrap admin needs an email")
	}
	users, err := s.ListUsers(ctx)
	if err != nil {
		return User{}, false, fmt.Errorf("db: reading the roster: %w", err)
	}
	if len(users) == 0 {
		u, err = s.CreateUser(ctx, User{Email: email, Name: strings.TrimSpace(name), Role: RoleAdmin})
		switch {
		case err == nil:
			return u, true, nil
		case !errors.Is(err, ErrConflict):
			return User{}, false, fmt.Errorf("db: adding the bootstrap admin: %w", err)
		}
		// Another replica starting at the same moment added them first.
	}
	u, err = s.GetUserByEmail(ctx, email)
	if errors.Is(err, ErrNotFound) {
		return User{}, false, nil
	}
	return u, false, err
}
