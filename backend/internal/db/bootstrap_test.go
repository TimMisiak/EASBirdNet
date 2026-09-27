package db

import (
	"context"
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
