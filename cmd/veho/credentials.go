package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// login is a person's OIDC login, cached for one API URL
// (veho-authentication.md §7). It is sent to that URL only.
type login struct {
	IssuerName string    `json:"issuer_name"`
	Issuer     string    `json:"issuer"`
	ClientID   string    `json:"client_id"`
	Scopes     []string  `json:"scopes"`
	IdPCAFile  string    `json:"idp_ca_file,omitempty"`
	Device     bool      `json:"device,omitempty"`
	Access     string    `json:"access_token"`
	Refresh    string    `json:"refresh_token,omitempty"`
	Expiry     time.Time `json:"expiry"`
	Tenant     string    `json:"tenant,omitempty"`
}

// credentials is the cache file: the logins by API URL.
type credentials struct {
	Logins map[string]*login `json:"logins"`
}

// credentialsDir is $XDG_CONFIG_HOME/veho, or ~/.config/veho.
func credentialsDir() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("credentials: %w", err)
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "veho"), nil
}

// private refuses a file or directory others can reach (C8-3): the cache
// holds tokens, and its mode is the person's to fix, not ours to change.
func private(path string, dir bool) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	want := fs.FileMode(0o600)
	if dir {
		want = 0o700
	}
	if fi.Mode().Perm()&^want != 0 {
		return fmt.Errorf("credentials: %s must not be accessible by group or others (mode %v, want %v)", path, fi.Mode().Perm(), want)
	}
	return nil
}

// parseCredentials decodes the cache, refusing what is not one.
func parseCredentials(b []byte) (*credentials, error) {
	c := &credentials{}
	if err := json.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("credentials: %w", err)
	}
	if c.Logins == nil {
		c.Logins = map[string]*login{}
	}
	for url, l := range c.Logins {
		if l == nil || l.Issuer == "" || l.ClientID == "" || l.Access == "" {
			return nil, fmt.Errorf("credentials: the login of %s is incomplete", url)
		}
	}
	return c, nil
}

// loadCredentials reads the cache; a missing one is empty.
func loadCredentials() (*credentials, error) {
	dir, err := credentialsDir()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "credentials.json")
	b, err := os.ReadFile(path) //nolint:gosec // the person's own cache
	if errors.Is(err, fs.ErrNotExist) {
		return &credentials{Logins: map[string]*login{}}, nil
	} else if err != nil {
		return nil, fmt.Errorf("credentials: %w", err)
	}
	if err := private(dir, true); err != nil {
		return nil, err
	}
	if err := private(path, false); err != nil {
		return nil, err
	}
	return parseCredentials(b)
}

// save writes the cache by rename, so that a reader never sees half of it.
func (c *credentials) save() error {
	dir, err := credentialsDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("credentials: %w", err)
	}
	if err := private(dir, true); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("credentials: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "credentials-*.json") // 0600
	if err != nil {
		return fmt.Errorf("credentials: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("credentials: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("credentials: %w", err)
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, "credentials.json")); err != nil {
		return fmt.Errorf("credentials: %w", err)
	}
	return nil
}

// lockCredentials holds the cache's lock until unlock: a refresh re-reads
// the cache under it, so that two processes never present a refresh token
// that the other has already rotated.
func lockCredentials() (unlock func(), err error) {
	dir, err := credentialsDir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("credentials: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "credentials.lock"), os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // the person's own cache
	if err != nil {
		return nil, fmt.Errorf("credentials: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil { //nolint:gosec // a file descriptor
		_ = f.Close()
		return nil, fmt.Errorf("credentials: lock: %w", err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:gosec // a file descriptor
		_ = f.Close()
	}, nil
}
