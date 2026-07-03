// Package auth is the user database: the Perl Auth module's binary
// privilege model (superuser, valid user, bad password, no such user),
// with bcrypt instead of md5 and immediate persistence instead of
// flush-on-unload (both locked decisions: no md5 migration, BenV
// resets the admin password at cutover). Superusers come from config,
// never from the stored user list.
package auth

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"golang.org/x/crypto/bcrypt"

	"go-botje/internal/storage"
)

// Result is the Perl authUser return protocol.
type Result int

const (
	NoSuchUser Result = iota
	BadPass
	Valid
	Super
)

const (
	ns       = "Auth"
	usersKey = "users"
)

// Auth checks and manages users. Dispatcher goroutine only.
type Auth struct {
	store storage.Store
	users map[string]string // name -> bcrypt hash

	superName string
	superHash string
}

// New loads the user table from store (namespace "Auth").
func New(store storage.Store) (*Auth, error) {
	a := &Auth{store: store, users: make(map[string]string)}
	if _, err := store.Get(ns, usersKey, &a.users); err != nil {
		return nil, fmt.Errorf("auth: load users: %w", err)
	}
	return a, nil
}

// SetSuperuser sets the config-provided superuser name and bcrypt hash.
func (a *Auth) SetSuperuser(name, bcryptHash string) {
	a.superName, a.superHash = name, bcryptHash
}

// AddUser adds a user with a bcrypt-hashed password, persisting
// immediately. Errors if the user exists.
func (a *Auth) AddUser(name, password string) error {
	if _, ok := a.users[name]; ok {
		return fmt.Errorf("auth: user %q already exists", name)
	}
	return a.setHash(name, password)
}

// SetPassword replaces an existing user's password.
func (a *Auth) SetPassword(name, password string) error {
	if _, ok := a.users[name]; !ok {
		return fmt.Errorf("auth: no such user %q", name)
	}
	return a.setHash(name, password)
}

func (a *Auth) setHash(name, password string) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	a.users[name] = string(hash)
	return a.store.Put(ns, usersKey, a.users)
}

// DeleteUser removes a user.
func (a *Auth) DeleteUser(name string) error {
	delete(a.users, name)
	return a.store.Put(ns, usersKey, a.users)
}

// Check verifies credentials. The superuser wins over a stored user of
// the same name, like the Perl.
func (a *Auth) Check(name, password string) Result {
	if a.superName != "" && name == a.superName {
		if bcrypt.CompareHashAndPassword([]byte(a.superHash), []byte(password)) == nil {
			return Super
		}
		return BadPass
	}
	hash, ok := a.users[name]
	if !ok {
		return NoSuchUser
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil {
		return Valid
	}
	return BadPass
}

// ParseSuperuser parses a "name:password" bootstrap spec (the
// BOTJE_SUPERUSER env var). The password may be a ready bcrypt hash
// (recognized by its $2 prefix, generate one with 'botje hash') or
// plaintext, which is hashed on the spot; plaintext in the environment
// is dev convenience, not a production suggestion.
func ParseSuperuser(spec string) (name, hash string, err error) {
	name, pass, ok := strings.Cut(spec, ":")
	if !ok || name == "" || pass == "" {
		return "", "", fmt.Errorf("auth: superuser spec must be name:password or name:bcrypt-hash")
	}
	if strings.HasPrefix(pass, "$2") {
		return name, pass, nil
	}
	h, err := bcrypt.GenerateFromPassword([]byte(pass), bcrypt.DefaultCost)
	if err != nil {
		return "", "", err
	}
	return name, string(h), nil
}

// Users lists usernames, sorted.
func (a *Auth) Users() []string {
	out := slices.Collect(maps.Keys(a.users))
	slices.Sort(out)
	return out
}
