package openstack

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gophercloud/gophercloud/v2"

	"github.com/dihedron/veho/internal/netguard"
	"github.com/dihedron/veho/pkg/plugin"
)

// fakeKeystone is a Keystone v3 that issues tokens and a catalog with one
// image service per region and interface; svc answers the service's requests.
type fakeKeystone struct {
	srv      *httptest.Server
	catalog  func(base string) string // the image endpoint's URL, given the server's
	issued   atomic.Int32
	lastAuth atomic.Value // the identity of the last authentication
	expire   atomic.Bool  // the next service request answers 401
}

func newFakeKeystone(t *testing.T) *fakeKeystone {
	t.Helper()
	k := &fakeKeystone{catalog: func(base string) string { return base + "/image" }}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v3/auth/tokens", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Auth struct {
				Identity map[string]json.RawMessage `json:"identity"`
			} `json:"auth"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		id := body.Auth.Identity
		var ok bool
		switch {
		case id["application_credential"] != nil:
			ok = strings.Contains(string(id["application_credential"]), `"secret":"s3cret"`)
			k.lastAuth.Store("application_credential")
		case id["password"] != nil:
			ok = strings.Contains(string(id["password"]), `"password":"pw"`) && strings.Contains(string(id["password"]), `"name":"Default"`)
			k.lastAuth.Store("password")
		}
		if !ok {
			http.Error(w, `{"error":{"code":401,"message":"The request you have made requires authentication."}}`, http.StatusUnauthorized)
			return
		}
		n := k.issued.Add(1)
		base := "https://" + r.Host
		ep := func(region, iface, url string) string {
			return fmt.Sprintf(`{"id":"%s-%s","interface":"%s","region":"%s","region_id":"%s","url":"%s"}`, region, iface, iface, region, region, url)
		}
		w.Header().Set("X-Subject-Token", fmt.Sprintf("token-%d", n))
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{"token":{"expires_at":"2099-01-01T00:00:00Z","catalog":[{"type":"image","name":"glance","endpoints":[%s,%s,%s]}]}}`,
			ep("RegionOne", "public", k.catalog(base)), ep("RegionOne", "internal", base+"/internal"), ep("RegionTwo", "public", base+"/two"))
	})
	for _, p := range []string{"/image/", "/internal/", "/two/"} {
		mux.HandleFunc(p, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == p { // the version document, unauthenticated, as Glance's
				w.WriteHeader(http.StatusMultipleChoices)
				_, _ = fmt.Fprintf(w, `{"versions":[{"id":"v2.6","status":"CURRENT","links":[{"rel":"self","href":"https://%s%sv2/"}]}]}`, r.Host, p)
				return
			}
			if k.expire.CompareAndSwap(true, false) || !strings.HasPrefix(r.Header.Get("X-Auth-Token"), "token-") {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = fmt.Fprintf(w, "%s %s", p, r.Header.Get("X-Auth-Token"))
		})
	}
	mux.HandleFunc("/redirect/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://example.com/", http.StatusFound)
	})
	k.srv = httptest.NewTLSServer(mux)
	t.Cleanup(k.srv.Close)
	return k
}

func (k *fakeKeystone) ca() string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: k.srv.Certificate().Raw}))
}

func (k *fakeKeystone) endpoint() Endpoint {
	return Endpoint{AuthURL: k.srv.URL + "/v3", Region: "RegionOne", CACert: k.ca()}
}

var appCred = plugin.Credentials{"application_credential_id": "id", "application_credential_secret": "s3cret"}

func local(t *testing.T) *Cloud {
	t.Helper()
	c, err := NewCloud(Settings{AllowCIDRs: []string{"127.0.0.1/32"}})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func get(t *testing.T, sc *gophercloud.ServiceClient, path string) (string, error) {
	t.Helper()
	resp, err := sc.Request(context.Background(), "GET", sc.ServiceURL(path), &gophercloud.RequestOpts{KeepResponseBody: true})
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return string(b), nil
}

// OS-T1: application credentials, username and password, a bad secret, a
// token expired during a transfer.
func TestAuthenticate(t *testing.T) {
	k := newFakeKeystone(t)
	c := local(t)
	ctx := context.Background()

	for name, creds := range map[string]plugin.Credentials{
		"application_credential": appCred,
		"password":               {"username": "u", "password": "pw", "project_name": "p"},
	} {
		t.Run(name, func(t *testing.T) {
			sc, err := c.Connect(ctx, k.endpoint(), creds, Image)
			if err != nil {
				t.Fatal(err)
			}
			if got := k.lastAuth.Load(); got != name {
				t.Errorf("authenticated with %v", got)
			}
			if body, err := get(t, sc, "images"); err != nil || !strings.HasPrefix(body, "/image/ token-") {
				t.Errorf("service: %q, %v", body, err)
			}
		})
	}

	t.Run("bad secret", func(t *testing.T) {
		_, err := c.Connect(ctx, k.endpoint(), plugin.Credentials{"application_credential_id": "id", "application_credential_secret": "wrong"}, Image)
		if plugin.ClassOf(err) != plugin.Permanent || strings.Contains(err.Error(), "wrong") {
			t.Fatalf("got %v (%v)", err, plugin.ClassOf(err))
		}
	})

	t.Run("expired token", func(t *testing.T) {
		sc, err := c.Connect(ctx, k.endpoint(), appCred, Image)
		if err != nil {
			t.Fatal(err)
		}
		before := k.issued.Load()
		k.expire.Store(true)
		body, err := get(t, sc, "images")
		if err != nil || k.issued.Load() != before+1 || !strings.HasSuffix(body, fmt.Sprintf("token-%d", before+1)) {
			t.Fatalf("re-authentication: %q, %v, %d tokens", body, err, k.issued.Load()-before)
		}
	})

	for name, creds := range map[string]plugin.Credentials{
		"none":    {},
		"both":    {"application_credential_id": "id", "application_credential_secret": "s", "username": "u", "password": "p", "project_name": "p"},
		"unknown": {"application_credential_id": "id", "application_credential_secret": "s", "token": "t"},
		"half":    {"application_credential_id": "id"},
		"project": {"username": "u", "password": "pw"},
	} {
		t.Run("refused "+name, func(t *testing.T) {
			if _, err := c.Connect(ctx, k.endpoint(), creds, Image); plugin.ClassOf(err) != plugin.Permanent {
				t.Fatalf("got %v", err)
			}
		})
	}
}

// OS-T2: region and interface; a refused address; ca_cert; TLS and
// redirects.
func TestCatalog(t *testing.T) {
	k := newFakeKeystone(t)
	c := local(t)
	ctx := context.Background()

	for _, tc := range []struct {
		region, iface, want string
	}{
		{"RegionOne", "", "/image/"}, {"RegionOne", "internal", "/internal/"}, {"RegionTwo", "public", "/two/"},
	} {
		ep := k.endpoint()
		ep.Region, ep.Interface = tc.region, tc.iface
		sc, err := c.Connect(ctx, ep, appCred, Image)
		if err != nil {
			t.Fatal(err)
		}
		if body, err := get(t, sc, "images"); err != nil || !strings.HasPrefix(body, tc.want) {
			t.Errorf("%s/%s: %q, %v", tc.region, tc.iface, body, err)
		}
	}

	t.Run("no such region", func(t *testing.T) {
		ep := k.endpoint()
		ep.Region = "Nowhere"
		if _, err := c.Connect(ctx, ep, appCred, Image); plugin.ClassOf(err) != plugin.Permanent {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("catalog endpoint refused by the guard", func(t *testing.T) {
		k.catalog = func(string) string { return "https://127.0.0.2:1/image" }
		defer func() { k.catalog = func(base string) string { return base + "/image" } }()
		sc, err := c.Connect(ctx, k.endpoint(), appCred, Image)
		if err != nil {
			t.Fatal(err)
		}
		_, err = get(t, sc, "images")
		err = Classify(err, "image")
		if !errors.Is(err, netguard.ErrForbidden) || plugin.ClassOf(err) != plugin.Permanent {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("catalog endpoint in plain http", func(t *testing.T) {
		k.catalog = func(base string) string { return strings.Replace(base, "https", "http", 1) + "/image" }
		defer func() { k.catalog = func(base string) string { return base + "/image" } }()
		if _, err := c.Connect(ctx, k.endpoint(), appCred, Image); plugin.ClassOf(err) != plugin.Permanent {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("keystone refused by the guard", func(t *testing.T) {
		strict, _ := NewCloud(Settings{})
		_, err := strict.Connect(ctx, k.endpoint(), appCred, Image)
		if !errors.Is(err, netguard.ErrForbidden) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("unknown CA", func(t *testing.T) {
		ep := k.endpoint()
		ep.CACert = ""
		if _, err := c.Connect(ctx, ep, appCred, Image); plugin.ClassOf(err) != plugin.Permanent {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("redirect not followed", func(t *testing.T) {
		sc, err := c.Connect(ctx, k.endpoint(), appCred, Image)
		if err != nil {
			t.Fatal(err)
		}
		sc.Endpoint, sc.ResourceBase = k.srv.URL+"/redirect/", k.srv.URL+"/redirect/"
		if _, err := get(t, sc, "images"); err == nil {
			t.Fatal("redirect followed")
		}
	})
}

func TestCheckEndpoint(t *testing.T) {
	plain, _ := NewCloud(Settings{Plaintext: true})
	strict, _ := NewCloud(Settings{})
	for _, tc := range []struct {
		c   *Cloud
		ep  Endpoint
		ok  bool
		why string
	}{
		{strict, Endpoint{AuthURL: "https://k:5000/v3"}, true, "https"},
		{strict, Endpoint{AuthURL: "https://k:5000"}, true, "the /v3 is added"},
		{strict, Endpoint{AuthURL: "http://k:5000/v3"}, false, "http without plaintext"},
		{plain, Endpoint{AuthURL: "http://k:5000/v3"}, true, "http with plaintext"},
		{strict, Endpoint{AuthURL: "https://u:p@k/v3"}, false, "user information"},
		{strict, Endpoint{AuthURL: "https://k/v3?x=1"}, false, "query"},
		{strict, Endpoint{AuthURL: "https://k/v2.0"}, false, "not v3"},
		{strict, Endpoint{AuthURL: ""}, false, "empty"},
		{strict, Endpoint{AuthURL: "https://k/v3", Interface: "private"}, false, "interface"},
		{strict, Endpoint{AuthURL: "https://k/v3", CACert: "not a PEM"}, false, "ca_cert"},
	} {
		_, err := tc.c.check(tc.ep)
		if (err == nil) != tc.ok {
			t.Errorf("%s: %v", tc.why, err)
		}
	}
}

func TestClassify(t *testing.T) {
	code := func(n int) error {
		return gophercloud.ErrUnexpectedResponseCode{Actual: n, Method: "GET", URL: "https://h/secret-path", Body: []byte("token=abc")}
	}
	for _, tc := range []struct {
		err  error
		want plugin.Class
	}{
		{code(401), plugin.Permanent}, {code(403), plugin.Permanent}, {code(400), plugin.Permanent},
		{code(404), plugin.Permanent}, {code(409), plugin.Permanent},
		{code(408), plugin.Transient}, {code(429), plugin.Transient}, {code(500), plugin.Transient}, {code(503), plugin.Transient},
		{context.DeadlineExceeded, plugin.Transient},
		{fmt.Errorf("dial: %w", netguard.ErrForbidden), plugin.Permanent},
		{errors.New("connection reset"), plugin.Transient},
	} {
		got := Classify(tc.err, "image")
		if plugin.ClassOf(got) != tc.want {
			t.Errorf("%v: %v, want %v", tc.err, plugin.ClassOf(got), tc.want)
		}
		if strings.Contains(got.Error(), "token=abc") {
			t.Errorf("the body leaked: %v", got)
		}
	}
	if !errors.Is(Classify(code(404), "image"), plugin.ErrNotFound) {
		t.Error("404 is not ErrNotFound")
	}
	if Classify(nil, "x") != nil {
		t.Error("nil is not nil")
	}
	if c, _ := StatusOf(code(409)); c != 409 {
		t.Error("StatusOf")
	}
}
