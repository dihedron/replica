package main

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dihedron/veho/pkg/plugin/process"
)

// TestSignVerify: PD-T5, a key made by genkey signs a manifest that
// process.Installed accepts with the printed public key, which pubkey
// prints again from the private key.
func TestSignVerify(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "plugin.key")
	var out bytes.Buffer
	if err := run([]string{"genkey", keyFile}, os.Getenv, &out); err != nil {
		t.Fatal(err)
	}
	pub := strings.TrimSpace(out.String())
	if fi, err := os.Stat(keyFile); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("the key file: %v %v", fi, err)
	}
	seed, _ := os.ReadFile(keyFile)
	env := func(k string) string {
		if k == "PLUGIN_SIGNING_KEY" {
			return string(seed)
		}
		return ""
	}
	var again bytes.Buffer
	if err := run([]string{"pubkey"}, env, &again); err != nil || strings.TrimSpace(again.String()) != pub {
		t.Errorf("pubkey: %q %v, want %q", again.String(), err, pub)
	}
	bin := filepath.Join(dir, "veho-plugin-acme")
	if err := os.WriteFile(bin, []byte("#!/bin/true\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"sign", "-name", "acme", "-version", "1.0.0", bin}, env, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	keys, err := process.ParseKeys([]byte(pub))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := process.Installed(dir, "acme", "1.0.0", keys, false); err != nil {
		t.Errorf("verify: %v", err)
	}
	if _, err := process.Installed(dir, "acme", "1.0.1", keys, false); err == nil {
		t.Error("the default range is the plugin's version alone")
	}
	if err := run([]string{"sign", "-name", "acme", "-version", "2.0.0", "-veho", "[1.0.0,2.0.0)", bin}, env, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if _, err := process.Installed(dir, "acme", "1.5.0", keys, false); err != nil {
		t.Errorf("veho 1.5.0 in [1.0.0,2.0.0): %v", err)
	}
	if err := run([]string{"sign", "-name", "acme", "-version", "2.0.0", "-veho", "1.0.0", bin}, env, &bytes.Buffer{}); err == nil {
		t.Error("a malformed range was signed")
	}
	bad := func(string) string { return base64.StdEncoding.EncodeToString([]byte("x")) }
	if err := run([]string{"sign", "-name", "acme", "-version", "1.0.0", bin}, bad, &bytes.Buffer{}); err == nil {
		t.Error("a bad key signed")
	}
	if err := run([]string{"sign", "-name", "acme", "-version", "1.0.0", bin}, func(string) string { return "" }, &bytes.Buffer{}); err == nil {
		t.Error("no key and no -allow-unsigned signed")
	}
	if err := run([]string{"genkey", keyFile}, os.Getenv, &bytes.Buffer{}); err == nil {
		t.Error("genkey overwrote a key")
	}
}

// TestUsage: the malformed command lines and keys are refused; a snapshot
// build signs with a throwaway key into -out.
func TestUsage(t *testing.T) {
	none := func(string) string { return "" }
	bad := func(string) string { return "not base64!" }
	for _, args := range [][]string{
		nil,
		{"genkey"},
		{"pubkey"},
		{"sign"},
		{"sign", "-bogus"},
		{"sign", "-name", "acme", "BINARY"},
		{"frobnicate"},
	} {
		if err := run(args, bad, &bytes.Buffer{}); err == nil {
			t.Errorf("%q: accepted", args)
		}
	}
	dir := t.TempDir()
	if err := run([]string{"genkey", filepath.Join(dir, "missing", "key")}, none, &bytes.Buffer{}); err == nil {
		t.Error("genkey into a missing directory")
	}
	if err := run([]string{"sign", "-name", "acme", "-version", "1.0.0", "-allow-unsigned", filepath.Join(dir, "nothere")}, none, &bytes.Buffer{}); err == nil {
		t.Error("signed a missing binary")
	}
	bin := filepath.Join(dir, "veho-plugin-acme")
	if err := os.WriteFile(bin, []byte("#!/bin/true\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out")
	if err := run([]string{"sign", "-name", "acme", "-version", "1.0.0", "-allow-unsigned", "-out", out, bin}, none, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(out, process.Binary("acme")+".manifest")); err != nil {
		t.Errorf("no manifest in -out: %v", err)
	}
}
