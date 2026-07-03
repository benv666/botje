package auth

import (
	"slices"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"go-botje/internal/storage"
)

func newAuth(t *testing.T) (*Auth, storage.Store) {
	t.Helper()
	store := storage.NewMemory()
	a, err := New(store)
	if err != nil {
		t.Fatal(err)
	}
	return a, store
}

func TestAddAndCheckUser(t *testing.T) {
	a, _ := newAuth(t)
	if err := a.AddUser("benv", "geheim"); err != nil {
		t.Fatal(err)
	}
	if got := a.Check("benv", "geheim"); got != Valid {
		t.Errorf("Check valid = %v, want Valid", got)
	}
	if got := a.Check("benv", "wrong"); got != BadPass {
		t.Errorf("Check bad pass = %v, want BadPass", got)
	}
	if got := a.Check("ghost", "x"); got != NoSuchUser {
		t.Errorf("Check unknown = %v, want NoSuchUser", got)
	}
}

func TestSuperuser(t *testing.T) {
	a, _ := newAuth(t)
	hash, err := bcrypt.GenerateFromPassword([]byte("supergeheim"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	a.SetSuperuser("benv", string(hash))
	if got := a.Check("benv", "supergeheim"); got != Super {
		t.Errorf("Check superuser = %v, want Super", got)
	}
	if got := a.Check("benv", "nope"); got != BadPass {
		t.Errorf("Check superuser bad pass = %v, want BadPass", got)
	}
}

func TestPersistedImmediately(t *testing.T) {
	a, store := newAuth(t)
	if err := a.AddUser("benv", "geheim"); err != nil {
		t.Fatal(err)
	}
	// a second Auth over the same store sees the user: no
	// flush-on-unload-only nonsense
	b, err := New(store)
	if err != nil {
		t.Fatal(err)
	}
	if got := b.Check("benv", "geheim"); got != Valid {
		t.Errorf("fresh Auth Check = %v, want Valid", got)
	}
}

func TestSetPassword(t *testing.T) {
	a, _ := newAuth(t)
	a.AddUser("benv", "old")
	if err := a.SetPassword("benv", "new"); err != nil {
		t.Fatal(err)
	}
	if got := a.Check("benv", "old"); got != BadPass {
		t.Errorf("old password still works: %v", got)
	}
	if got := a.Check("benv", "new"); got != Valid {
		t.Errorf("new password rejected: %v", got)
	}
	if err := a.SetPassword("ghost", "x"); err == nil {
		t.Error("SetPassword for unknown user did not error")
	}
}

func TestAddUserDuplicate(t *testing.T) {
	a, _ := newAuth(t)
	a.AddUser("benv", "x")
	if err := a.AddUser("benv", "y"); err == nil {
		t.Error("duplicate AddUser did not error")
	}
}

func TestDeleteUser(t *testing.T) {
	a, store := newAuth(t)
	a.AddUser("benv", "x")
	if err := a.DeleteUser("benv"); err != nil {
		t.Fatal(err)
	}
	if got := a.Check("benv", "x"); got != NoSuchUser {
		t.Errorf("Check after delete = %v, want NoSuchUser", got)
	}
	b, _ := New(store)
	if got := b.Check("benv", "x"); got != NoSuchUser {
		t.Errorf("delete not persisted: %v", got)
	}
}

func TestUsersSorted(t *testing.T) {
	a, _ := newAuth(t)
	a.AddUser("zed", "x")
	a.AddUser("anna", "x")
	if got := a.Users(); !slices.Equal(got, []string{"anna", "zed"}) {
		t.Errorf("Users = %v", got)
	}
}
