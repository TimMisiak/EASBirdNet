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

// BackfillRoles gives every volunteer from before volunteer roles the one role
// that was all a volunteer could do then: upload cards. An admin is left as
// they are, since being one is still everything, and so is anyone who already
// holds a role. After the first start it reads the roster and writes nothing.
//
// A volunteer with no roles can't otherwise exist -- the roster routes refuse
// one -- so this also mends a document an older image rewrote without its
// roles after a rollback (ROLLBACK.md), at the cost of that person coming back
// as a field volunteer whatever they were.
func BackfillRoles(ctx context.Context, s Store) (updated []User, err error) {
	users, err := s.ListUsers(ctx)
	if err != nil {
		return nil, fmt.Errorf("db: reading the roster: %w", err)
	}
	for _, u := range users {
		if u.Role == RoleAdmin || len(u.Roles) > 0 {
			continue
		}
		done, err := s.UpdateUser(ctx, u.ID, func(u *User) error {
			if u.Role != RoleAdmin && len(u.Roles) == 0 {
				u.Roles = []string{RoleFieldVolunteer}
			}
			return nil
		})
		if err != nil {
			return updated, fmt.Errorf("db: giving %s a role: %w", u.Email, err)
		}
		updated = append(updated, done)
	}
	return updated, nil
}
