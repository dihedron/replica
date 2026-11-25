// Package openstack is what the OpenStack drivers share (.specs/veho-openstack.md
// §2): the endpoint's Keystone and catalog settings, the authentication with
// application credentials or a username and password, and the HTTP client
// every request goes through, with the dial guard, the endpoint's CA and no
// redirects.
package openstack

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack"
	"github.com/gophercloud/gophercloud/v2/openstack/identity/v3/tokens"

	"github.com/dihedron/veho/internal/netguard"
	"github.com/dihedron/veho/pkg/plugin"
)

// Timeouts of a connection; the whole of a call is bounded by its context.
const (
	dialTimeout   = 10 * time.Second
	headerTimeout = 60 * time.Second
)

// Settings are the platform's, in drivers[].settings.
type Settings struct {
	// AllowCIDRs are the ranges the dial guard allows besides the public
	// addresses (a private cloud's).
	AllowCIDRs []string `json:"allow_cidrs"`
	// Plaintext allows http for Keystone and the services: a lab, never
	// production.
	Plaintext bool `json:"plaintext"`
}

// Endpoint is the part of an endpoint's configuration that finds the cloud.
type Endpoint struct {
	AuthURL   string `json:"auth_url"`
	Region    string `json:"region"`
	Interface string `json:"interface"`
	CACert    string `json:"ca_cert"`
}

// endpoint are Endpoint's fields as JSON Schema properties.
//
//go:embed endpoint.json
var endpoint []byte

// Schema is a driver's ConfigSchema: its own schema, with Endpoint's
// properties added to its "properties" and auth_url to its "required".
func Schema(own []byte) ([]byte, error) {
	var s map[string]any
	var props map[string]any
	if err := json.Unmarshal(own, &s); err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	if err := json.Unmarshal(endpoint, &props); err != nil {
		return nil, fmt.Errorf("endpoint schema: %w", err)
	}
	mine, _ := s["properties"].(map[string]any)
	if mine == nil {
		mine = map[string]any{}
	}
	for k, v := range props {
		mine[k] = v
	}
	s["properties"] = mine
	req, _ := s["required"].([]any)
	s["required"] = append(req, "auth_url")
	return json.MarshalIndent(s, "", "  ")
}

// Service is an OpenStack service in the catalog.
type Service struct {
	name   string
	client func(*gophercloud.ProviderClient, gophercloud.EndpointOpts) (*gophercloud.ServiceClient, error)
}

// The services of the drivers.
var (
	Image       = Service{"image", openstack.NewImageV2}
	ObjectStore = Service{"object-store", openstack.NewObjectStorageV1}
)

// Cloud connects to OpenStack clouds under one driver's settings.
type Cloud struct {
	guard     *netguard.Guard
	plaintext bool
}

// NewCloud is a Cloud with the driver's settings.
func NewCloud(s Settings) (*Cloud, error) {
	g, err := netguard.New(s.AllowCIDRs)
	if err != nil {
		return nil, err
	}
	return &Cloud{guard: g, plaintext: s.Plaintext}, nil
}

type checked struct {
	authURL string
	roots   *x509.CertPool
	avail   gophercloud.Availability
}

// check validates the endpoint before any connection.
func (c *Cloud) check(ep Endpoint) (checked, error) {
	u, err := url.Parse(ep.AuthURL)
	switch {
	case err != nil || u.Host == "":
		return checked{}, plugin.Wrapf(plugin.Permanent, "auth_url: want a URL")
	case u.Scheme != "https" && (u.Scheme != "http" || !c.plaintext):
		return checked{}, plugin.Wrapf(plugin.Permanent, "auth_url: want https (http only with the driver's plaintext setting)")
	case u.User != nil || u.RawQuery != "" || u.Fragment != "":
		return checked{}, plugin.Wrapf(plugin.Permanent, "auth_url: want no user information, query or fragment")
	}
	p := strings.TrimSuffix(u.Path, "/")
	switch {
	case p == "" || strings.HasSuffix(p, "/identity"):
		p += "/v3"
	case !strings.HasSuffix(p, "/v3"):
		return checked{}, plugin.Wrapf(plugin.Permanent, "auth_url: want Keystone v3 (a path ending in /v3)")
	}
	u.Path = p + "/"
	out := checked{authURL: u.String(), avail: gophercloud.AvailabilityPublic}
	switch ep.Interface {
	case "", "public":
	case "internal":
		out.avail = gophercloud.AvailabilityInternal
	case "admin":
		out.avail = gophercloud.AvailabilityAdmin
	default:
		return checked{}, plugin.Wrapf(plugin.Permanent, "interface: want public, internal or admin")
	}
	if out.roots, err = x509.SystemCertPool(); err != nil {
		out.roots = x509.NewCertPool()
	}
	if ep.CACert != "" && !out.roots.AppendCertsFromPEM([]byte(ep.CACert)) {
		return checked{}, plugin.Wrapf(plugin.Permanent, "ca_cert: no PEM certificate")
	}
	return out, nil
}

// CheckEndpoint validates an endpoint without connecting.
func (c *Cloud) CheckEndpoint(ep Endpoint) error {
	_, err := c.check(ep)
	return err
}

// Plaintext reports whether the driver allows http.
func (c *Cloud) Plaintext() bool { return c.plaintext }

// HTTPClient is the client of every request: the dial guard on each
// connection, the endpoint's roots, no proxy (it would dial for us, past the
// guard) and no redirects (SEC-13).
func (c *Cloud) httpClient(roots *x509.CertPool) http.Client {
	return http.Client{
		Transport: &http.Transport{
			DialContext:           c.guard.Dialer(dialTimeout).DialContext,
			TLSClientConfig:       &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
			TLSHandshakeTimeout:   dialTimeout,
			ResponseHeaderTimeout: headerTimeout,
			ForceAttemptHTTP2:     true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// authOptions maps the endpoint's credentials: an application credential, or
// a username and password scoped to a project. The domains default to
// Default.
func authOptions(authURL string, creds plugin.Credentials) (gophercloud.AuthOptions, error) {
	known := map[string]bool{"application_credential_id": true, "application_credential_secret": true,
		"username": true, "password": true, "user_domain_name": true, "project_name": true, "project_domain_name": true}
	for k := range creds {
		if !known[k] {
			return gophercloud.AuthOptions{}, plugin.Wrapf(plugin.Permanent, "unknown credential %q", k)
		}
	}
	or := func(v, d string) string {
		if v == "" {
			return d
		}
		return v
	}
	opts := gophercloud.AuthOptions{IdentityEndpoint: authURL, AllowReauth: true}
	app, user := creds["application_credential_id"] != "" || creds["application_credential_secret"] != "", creds["username"] != "" || creds["password"] != ""
	switch {
	case app && !user && creds["user_domain_name"] == "" && creds["project_name"] == "" && creds["project_domain_name"] == "":
		if creds["application_credential_id"] == "" || creds["application_credential_secret"] == "" {
			return opts, plugin.Wrapf(plugin.Permanent, "credentials: application_credential_id and application_credential_secret")
		}
		opts.ApplicationCredentialID, opts.ApplicationCredentialSecret = creds["application_credential_id"], creds["application_credential_secret"]
	case user && !app:
		if creds["username"] == "" || creds["password"] == "" || creds["project_name"] == "" {
			return opts, plugin.Wrapf(plugin.Permanent, "credentials: username, password and project_name")
		}
		opts.Username, opts.Password, opts.DomainName = creds["username"], creds["password"], or(creds["user_domain_name"], "Default")
		opts.Scope = &gophercloud.AuthScope{ProjectName: creds["project_name"], DomainName: or(creds["project_domain_name"], "Default")}
	default:
		return opts, plugin.Wrapf(plugin.Permanent, "credentials: an application credential, or a username and password")
	}
	return opts, nil
}

// Connect authenticates to the endpoint's Keystone and returns the client of
// svc from the catalog. Gophercloud re-authenticates when the token expires.
func (c *Cloud) Connect(ctx context.Context, ep Endpoint, creds plugin.Credentials, svc Service) (*gophercloud.ServiceClient, error) {
	ck, err := c.check(ep)
	if err != nil {
		return nil, err
	}
	opts, err := authOptions(ck.authURL, creds)
	if err != nil {
		return nil, err
	}
	pc, err := openstack.NewClient(ck.authURL)
	if err != nil {
		return nil, plugin.Wrapf(plugin.Permanent, "auth_url: %v", err)
	}
	pc.HTTPClient = c.httpClient(ck.roots)
	if err := openstack.AuthenticateV3(ctx, pc, &opts, gophercloud.EndpointOpts{}); err != nil {
		return nil, Classify(err, "keystone")
	}
	catalog, err := catalogOf(pc)
	if err != nil {
		return nil, err
	}
	// The endpoint straight from the catalog, checked before any request:
	// gophercloud's version discovery would send the token to it first, with
	// no context. Tokens cross it as credentials cross Keystone.
	pc.EndpointLocator = func(eo gophercloud.EndpointOpts) (string, error) {
		u, ok := find(catalog, eo)
		if !ok {
			return "", plugin.Wrapf(plugin.Permanent, "%s: not in the catalog for region %q and interface %q", svc.name, ep.Region, ck.avail)
		}
		if p, err := url.Parse(u); err != nil || (p.Scheme != "https" && (p.Scheme != "http" || !c.plaintext)) {
			return "", plugin.Wrapf(plugin.Permanent, "%s: the catalog's endpoint is not https", svc.name)
		}
		return u, nil
	}
	sc, err := svc.client(pc, gophercloud.EndpointOpts{Region: ep.Region, Availability: ck.avail})
	if err != nil {
		return nil, err
	}
	return sc, nil
}

// catalogOf is the catalog of the token the provider holds.
func catalogOf(pc *gophercloud.ProviderClient) (*tokens.ServiceCatalog, error) {
	r, ok := pc.GetAuthResult().(tokens.CreateResult)
	if !ok {
		return nil, plugin.Wrapf(plugin.Permanent, "keystone: no catalog in the token")
	}
	cat, err := r.ExtractServiceCatalog()
	if err != nil {
		return nil, plugin.Wrapf(plugin.Permanent, "keystone: catalog: %v", err)
	}
	return cat, nil
}

// find is the first endpoint of the service, interface and region, as
// Keystone's own clients choose it.
func find(catalog *tokens.ServiceCatalog, eo gophercloud.EndpointOpts) (string, bool) {
	for _, e := range catalog.Entries {
		if e.Type != eo.Type {
			continue
		}
		for _, ep := range e.Endpoints {
			if ep.Interface == string(eo.Availability) && (eo.Region == "" || ep.Region == eo.Region || ep.RegionID == eo.Region) {
				return gophercloud.NormalizeURL(ep.URL), true
			}
		}
	}
	return "", false
}

// ResponseError is the service's answer in err, if any: gophercloud returns
// it by value, or by pointer after a re-authentication.
func ResponseError(err error) (gophercloud.ErrUnexpectedResponseCode, bool) {
	if e, ok := errors.AsType[gophercloud.ErrUnexpectedResponseCode](err); ok {
		return e, true
	}
	if e, ok := errors.AsType[*gophercloud.ErrUnexpectedResponseCode](err); ok && e != nil {
		return *e, true
	}
	return gophercloud.ErrUnexpectedResponseCode{}, false
}

// StatusOf is the HTTP status of a service's error, if it has one.
func StatusOf(err error) (int, bool) {
	e, ok := ResponseError(err)
	return e.Actual, ok
}

// Classify gives an error of gophercloud or of the transport its class
// (PLG-4), with a message that carries neither the response's body nor a
// refused address. what names the service.
func Classify(err error, what string) error {
	if err == nil {
		return nil
	}
	if plugin.ClassOf(err) != plugin.Unknown {
		return err
	}
	if errors.Is(err, netguard.ErrForbidden) {
		return plugin.Wrap(fmt.Errorf("%s: %w", what, netguard.ErrForbidden), plugin.Permanent)
	}
	if _, ok := errors.AsType[*gophercloud.ErrUnableToReauthenticate](err); ok {
		return plugin.Wrap(fmt.Errorf("%s: not authorised (re-authentication failed)", what), plugin.Permanent)
	}
	if e, ok := ResponseError(err); ok {
		msg := fmt.Errorf("%s: %s answered %d", what, e.Method, e.Actual)
		switch {
		case e.Actual == http.StatusNotFound:
			return fmt.Errorf("%s: %w", what, plugin.ErrNotFound)
		case e.Actual == http.StatusUnauthorized || e.Actual == http.StatusForbidden:
			return plugin.Wrap(fmt.Errorf("%s: not authorised (%d)", what, e.Actual), plugin.Permanent)
		case e.Actual == http.StatusRequestTimeout || e.Actual == http.StatusTooManyRequests || e.Actual >= 500:
			return plugin.Wrap(msg, plugin.Transient)
		default:
			return plugin.Wrap(msg, plugin.Permanent)
		}
	}
	var unknownCA x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	var verify *tls.CertificateVerificationError
	if errors.As(err, &unknownCA) || errors.As(err, &hostname) || errors.As(err, &invalid) || errors.As(err, &verify) {
		return plugin.Wrap(fmt.Errorf("%s: certificate: %w", what, err), plugin.Permanent)
	}
	return plugin.Wrap(fmt.Errorf("%s: %w", what, err), plugin.Transient)
}
