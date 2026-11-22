// Command faultproxy sits in front of a region's Garage in the lab and
// injects the faults of the acceptance scenarios: S3 errors, corrupted part
// uploads and delays (.specs/veho-lab-l3.md §2.2). It forwards the client's
// Host header unchanged, so SigV4 signatures still verify.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

// duration is a time.Duration that JSON writes as "5s".
type duration time.Duration

func (d duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

func (d *duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	*d = duration(v)
	return err
}

// fault is the proxy's one active fault, as the control API sets it.
type fault struct {
	Mode   string   `json:"mode"`             // status, corrupt or delay
	Code   int      `json:"code,omitempty"`   // status: the HTTP status
	For    duration `json:"for,omitempty"`    // status: how long; 0 until cleared
	Count  int      `json:"count,omitempty"`  // corrupt: how many uploads
	Delay  duration `json:"delay,omitempty"`  // delay: added before each response
	Bucket string   `json:"bucket,omitempty"` // only requests to this bucket
	until  time.Time
}

func (f *fault) validate() error {
	switch f.Mode {
	case "status":
		if f.Code < 400 || f.Code > 599 {
			return fmt.Errorf("status: code %d is not an error", f.Code)
		}
	case "corrupt":
		if f.Count < 1 {
			return errors.New("corrupt: count must be at least 1")
		}
	case "delay":
		if f.Delay <= 0 {
			return errors.New("delay: delay must be positive")
		}
	default:
		return fmt.Errorf("unknown mode %q", f.Mode)
	}
	return nil
}

// s3Codes are the S3 error codes the status fault answers with.
var s3Codes = map[int]string{403: "AccessDenied", 404: "NoSuchKey", 500: "InternalError", 503: "SlowDown"}

type proxy struct {
	mu    sync.Mutex
	fault *fault
	next  *httputil.ReverseProxy
}

func newProxy(upstream string) (*proxy, error) {
	u, err := url.Parse(upstream)
	if err != nil {
		return nil, fmt.Errorf("upstream %q: %w", upstream, err)
	}
	return &proxy{next: &httputil.ReverseProxy{Rewrite: func(pr *httputil.ProxyRequest) {
		pr.SetURL(u)
		pr.Out.Host = pr.In.Host // signed by the client
	}}}, nil
}

// active returns the fault that applies to r, consuming a corrupt count.
func (p *proxy) active(r *http.Request) *fault {
	p.mu.Lock()
	defer p.mu.Unlock()
	f := p.fault
	if f == nil {
		return nil
	}
	if !f.until.IsZero() && time.Now().After(f.until) {
		p.fault = nil
		return nil
	}
	if f.Bucket != "" && strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 2)[0] != f.Bucket {
		return nil
	}
	if f.Mode == "corrupt" {
		if r.Method != http.MethodPut || r.ContentLength <= 0 {
			return nil
		}
		if f.Count--; f.Count == 0 {
			p.fault = nil
		}
	}
	c := *f
	return &c
}

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f := p.active(r)
	if f == nil {
		p.next.ServeHTTP(w, r)
		return
	}
	slog.Info("fault", "mode", f.Mode, "method", r.Method, "path", r.URL.Path, "query", r.URL.RawQuery)
	switch f.Mode {
	case "status":
		code := s3Codes[f.Code]
		if code == "" {
			code = "InternalError"
		}
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(f.Code)
		_, _ = fmt.Fprintf(w, "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<Error><Code>%s</Code><Message>injected by the lab</Message><Resource>%s</Resource></Error>", code, r.URL.Path)
	case "corrupt":
		r.Body = &flipper{r: r.Body, at: r.ContentLength / 2}
		p.next.ServeHTTP(w, r)
	case "delay":
		select {
		case <-time.After(time.Duration(f.Delay)):
		case <-r.Context().Done():
			return
		}
		p.next.ServeHTTP(w, r)
	}
}

// flipper inverts the byte at offset at of the body it reads.
type flipper struct {
	r   io.ReadCloser
	at  int64
	off int64
}

func (f *flipper) Read(b []byte) (int, error) {
	n, err := f.r.Read(b)
	if i := f.at - f.off; i >= 0 && i < int64(n) {
		b[i] ^= 0xff
	}
	f.off += int64(n)
	return n, err
}

func (f *flipper) Close() error { return f.r.Close() }

// control is the API that sets, shows and clears the fault.
func (p *proxy) control() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /fault", func(w http.ResponseWriter, r *http.Request) {
		var f fault
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&f); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := f.validate(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if f.Mode == "status" && f.For > 0 {
			f.until = time.Now().Add(time.Duration(f.For))
		}
		p.mu.Lock()
		p.fault = &f
		p.mu.Unlock()
		slog.Info("fault set", "mode", f.Mode, "code", f.Code, "for", time.Duration(f.For), "count", f.Count, "delay", time.Duration(f.Delay), "bucket", f.Bucket)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("DELETE /fault", func(w http.ResponseWriter, _ *http.Request) {
		p.mu.Lock()
		p.fault = nil
		p.mu.Unlock()
		slog.Info("fault cleared")
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /fault", func(w http.ResponseWriter, _ *http.Request) {
		p.mu.Lock()
		f := p.fault
		p.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(f)
	})
	return mux
}

func main() {
	listen := flag.String("listen", ":3910", "the S3 address the clients use")
	upstream := flag.String("upstream", "http://127.0.0.1:3900", "Garage's S3 API")
	control := flag.String("control", "127.0.0.1:3911", "the control API (keep it local)")
	flag.Parse()
	p, err := newProxy(*upstream)
	if err != nil {
		slog.Error("faultproxy", "error", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	servers := []*http.Server{
		{Addr: *listen, Handler: p, ReadHeaderTimeout: 10 * time.Second},
		{Addr: *control, Handler: p.control(), ReadHeaderTimeout: 10 * time.Second},
	}
	errs := make(chan error, len(servers))
	for _, s := range servers {
		go func() { errs <- s.ListenAndServe() }()
	}
	select {
	case err = <-errs:
		slog.Error("faultproxy", "error", err)
	case <-ctx.Done():
	}
	for _, s := range servers {
		_ = s.Shutdown(context.Background())
	}
	if err != nil {
		os.Exit(1)
	}
}
