//go:build integration

package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/dihedron/veho/internal/audit"
	"github.com/dihedron/veho/internal/client"
	"github.com/dihedron/veho/internal/testidp"
)

// authLab is a node whose API trusts a test IdP, issuer corp, which asks
// the sensitive methods for acr 2; alice is the admin of tenant.
type authLab struct {
	idp     *testidp.IdP
	url, ca string
	tenant  string
	browser *http.Client // follows the IdP's redirect to the loopback
}

func setupAuth(t *testing.T) *authLab {
	t.Helper()
	idp := testidp.New(t, "veho-api", "veho-cli")
	url, ca := node(t, `oidc: [{name: corp, issuer: "`+idp.URL+`", audience: veho-api, ca_file: "`+idp.CAFile+`", cli_client_id: veho-cli,
    step_up: {acr_values: ["2"], max_age: 10m}}]`)
	a := &authLab{idp: idp, url: url, ca: ca, tenant: "auth-" + uuid.New().String()[:8]}
	db := dbOptions{DSNEnv: "VEHO_TEST_DATABASE_URL"}
	bt := &BootstrapTenantCommand{dbOptions: db, auditOptions: auditOptions{auditor: &audit.Recorder{}}}
	bt.Args.Name = a.tenant
	_ = stdout(t, func() error { return bt.Execute(nil) })
	be := &BootstrapEntitlementCommand{dbOptions: db, auditOptions: auditOptions{auditor: &audit.Recorder{}}, Kind: "oidc", Issuer: idp.URL,
		Subject: "alice", Tenant: a.tenant, Role: "admin"}
	_ = stdout(t, func() error { return be.Execute(nil) })

	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("VEHO_TOKEN", "")
	pem, _ := os.ReadFile(idp.CAFile)
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(pem)
	a.browser = &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}}
	browse, inter := openBrowser, interactive
	openBrowser = func(u string) error {
		resp, err := a.browser.Get(u)
		if err != nil {
			return err
		}
		return resp.Body.Close()
	}
	interactive = func() bool { return true }
	t.Cleanup(func() { openBrowser, interactive = browse, inter })
	return a
}

func (a *authLab) opts(out *bytes.Buffer) apiOptions {
	return apiOptions{URL: a.url, CAFile: a.ca, API: "grpc", Output: "text", Timeout: 30 * time.Second, out: out}
}

func (a *authLab) login(t *testing.T, device bool) {
	t.Helper()
	var out bytes.Buffer
	cmd := &AuthLoginCommand{apiOptions: a.opts(&out), Device: device, IdPCAFile: a.idp.CAFile}
	cmd.Tenant = a.tenant
	if err := cmd.Execute(nil); err != nil {
		t.Fatalf("login (device %v): %v: %s", device, err, out.String())
	}
	if !strings.Contains(out.String(), "Signing in at "+strings.TrimPrefix(a.idp.URL, "https://")) {
		t.Errorf("the issuer's host is not shown: %s", out.String())
	}
	if device && !strings.Contains(out.String(), "Never enter a code") {
		t.Errorf("no warning with the device code: %s", out.String())
	}
}

// AU-T17: loopback and device logins; the cached login serves the calls
// of its own API URL only; step-up signs in again once.
func TestAuthLogin(t *testing.T) {
	a := setupAuth(t)
	a.login(t, false)
	dir, _ := credentialsDir()
	if fi, err := os.Stat(filepath.Join(dir, "credentials.json")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("the cache: %v, %v", fi, err)
	}
	var out bytes.Buffer
	if err := (&AuthStatusCommand{apiOptions: a.opts(&out)}).Execute(nil); err != nil ||
		!strings.Contains(out.String(), "oidc:"+a.idp.URL+"#alice") || !strings.Contains(out.String(), a.tenant) {
		t.Fatalf("status: %v: %s", err, out.String())
	}
	out.Reset()
	if err := (&EndpointListCommand{apiOptions: a.opts(&out)}).Execute(nil); err != nil {
		t.Errorf("a call with the cached login, its tenant the default: %v", err)
	}

	// another API URL gets nothing of it
	other := a.opts(&out)
	other.URL = "https://127.0.0.1:18445"
	if err := (&EndpointListCommand{apiOptions: other}).Execute(nil); err == nil || !strings.Contains(err.Error(), "no credential") {
		t.Errorf("another API URL: %v", err)
	}

	// step-up: the login's acr 1 is too weak for a secret; signed in again
	// with acr_values 2, once, and the call repeated
	secret := filepath.Join(t.TempDir(), "v.json")
	_ = os.WriteFile(secret, []byte(`{"k": "v"}`), 0o600)
	_, _, before := a.idp.Counts()
	set := &SecretSetCommand{apiOptions: a.opts(&out), FromFile: secret}
	set.Args.Name = "s1"
	if err := set.Execute(nil); err != nil {
		t.Fatalf("a secret after a step-up: %v: %s", err, out.String())
	}
	if _, _, after := a.idp.Counts(); after != before+1 || a.idp.LastACR != "2" {
		t.Errorf("step-up: %d logins (want %d), acr %q", after, before+1, a.idp.LastACR)
	}
	interactive = func() bool { return false }
	a.idp.Set("alice", "1", time.Hour)
	a.login(t, true) // the device flow, acr 1 again
	set.Args.Name = "s2"
	var ce *client.Error
	if err := set.Execute(nil); !errors.As(err, &ce) || ce.Code != "step_up_required" {
		t.Errorf("step-up outside a terminal: %v", err)
	}

	// VEHO_TOKEN comes before the cache
	t.Setenv("VEHO_TOKEN", "veho_notakey")
	if err := (&EndpointListCommand{apiOptions: a.opts(&out)}).Execute(nil); err == nil {
		t.Error("VEHO_TOKEN did not take precedence over the cache")
	}
	t.Setenv("VEHO_TOKEN", "")

	// logout revokes the refresh token and forgets the login
	creds, _ := loadCredentials()
	refresh := creds.Logins[a.url].Refresh
	if err := (&AuthLogoutCommand{apiOptions: a.opts(&out)}).Execute(nil); err != nil {
		t.Fatal(err)
	}
	if creds, _ := loadCredentials(); creds.Logins[a.url] != nil || len(a.idp.Revoked) == 0 || a.idp.Revoked[len(a.idp.Revoked)-1] != refresh {
		t.Errorf("logout: %v, revoked %v", creds.Logins, a.idp.Revoked)
	}
}

// AU-T17: only the redirect with the login's state is accepted.
func TestAuthLoginState(t *testing.T) {
	a := setupAuth(t)
	var refused int
	openBrowser = func(u string) error {
		au, _ := url.Parse(u)
		redirect := au.Query().Get("redirect_uri")
		resp, err := http.Get(redirect + "?state=forged&code=stolen") //nolint:gosec,noctx // the test's loopback
		if err != nil {
			return err
		}
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusBadRequest {
			refused++
		}
		resp, err = a.browser.Get(u)
		if err != nil {
			return err
		}
		return resp.Body.Close()
	}
	a.login(t, false)
	if refused != 1 {
		t.Errorf("a forged state was accepted")
	}
}

// AU-T17: two processes refresh at once; the second uses the first's
// tokens rather than a refresh token the IdP has rotated.
func TestAuthRefresh(t *testing.T) {
	a := setupAuth(t)
	a.idp.Set("alice", "1", 10*time.Second) // within the 30 s margin: refreshed
	a.login(t, false)
	a.idp.Set("alice", "1", time.Hour)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Go(func() {
			if _, _, err := cachedToken(context.Background(), a.url); err != nil {
				errs <- err
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if refreshes, reused, _ := a.idp.Counts(); refreshes != 1 || reused != 0 {
		t.Errorf("%d refreshes, %d rotated tokens presented again", refreshes, reused)
	}

	// a cache that others can read is refused, not fixed
	dir, _ := credentialsDir()
	file := filepath.Join(dir, "credentials.json")
	_ = os.Chmod(file, 0o644) //nolint:gosec // the test
	if _, _, err := cachedToken(context.Background(), a.url); err == nil || !strings.Contains(err.Error(), "must not be accessible") {
		t.Errorf("a cache with mode 0644: %v", err)
	}
	_ = os.Chmod(file, 0o600)
	_ = os.Chmod(dir, 0o755) //nolint:gosec // the test
	if _, _, err := cachedToken(context.Background(), a.url); err == nil || !strings.Contains(err.Error(), "must not be accessible") {
		t.Errorf("a cache directory with mode 0755: %v", err)
	}
	_ = os.Chmod(dir, 0o700)

	// a refused refresh ends the login
	a.idp.Set("alice", "1", 10*time.Second)
	a.login(t, false)
	creds, _ := loadCredentials()
	creds.Logins[a.url].Refresh = "revoked"
	_ = creds.save()
	if _, _, err := cachedToken(context.Background(), a.url); !errors.Is(err, errLoginEnded) {
		t.Errorf("a refused refresh: %v", err)
	}
}
