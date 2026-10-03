package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"log"
	"net/http"

	"golang.org/x/crypto/bcrypt"
)

// authn authenticates Basic-auth requests and returns the project scope
// ("*" = all projects) the caller is allowed to touch.
type authn struct {
	store    Store
	disabled bool

	// optional bootstrap admin from flags/env (scope "*"); stored as hashes
	// so the comparison is constant-time regardless of input length
	hasAdmin  bool
	adminUser [32]byte
	adminPass [32]byte

	dummy []byte // bcrypt hash compared against for unknown users (blunts user enumeration by timing)
}

func newAuthn(store Store, disabled bool, adminUser, adminPass string) *authn {
	a := &authn{store: store, disabled: disabled}
	if adminUser != "" {
		a.hasAdmin = true
		a.adminUser = sha256.Sum256([]byte(adminUser))
		a.adminPass = sha256.Sum256([]byte(adminPass))
	}
	a.dummy, _ = bcrypt.GenerateFromPassword([]byte("dummy-password"), bcrypt.DefaultCost)
	return a
}

func (a *authn) authenticate(r *http.Request) (scope string, ok bool) {
	if a.disabled {
		return "*", true
	}
	u, p, has := r.BasicAuth()
	if !has {
		return "", false
	}

	if a.hasAdmin {
		gu, gp := sha256.Sum256([]byte(u)), sha256.Sum256([]byte(p))
		if subtle.ConstantTimeCompare(gu[:], a.adminUser[:])&subtle.ConstantTimeCompare(gp[:], a.adminPass[:]) == 1 {
			return "*", true
		}
	}

	cred, err := a.store.GetCredential(r.Context(), u)
	hash := a.dummy
	if err == nil {
		hash = []byte(cred.PasswordHash)
	} else if !errors.Is(err, ErrNotFound) {
		log.Printf("credential lookup: %v", err)
	}
	cmp := bcrypt.CompareHashAndPassword(hash, []byte(p)) // always runs
	if err != nil || cmp != nil {
		return "", false
	}
	return cred.Project, true
}
