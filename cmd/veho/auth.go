package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	vehov1 "github.com/dihedron/veho/api/veho/v1"
	"github.com/dihedron/veho/internal/client"
	"golang.org/x/oauth2"
)

// AuthCommand is "veho auth" (veho-authentication.md §7).
type AuthCommand struct {
	Login  AuthLoginCommand  `command:"login" description:"Sign in with the identity provider of the API, and cache the tokens for this API URL"`
	Logout AuthLogoutCommand `command:"logout" description:"Forget this API URL's login, and revoke its refresh token"`
	Status AuthStatusCommand `command:"status" description:"Show who you are, your roles and the login's expiry"`
}

// AuthLoginCommand is "veho auth login".
type AuthLoginCommand struct {
	apiOptions
	Issuer    string `long:"issuer" description:"The issuer's name, when the API offers several"`
	Device    bool   `long:"device" description:"The device flow, for a host without a browser"`
	IdPCAFile string `long:"idp-ca-file" env:"VEHO_IDP_CA_FILE" description:"The CA of the identity provider's certificate (default: the system's roots)"`
}

// loginTimeout bounds the wait for the browser's redirect; a variable for
// the tests.
var loginTimeout = 5 * time.Minute

// openBrowser shows the person the issuer's login page; the tests replace
// it with a client that follows the redirects.
var openBrowser = func(u string) error { return exec.Command("xdg-open", u).Start() } //nolint:gosec // the issuer's URL, also printed

// Execute signs in and caches the tokens.
func (cmd *AuthLoginCommand) Execute([]string) error {
	ctx, cancel := context.WithTimeout(context.Background(), loginTimeout+time.Minute)
	defer cancel()
	c, err := client.New(client.Options{URL: cmd.URL, CAFile: cmd.CAFile, REST: cmd.API == "rest", Anonymous: true})
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	m, _ := client.Method("veho.v1.AuthService.GetConfig")
	conf := &vehov1.AuthConfig{}
	if err := c.Call(ctx, m, &vehov1.GetConfigRequest{}, conf); err != nil {
		return err
	}
	var iss *vehov1.AuthIssuer
	for _, i := range conf.GetIssuers() {
		if cmd.Issuer == "" || i.GetName() == cmd.Issuer {
			if iss != nil {
				return errors.New("auth login: the API offers several issuers: name one with --issuer")
			}
			iss = i
		}
	}
	if iss == nil && cmd.Issuer != "" {
		return fmt.Errorf("auth login: the API offers no CLI login by %s", cmd.Issuer)
	} else if iss == nil {
		return errors.New("auth login: the API offers no CLI login")
	}
	l := &login{IssuerName: iss.GetName(), Issuer: iss.GetIssuer(), ClientID: iss.GetCliClientId(), Scopes: iss.GetScopes(),
		IdPCAFile: cmd.IdPCAFile, Device: cmd.Device, Tenant: cmd.Tenant}
	if err := signIn(ctx, l, cmd.stderr(), "", ""); err != nil {
		return err
	}
	return storeLogin(cmd.URL, l)
}

func (o *apiOptions) stderr() io.Writer {
	if o.out != nil {
		return o.out
	}
	return os.Stderr
}

// storeLogin caches l for apiURL.
func storeLogin(apiURL string, l *login) error {
	unlock, err := lockCredentials()
	if err != nil {
		return err
	}
	defer unlock()
	creds, err := loadCredentials()
	if err != nil {
		return err
	}
	creds.Logins[apiURL] = l
	return creds.save()
}

// oauthConfig is l's OAuth client, and the context whose HTTP client trusts
// the issuer.
func oauthConfig(ctx context.Context, l *login) (*oauth2.Config, *oidc.Provider, context.Context, error) {
	hc := &http.Client{Timeout: 30 * time.Second}
	if l.IdPCAFile != "" {
		pem, err := os.ReadFile(l.IdPCAFile)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("auth: %w", err)
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(pem) {
			return nil, nil, nil, fmt.Errorf("auth: no certificate in %s", l.IdPCAFile)
		}
		hc.Transport = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}
	}
	ctx = oidc.ClientContext(context.WithValue(ctx, oauth2.HTTPClient, hc), hc)
	p, err := oidc.NewProvider(ctx, l.Issuer)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("auth: the issuer %s: %w", l.Issuer, err)
	}
	return &oauth2.Config{ClientID: l.ClientID, Endpoint: p.Endpoint(), Scopes: l.Scopes}, p, ctx, nil
}

// signIn runs l's flow, asking for acrValues and maxAge when given (a
// step-up), and puts the tokens in l.
func signIn(ctx context.Context, l *login, w io.Writer, acrValues, maxAge string) error {
	conf, _, ctx, err := oauthConfig(ctx, l)
	if err != nil {
		return err
	}
	var opts []oauth2.AuthCodeOption
	if acrValues != "" {
		opts = append(opts, oauth2.SetAuthURLParam("acr_values", acrValues))
	}
	if maxAge != "" {
		opts = append(opts, oauth2.SetAuthURLParam("max_age", maxAge))
	}
	host := l.Issuer
	if u, err := url.Parse(l.Issuer); err == nil {
		host = u.Host
	}
	_, _ = fmt.Fprintf(w, "Signing in at %s (%s)\n", host, l.IssuerName)
	var tok *oauth2.Token
	if l.Device {
		tok, err = deviceLogin(ctx, conf, w, opts)
	} else {
		tok, err = loopbackLogin(ctx, conf, w, opts)
	}
	if err != nil {
		return err
	}
	l.Access, l.Refresh, l.Expiry = tok.AccessToken, tok.RefreshToken, tok.Expiry
	return nil
}

func randomState() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// loopbackLogin is the authorization code flow with PKCE: the browser comes
// back to 127.0.0.1, where one request with the right state is accepted.
func loopbackLogin(ctx context.Context, conf *oauth2.Config, w io.Writer, opts []oauth2.AuthCodeOption) (*oauth2.Token, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("auth: %w", err)
	}
	defer func() { _ = ln.Close() }()
	c := *conf
	c.RedirectURL = fmt.Sprintf("http://127.0.0.1:%d/callback", ln.Addr().(*net.TCPAddr).Port)
	state, verifier := randomState(), oauth2.GenerateVerifier()
	codes := make(chan string, 1)
	srv := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/callback" || subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(state)) != 1 || q.Get("code") == "" {
			http.Error(rw, "this is not the login veho is waiting for", http.StatusBadRequest)
			return
		}
		select {
		case codes <- q.Get("code"):
			_, _ = io.WriteString(rw, "Signed in to veho: you can close this window.\n")
		default:
			http.Error(rw, "already signed in", http.StatusConflict)
		}
	})}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()
	authURL := c.AuthCodeURL(state, append(opts, oauth2.S256ChallengeOption(verifier))...)
	_, _ = fmt.Fprintf(w, "Open this page if no browser opens:\n  %s\n", authURL)
	if err := openBrowser(authURL); err != nil {
		_, _ = fmt.Fprintf(w, "(no browser: %v)\n", err)
	}
	select {
	case code := <-codes:
		tok, err := c.Exchange(ctx, code, oauth2.VerifierOption(verifier))
		if err != nil {
			return nil, fmt.Errorf("auth: %w", err)
		}
		return tok, nil
	case <-time.After(loginTimeout):
		return nil, errors.New("auth: no login within the time allowed")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// deviceLogin is the device flow (RFC 8628).
func deviceLogin(ctx context.Context, conf *oauth2.Config, w io.Writer, opts []oauth2.AuthCodeOption) (*oauth2.Token, error) {
	if conf.Endpoint.DeviceAuthURL == "" {
		return nil, errors.New("auth: the issuer has no device flow")
	}
	da, err := conf.DeviceAuth(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("auth: %w", err)
	}
	u := da.VerificationURIComplete
	if u == "" {
		u = da.VerificationURI
	}
	_, _ = fmt.Fprintf(w, "On any device, open %s and enter the code %s\n", u, da.UserCode)
	_, _ = fmt.Fprintln(w, "Never enter a code that somebody else sent you: it would sign them in as you.")
	tctx, cancel := context.WithTimeout(ctx, loginTimeout)
	defer cancel()
	tok, err := conf.DeviceAccessToken(tctx, da)
	if err != nil {
		return nil, fmt.Errorf("auth: %w", err)
	}
	return tok, nil
}

// errLoginEnded asks the person to sign in again.
var errLoginEnded = errors.New("the login has ended: run veho auth login")

// cachedToken is the access token cached for apiURL, refreshed under the
// cache's lock when it ends within 30 seconds; "" when there is no login.
func cachedToken(ctx context.Context, apiURL string) (string, *login, error) {
	creds, err := loadCredentials()
	if err != nil {
		return "", nil, err
	}
	l := creds.Logins[apiURL]
	if l == nil {
		return "", nil, nil
	}
	if time.Until(l.Expiry) > 30*time.Second {
		return l.Access, l, nil
	}
	unlock, err := lockCredentials()
	if err != nil {
		return "", nil, err
	}
	defer unlock()
	// another process may have refreshed it while this one waited
	if creds, err = loadCredentials(); err != nil {
		return "", nil, err
	}
	if l = creds.Logins[apiURL]; l == nil {
		return "", nil, nil
	}
	if time.Until(l.Expiry) > 30*time.Second {
		return l.Access, l, nil
	}
	if l.Refresh == "" {
		return "", nil, errLoginEnded
	}
	conf, _, octx, err := oauthConfig(ctx, l)
	if err != nil {
		return "", nil, err
	}
	tok, err := conf.TokenSource(octx, &oauth2.Token{RefreshToken: l.Refresh, Expiry: time.Now().Add(-time.Second)}).Token()
	if err != nil {
		if re, ok := errors.AsType[*oauth2.RetrieveError](err); ok && re.ErrorCode == "invalid_grant" {
			return "", nil, errLoginEnded
		}
		return "", nil, fmt.Errorf("auth: refresh: %w", err)
	}
	l.Access, l.Expiry = tok.AccessToken, tok.Expiry
	if tok.RefreshToken != "" {
		l.Refresh = tok.RefreshToken
	}
	if err := creds.save(); err != nil {
		return "", nil, err
	}
	return l.Access, l, nil
}

// AuthLogoutCommand is "veho auth logout".
type AuthLogoutCommand struct{ apiOptions }

// Execute revokes the refresh token, when the issuer can, and forgets the
// login.
func (cmd *AuthLogoutCommand) Execute([]string) error {
	unlock, err := lockCredentials()
	if err != nil {
		return err
	}
	defer unlock()
	creds, err := loadCredentials()
	if err != nil {
		return err
	}
	l := creds.Logins[cmd.URL]
	if l == nil {
		return fmt.Errorf("auth logout: no login for %s", cmd.URL)
	}
	if l.Refresh != "" {
		if err := revoke(l); err != nil {
			_, _ = fmt.Fprintf(cmd.stderr(), "warning: the refresh token was not revoked: %v\n", err)
		}
	}
	delete(creds.Logins, cmd.URL)
	return creds.save()
}

// revoke revokes l's refresh token (RFC 7009).
func revoke(l *login) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, p, octx, err := oauthConfig(ctx, l)
	if err != nil {
		return err
	}
	var meta struct {
		Revocation string `json:"revocation_endpoint"`
	}
	if err := p.Claims(&meta); err != nil || meta.Revocation == "" {
		return errors.New("the issuer offers no revocation")
	}
	form := url.Values{"token": {l.Refresh}, "token_type_hint": {"refresh_token"}, "client_id": {l.ClientID}}
	req, err := http.NewRequestWithContext(octx, http.MethodPost, meta.Revocation, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	hc, _ := octx.Value(oauth2.HTTPClient).(*http.Client)
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the issuer answered %s", resp.Status)
	}
	return nil
}

// AuthStatusCommand is "veho auth status".
type AuthStatusCommand struct{ apiOptions }

// Execute prints WhoAmI, and the cached login's expiry.
func (cmd *AuthStatusCommand) Execute([]string) error {
	if err := cmd.show("veho.v1.IdentityService.WhoAmI", &vehov1.WhoAmIRequest{}, &vehov1.Identity{}); err != nil {
		return err
	}
	creds, err := loadCredentials()
	if err != nil {
		return err
	}
	if l := creds.Logins[cmd.URL]; l != nil && cmd.Output != "json" {
		refresh := ""
		if l.Refresh != "" {
			refresh = ", refreshable"
		}
		_, _ = fmt.Fprintf(cmd.stdout(), "login: %s, until %s%s\n", l.IssuerName, l.Expiry.Local().Format(time.RFC3339), refresh)
	}
	return nil
}
