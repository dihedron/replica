// plugin-manifest writes and signs the manifest of a plugin binary of a
// release, and creates the signing key (.specs/veho-plugin-distribution.md
// §3.3). Standard library only.
//
//	plugin-manifest genkey FILE                    the private key to FILE (0600), the public key to stdout
//	plugin-manifest pubkey                         the public key of $PLUGIN_SIGNING_KEY
//	plugin-manifest sign -name N -version V [-out DIR] [-allow-unsigned] BINARY
//	    the manifest and signature into DIR (default: next to BINARY), signed
//	    with the private key in $PLUGIN_SIGNING_KEY; with -allow-unsigned and
//	    no key (snapshot builds), with a throwaway key no veho accepts
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/dihedron/veho/pkg/plugin/process"
)

func main() {
	if err := run(os.Args[1:], os.Getenv, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "plugin-manifest:", err)
		os.Exit(1)
	}
}

func run(args []string, getenv func(string) string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: plugin-manifest genkey FILE | pubkey | sign -name N -version V [-out DIR] [-allow-unsigned] BINARY")
	}
	switch args[0] {
	case "genkey":
		if len(args) != 2 {
			return errors.New("usage: plugin-manifest genkey FILE")
		}
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return err
		}
		f, err := os.OpenFile(args[1], os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // the operator names the key file
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintln(f, base64.StdEncoding.EncodeToString(priv.Seed())); err != nil {
			_ = f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		_, err = fmt.Fprintln(stdout, base64.StdEncoding.EncodeToString(pub))
		return err
	case "pubkey":
		seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(getenv("PLUGIN_SIGNING_KEY")))
		if err != nil || len(seed) != ed25519.SeedSize {
			return errors.New("PLUGIN_SIGNING_KEY must hold the base64 private key of plugin-manifest genkey")
		}
		_, err = fmt.Fprintln(stdout, base64.StdEncoding.EncodeToString(ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)))
		return err
	case "sign":
		fs := flag.NewFlagSet("sign", flag.ContinueOnError)
		name := fs.String("name", "", "the driver's name")
		version := fs.String("version", "", "the release's version")
		out := fs.String("out", "", "the directory of the manifest (default: the binary's)")
		unsigned := fs.Bool("allow-unsigned", false, "without PLUGIN_SIGNING_KEY, sign with a throwaway key")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *name == "" || *version == "" || fs.NArg() != 1 {
			return errors.New("usage: plugin-manifest sign -name N -version V BINARY")
		}
		env := strings.TrimSpace(getenv("PLUGIN_SIGNING_KEY"))
		var key ed25519.PrivateKey
		if env == "" && *unsigned {
			_, k, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				return err
			}
			key = k
			fmt.Fprintln(os.Stderr, "plugin-manifest: no PLUGIN_SIGNING_KEY: signed with a throwaway key")
		} else {
			seed, err := base64.StdEncoding.DecodeString(env)
			if err != nil || len(seed) != ed25519.SeedSize {
				return errors.New("PLUGIN_SIGNING_KEY must hold the base64 private key of plugin-manifest genkey")
			}
			key = ed25519.NewKeyFromSeed(seed)
		}
		data, err := os.ReadFile(fs.Arg(0))
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		dir := *out
		if dir == "" {
			dir = filepath.Dir(fs.Arg(0))
		}
		if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // build output
			return err
		}
		return process.WriteManifest(dir, key, process.Manifest{
			Name: *name, Version: *version, SHA256: hex.EncodeToString(sum[:]), Protocol: process.Handshake.ProtocolVersion,
		})
	}
	return fmt.Errorf("unknown command %q", args[0])
}
