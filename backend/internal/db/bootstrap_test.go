package db

import (
	"context"
	"slices"
	"testing"
)

func TestBootstrapAdmin(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)

	u, created, err := BootstrapAdmin(ctx, s, " Ada Admin ", "Ada@Audubon.test")
	if err != nil || !created {
		t.Fatalf("bootstrap on an empty roster = created %v, %v; want created", created, err)
	}
	if u.Email != "ada@audubon.test" || u.Name != "Ada Admin" || u.Role != RoleAdmin {
		t.Errorf("bootstrapped user = %+v, want admin Ada Admin <ada@audubon.test>", u)
	}

	// A restart with the setting still in place finds them and changes nothing,
	// even after they were demoted.
	if _, err := s.UpdateUser(ctx, u.ID, func(u *User) error { u.Role = RoleVolunteer; return nil }); err != nil {
		t.Fatal(err)
	}
	again, created, err := BootstrapAdmin(ctx, s, "Ada Admin", "ada@audubon.test")
	if err != nil || created || again.ID != u.ID || again.Role != RoleVolunteer {
		t.Errorf("bootstrap again = %+v, created %v, %v; want the demoted user untouched", again, created, err)
	}

	// A different address on a non-empty roster is not added.
	other, created, err := BootstrapAdmin(ctx, s, "Someone Else", "else@audubon.test")
	if err != nil || created || other.ID != "" {
		t.Errorf("bootstrap on a non-empty roster = %+v, created %v, %v; want nothing", other, created, err)
	}
	if users, _ := s.ListUsers(ctx); len(users) != 1 {
		t.Errorf("roster has %d users, want 1", len(users))
	}

	if _, _, err := BootstrapAdmin(ctx, s, "No Address", "  "); err == nil {
		t.Error("a bootstrap admin with no email was accepted")
	}
}

func TestBackfillRoles(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	create := func(u User) User {
		t.Helper()
		u, err := s.CreateUser(ctx, u)
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	// As the roster stood before volunteer roles: a role and nothing else.
	admin := create(User{Email: "admin@audubon.test", Role: RoleAdmin})
	volunteer := create(User{Email: "vol@audubon.test", Role: RoleVolunteer})
	// One added since, who mustn't be made a field volunteer as well.
	reviewer := create(User{Email: "rev@audubon.test", Role: RoleVolunteer, Roles: []string{RoleReviewer}})

	updated, err := BackfillRoles(ctx, s)
	if err != nil || len(updated) != 1 || updated[0].ID != volunteer.ID {
		t.Fatalf("backfill = %+v, %v; want the one volunteer without roles", updated, err)
	}
	for _, tc := range []struct {
		id   string
		want []string
	}{
		{admin.ID, nil},
		{volunteer.ID, []string{RoleFieldVolunteer}},
		{reviewer.ID, []string{RoleReviewer}},
	} {
		if u, _ := s.GetUser(ctx, tc.id); !slices.Equal(u.Roles, tc.want) {
			t.Errorf("%s's roles = %v, want %v", u.Email, u.Roles, tc.want)
		}
	}

	// The next start has nothing to do.
	if updated, err := BackfillRoles(ctx, s); err != nil || len(updated) != 0 {
		t.Errorf("backfill again = %+v, %v; want nothing", updated, err)
	}
}

func TestHas(t *testing.T) {
	for _, tc := range []struct {
		user User
		role string
		want bool
	}{
		{User{Role: RoleAdmin}, RoleAdmin, true},
		{User{Role: RoleAdmin}, RoleBirdingExpert, true},
		{User{Role: RoleVolunteer, Roles: VolunteerRoles}, RoleAdmin, false},
		{User{Role: RoleVolunteer, Roles: []string{RoleFieldVolunteer}}, RoleFieldVolunteer, true},
		{User{Role: RoleVolunteer, Roles: []string{RoleFieldVolunteer}}, RoleReviewer, false},
		{User{Role: RoleVolunteer, Roles: []string{RoleReviewer}}, RoleReviewer, true},
		{User{Role: RoleVolunteer, Roles: []string{RoleReviewer}}, RoleBirdingExpert, false},
		// An expert is a reviewer too.
		{User{Role: RoleVolunteer, Roles: []string{RoleBirdingExpert}}, RoleReviewer, true},
		{User{Role: RoleVolunteer, Roles: []string{RoleBirdingExpert}}, RoleFieldVolunteer, false},
	} {
		if got := tc.user.Has(tc.role); got != tc.want {
			t.Errorf("%s with %v Has(%s) = %v, want %v", tc.user.Role, tc.user.Roles, tc.role, got, tc.want)
		}
	}
}

func TestNormalizeRoles(t *testing.T) {
	got, err := NormalizeRoles([]string{RoleBirdingExpert, RoleFieldVolunteer, RoleBirdingExpert})
	if want := []string{RoleFieldVolunteer, RoleBirdingExpert}; err != nil || !slices.Equal(got, want) {
		t.Errorf("NormalizeRoles = %v, %v; want %v", got, err, want)
	}
	if got, err := NormalizeRoles(nil); err != nil || got == nil || len(got) != 0 {
		t.Errorf("NormalizeRoles(nil) = %#v, %v; want an empty list", got, err)
	}
	if _, err := NormalizeRoles([]string{RoleAdmin}); err == nil {
		t.Error("admin was accepted as a volunteer role")
	}
}
