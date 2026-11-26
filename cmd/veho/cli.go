package main

import (
	"context"
	"errors"
	"io"
	"os"
	"time"

	"github.com/dihedron/veho/internal/client"
	"google.golang.org/protobuf/proto"
)

// apiOptions locate the API and the caller's credential (.specs/veho-cli.md
// §2); the token comes from VEHO_TOKEN or a file, never from an argument.
type apiOptions struct {
	URL       string        `long:"api-url" env:"VEHO_API_URL" description:"The API: https://host:port"`
	CAFile    string        `long:"ca-file" env:"VEHO_CA_FILE" description:"The CA of the API's certificate (default: the system's roots)"`
	TokenFile string        `long:"token-file" env:"VEHO_TOKEN_FILE" description:"A file holding the credential (else VEHO_TOKEN)"`
	CertFile  string        `long:"cert-file" env:"VEHO_CERT_FILE" description:"A client certificate (an X.509-SVID)"`
	KeyFile   string        `long:"key-file" env:"VEHO_KEY_FILE" description:"The client certificate's key"`
	Tenant    string        `long:"tenant" env:"VEHO_TENANT" description:"The tenant to act in, or platform (required without an API key)"`
	API       string        `long:"api" env:"VEHO_API" default:"grpc" choice:"grpc" choice:"rest" description:"The transport"`
	Format    string        `short:"F" long:"format" default:"table" choice:"table" choice:"json" choice:"yaml" description:"The output's format"`
	Wide      bool          `short:"w" long:"wide" description:"Lists as tables of all their fields, not only the main columns"`
	Timeout   time.Duration `long:"timeout" default:"30s" description:"The timeout of each call"`

	out io.Writer // tests only; default os.Stdout
	// cached is the login whose token the last client used, if any
	cached *login
}

func (o *apiOptions) stdout() io.Writer {
	if o.out != nil {
		return o.out
	}
	return os.Stdout
}

// client connects with the caller's credential: VEHO_TOKEN, --token-file
// or a certificate first, else the login cached for this API URL
// (veho-authentication.md §7), whose tenant is the default.
func (o *apiOptions) client() (*client.Client, error) {
	opts := client.Options{URL: o.URL, CAFile: o.CAFile, TokenFile: o.TokenFile, Token: os.Getenv("VEHO_TOKEN"),
		CertFile: o.CertFile, KeyFile: o.KeyFile, Tenant: o.Tenant, REST: o.API == "rest"}
	o.cached = nil
	if opts.Token == "" && opts.TokenFile == "" && opts.CertFile == "" {
		ctx, cancel := context.WithTimeout(context.Background(), o.Timeout)
		defer cancel()
		token, l, err := cachedToken(ctx, o.URL)
		if err != nil {
			return nil, err
		}
		if l != nil {
			opts.Token, o.cached = token, l
			if opts.Tenant == "" {
				opts.Tenant = l.Tenant
			}
		}
	}
	return client.New(opts)
}

// interactive says whether the CLI may sign the person in again; a
// variable for the tests.
var interactive = func() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// call calls method (veho.v1.JobService.GetJob) once; a cached login that
// the method finds too weak or too old is signed in again, in a terminal,
// and the call repeated once (step-up, RFC 9470).
func (o *apiOptions) call(method string, req, resp proto.Message, headers ...string) error {
	err := o.callOnce(method, req, resp, headers...)
	e, ok := errors.AsType[*client.Error](err)
	if !ok || o.cached == nil || !interactive() {
		return err
	}
	acr, maxAge, stepUp := e.StepUp()
	if !stepUp {
		return err
	}
	l := o.cached
	ctx, cancel := context.WithTimeout(context.Background(), loginTimeout+time.Minute)
	defer cancel()
	if err := signIn(ctx, l, o.stderr(), acr, maxAge); err != nil {
		return err
	}
	if err := storeLogin(o.URL, l); err != nil {
		return err
	}
	return o.callOnce(method, req, resp, headers...)
}

func (o *apiOptions) callOnce(method string, req, resp proto.Message, headers ...string) error {
	c, err := o.client()
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	m, err := client.Method(method)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), o.Timeout)
	defer cancel()
	return c.Call(ctx, m, req, resp, headers...)
}

// show calls method and prints its answer.
func (o *apiOptions) show(method string, req, resp proto.Message, columns ...string) error {
	if err := o.call(method, req, resp); err != nil {
		return err
	}
	return o.print(resp, columns...)
}

// exitError ends the process with its code (.specs/veho-cli.md §3).
type exitError struct {
	code int
	msg  string
}

func (e *exitError) Error() string { return e.msg }

// exitCode is the process's exit code for err.
func exitCode(err error) int {
	if e, ok := errors.AsType[*exitError](err); ok {
		return e.code
	}
	return 1
}

// readInput reads a file, or standard input for "-".
func readInput(path string) ([]byte, error) {
	if path == "-" {
		return io.ReadAll(os.Stdin)
	}
	return os.ReadFile(path) //nolint:gosec // the user's own file
}
