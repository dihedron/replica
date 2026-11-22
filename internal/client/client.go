// Package client calls veho's API over gRPC or REST (.specs/veho-cli.md §2):
// one generic Call per method, the REST request built from the method's
// google.api.http annotation, so that both transports agree on every field.
package client

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	_ "github.com/dihedron/veho/api/veho/v1" // registers the services' descriptors
	"google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

// Options locate the API and the caller's credential.
type Options struct {
	URL       string
	CAFile    string
	TokenFile string
	// Token, when TokenFile is empty (VEHO_TOKEN).
	Token    string
	CertFile string
	KeyFile  string
	// REST selects REST instead of gRPC.
	REST bool
}

// Client calls the API.
type Client struct {
	rest  bool
	base  *url.URL
	token string
	http  *http.Client
	conn  *grpc.ClientConn
}

// Error is an error of the API, from either transport.
type Error struct {
	Code      string
	Message   string
	RequestID string
	Retryable bool
}

func (e *Error) Error() string {
	if e.RequestID != "" {
		return fmt.Sprintf("%s: %s (request %s)", e.Code, e.Message, e.RequestID)
	}
	return e.Code + ": " + e.Message
}

// ErrNotModified is the end of a long poll without a change.
var ErrNotModified = errors.New("not modified")

// New connects to the API.
func New(o Options) (*Client, error) {
	u, err := url.Parse(o.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("client: the API's URL %q: want https://host:port", o.URL)
	}
	token := o.Token
	if o.TokenFile != "" {
		if token, err = readToken(o.TokenFile); err != nil {
			return nil, err
		}
	}
	tc := &tls.Config{MinVersion: tls.VersionTLS13, ServerName: u.Hostname()}
	if o.CAFile != "" {
		pem, err := os.ReadFile(o.CAFile)
		if err != nil {
			return nil, fmt.Errorf("client: %w", err)
		}
		tc.RootCAs = x509.NewCertPool()
		if !tc.RootCAs.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("client: no certificate in %s", o.CAFile)
		}
	}
	if o.CertFile != "" {
		cert, err := tls.LoadX509KeyPair(o.CertFile, o.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("client: %w", err)
		}
		tc.Certificates = []tls.Certificate{cert}
	}
	if token == "" && o.CertFile == "" {
		return nil, errors.New("client: no credential: set VEHO_TOKEN or --token-file, or give --cert-file and --key-file")
	}
	c := &Client{rest: o.REST, base: u, token: strings.TrimSpace(token)}
	if c.rest {
		c.http = &http.Client{Transport: &http.Transport{TLSClientConfig: tc, ForceAttemptHTTP2: true}}
		return c, nil
	}
	if c.conn, err = grpc.NewClient(u.Host, grpc.WithTransportCredentials(credentials.NewTLS(tc))); err != nil {
		return nil, fmt.Errorf("client: %w", err)
	}
	return c, nil
}

// readToken reads a token file, refusing one others can read (C8-3).
func readToken(path string) (string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("client: %w", err)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("client: %s must not be accessible by group or others (mode %v)", path, fi.Mode().Perm())
	}
	b, err := os.ReadFile(path) //nolint:gosec // the user's own file
	if err != nil {
		return "", fmt.Errorf("client: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}

// Close closes the connection.
func (c *Client) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

// Method finds a method by its full name, veho.v1.JobService.GetJob.
func Method(full string) (protoreflect.MethodDescriptor, error) {
	d, err := protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName(full))
	if err != nil {
		return nil, fmt.Errorf("client: %s: %w", full, err)
	}
	m, ok := d.(protoreflect.MethodDescriptor)
	if !ok {
		return nil, fmt.Errorf("client: %s is not a method", full)
	}
	return m, nil
}

// Call calls a unary method; headers are extra metadata (If-None-Match).
func (c *Client) Call(ctx context.Context, m protoreflect.MethodDescriptor, req, resp proto.Message, headers ...string) error {
	if c.rest {
		return c.restCall(ctx, m, req, func(body io.Reader) error { return decode(body, resp) }, headers...)
	}
	ctx = c.outgoing(ctx, headers...)
	return grpcError(c.conn.Invoke(ctx, "/"+string(m.Parent().FullName())+"/"+string(m.Name()), req, resp))
}

// Stream calls a server-streaming method and gives each message to fn,
// made by newResp, until the stream ends.
func (c *Client) Stream(ctx context.Context, m protoreflect.MethodDescriptor, req proto.Message, newResp func() proto.Message, fn func(proto.Message) error) error {
	if c.rest {
		return c.restCall(ctx, m, req, func(body io.Reader) error {
			sc := bufio.NewScanner(body)
			sc.Buffer(make([]byte, 64<<10), 16<<20)
			for sc.Scan() {
				var line struct {
					Result json.RawMessage `json:"result"`
					Error  *struct {
						Code    int    `json:"code"`
						Message string `json:"message"`
					} `json:"error"`
				}
				if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
					return fmt.Errorf("client: stream: %w", err)
				}
				if line.Error != nil {
					return &Error{Code: strings.ToLower(codes.Code(line.Error.Code).String()), Message: line.Error.Message} //nolint:gosec // a gRPC code
				}
				msg := newResp()
				if err := decode(bytes.NewReader(line.Result), msg); err != nil {
					return err
				}
				if err := fn(msg); err != nil {
					return err
				}
			}
			return sc.Err()
		})
	}
	ctx = c.outgoing(ctx)
	desc := &grpc.StreamDesc{StreamName: string(m.Name()), ServerStreams: true}
	s, err := c.conn.NewStream(ctx, desc, "/"+string(m.Parent().FullName())+"/"+string(m.Name()))
	if err != nil {
		return grpcError(err)
	}
	if err := s.SendMsg(req); err != nil {
		return grpcError(err)
	}
	if err := s.CloseSend(); err != nil {
		return grpcError(err)
	}
	for {
		msg := newResp()
		if err := s.RecvMsg(msg); errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return grpcError(err)
		}
		if err := fn(msg); err != nil {
			return err
		}
	}
}

func (c *Client) outgoing(ctx context.Context, headers ...string) context.Context {
	if c.token != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+c.token)
	}
	if len(headers) > 0 {
		ctx = metadata.AppendToOutgoingContext(ctx, headers...)
	}
	return ctx
}

func decode(r io.Reader, msg proto.Message) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("client: %w", err)
	}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(b, msg); err != nil {
		return fmt.Errorf("client: the answer: %w", err)
	}
	return nil
}

// grpcError turns a status into an Error, with its ErrorInfo's reason.
func grpcError(err error) error {
	if err == nil {
		return nil
	}
	st, ok := status.FromError(err)
	if !ok {
		return err
	}
	e := &Error{Code: strings.ToLower(st.Code().String()), Message: st.Message()}
	for _, d := range st.Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok {
			e.Code, e.Retryable = info.Reason, info.Metadata["retryable"] == "true"
		}
	}
	if e.Code == "not_modified" {
		return ErrNotModified
	}
	return e
}

// Request builds the REST request of a call from the method's
// google.api.http annotation (§2): path variables from the request, then
// the body ("*": the whole request) or the query.
func Request(m protoreflect.MethodDescriptor, req proto.Message) (method, path string, query url.Values, body []byte, err error) {
	rule, _ := proto.GetExtension(m.Options(), annotations.E_Http).(*annotations.HttpRule)
	var tmpl string
	switch p := rule.GetPattern().(type) {
	case *annotations.HttpRule_Get:
		method, tmpl = http.MethodGet, p.Get
	case *annotations.HttpRule_Post:
		method, tmpl = http.MethodPost, p.Post
	case *annotations.HttpRule_Put:
		method, tmpl = http.MethodPut, p.Put
	case *annotations.HttpRule_Delete:
		method, tmpl = http.MethodDelete, p.Delete
	case *annotations.HttpRule_Patch:
		method, tmpl = http.MethodPatch, p.Patch
	default:
		return "", "", nil, nil, fmt.Errorf("client: %s has no REST route", m.FullName())
	}
	raw, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(req)
	if err != nil {
		return "", "", nil, nil, fmt.Errorf("client: %w", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		return "", "", nil, nil, fmt.Errorf("client: %w", err)
	}
	for {
		i := strings.IndexByte(tmpl, '{')
		if i < 0 {
			break
		}
		j := strings.IndexByte(tmpl[i:], '}') + i
		name := tmpl[i+1 : j]
		v := fmt.Sprint(fields[name])
		if fields[name] == nil {
			v = ""
		}
		tmpl = tmpl[:i] + url.PathEscape(v) + tmpl[j+1:]
		delete(fields, name)
	}
	path = tmpl
	if rule.GetBody() == "*" {
		return method, path, nil, raw, nil
	}
	query = url.Values{}
	for k, v := range fields {
		switch x := v.(type) {
		case map[string]any:
			for mk, mv := range x {
				query.Set(k+"["+mk+"]", fmt.Sprint(mv))
			}
		case []any:
			for _, e := range x {
				query.Add(k, fmt.Sprint(e))
			}
		default:
			query.Set(k, fmt.Sprint(x))
		}
	}
	return method, path, query, nil, nil
}

func (c *Client) restCall(ctx context.Context, m protoreflect.MethodDescriptor, req proto.Message, read func(io.Reader) error, headers ...string) error {
	method, path, query, body, err := Request(m, req)
	if err != nil {
		return err
	}
	u := *c.base
	u.Path = strings.TrimSuffix(u.Path, "/") + path
	u.RawQuery = query.Encode()
	hr, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("client: %w", err)
	}
	if body != nil {
		hr.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		hr.Header.Set("Authorization", "Bearer "+c.token)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		hr.Header.Set(headers[i], headers[i+1])
	}
	resp, err := c.http.Do(hr)
	if err != nil {
		return fmt.Errorf("client: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusNotModified:
		return ErrNotModified
	case resp.StatusCode >= 400:
		var p struct {
			Detail    string `json:"detail"`
			Code      string `json:"code"`
			Retryable bool   `json:"retryable"`
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if json.Unmarshal(b, &p) != nil || p.Code == "" {
			return &Error{Code: strings.ToLower(strings.ReplaceAll(http.StatusText(resp.StatusCode), " ", "_")), Message: strings.TrimSpace(string(b)),
				RequestID: resp.Header.Get("X-Request-Id")}
		}
		return &Error{Code: p.Code, Message: p.Detail, Retryable: p.Retryable, RequestID: resp.Header.Get("X-Request-Id")}
	}
	return read(resp.Body)
}

// DefaultTimeout bounds a call when the context has no deadline.
const DefaultTimeout = 30 * time.Second
