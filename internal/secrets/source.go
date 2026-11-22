package secrets

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
)

// UnwrapKeySource gives the node the unwrap key (SEC-26, §3).
type UnwrapKeySource interface {
	// Name says where the key comes from, as ParseSource reads it.
	Name() string
	// Load reads the key; the caller clears it.
	Load(ctx context.Context) ([]byte, error)
}

// ParseSource reads "file:<path>" or "systemd-credential:<name>".
func ParseSource(s string) (UnwrapKeySource, error) {
	kind, arg, _ := strings.Cut(s, ":")
	switch {
	case arg == "":
	case kind == "file":
		return FileSource{Path: arg}, nil
	case kind == "systemd-credential":
		if strings.ContainsRune(arg, '/') {
			return nil, fmt.Errorf("secrets: credential name %q has a slash", arg)
		}
		return CredentialSource{Credential: arg}, nil
	}
	return nil, fmt.Errorf("secrets: unwrap key %q: want file:<path> or systemd-credential:<name>", s)
}

// FileSource reads the key from a file of 32 bytes, owned by root or by the
// process's user and accessible by them only.
type FileSource struct{ Path string }

// Name implements UnwrapKeySource.
func (s FileSource) Name() string { return "file:" + s.Path }

// Load implements UnwrapKeySource.
func (s FileSource) Load(context.Context) ([]byte, error) {
	f, err := os.Open(s.Path)
	if err != nil {
		return nil, fmt.Errorf("secrets: unwrap key: %w", err)
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("secrets: unwrap key: %w", err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	switch {
	case !fi.Mode().IsRegular():
		return nil, fmt.Errorf("secrets: unwrap key %s is not a regular file", s.Path)
	case fi.Mode().Perm()&0o077 != 0:
		return nil, fmt.Errorf("secrets: unwrap key %s must not be accessible by group or others (mode %v)", s.Path, fi.Mode().Perm())
	case !ok || (st.Uid != 0 && int(st.Uid) != os.Geteuid()):
		return nil, fmt.Errorf("secrets: unwrap key %s must be owned by root or by this process's user", s.Path)
	}
	return readKey(f, s.Name())
}

// CredentialSource reads the key from a systemd credential, which systemd
// decrypts (LoadCredentialEncrypted=) into $CREDENTIALS_DIRECTORY.
type CredentialSource struct{ Credential string }

// Name implements UnwrapKeySource.
func (s CredentialSource) Name() string { return "systemd-credential:" + s.Credential }

// Load implements UnwrapKeySource.
func (s CredentialSource) Load(context.Context) ([]byte, error) {
	dir := os.Getenv("CREDENTIALS_DIRECTORY")
	if dir == "" {
		return nil, errors.New("secrets: unwrap key: $CREDENTIALS_DIRECTORY is not set: is the credential in the unit's LoadCredentialEncrypted=?")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("secrets: unwrap key: %w", err)
	}
	defer func() { _ = root.Close() }()
	f, err := root.Open(s.Credential)
	if err != nil {
		return nil, fmt.Errorf("secrets: unwrap key: %w", err)
	}
	defer func() { _ = f.Close() }()
	return readKey(f, s.Name())
}

// readKey reads exactly keySize bytes.
func readKey(r io.Reader, name string) ([]byte, error) {
	buf := make([]byte, keySize+1)
	n, err := io.ReadFull(r, buf)
	if n != keySize || !errors.Is(err, io.ErrUnexpectedEOF) {
		clear(buf)
		if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("secrets: unwrap key %s: %w", name, err)
		}
		return nil, fmt.Errorf("secrets: unwrap key %s must hold exactly %d bytes", name, keySize)
	}
	return buf[:keySize], nil
}
