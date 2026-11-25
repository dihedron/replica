package swift

import (
	"bytes"
	"crypto/md5" //nolint:gosec // Swift's ETags
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSwift is a Keystone and a Swift proxy in one server: containers with a
// storage policy, objects, static large objects, ranges and If-Match.
type fakeSwift struct {
	srv *httptest.Server

	mu         sync.Mutex
	containers map[string]string  // name to storage policy
	objects    map[string]*object // container/name
	clock      int64
	putBody    func(name string, b []byte) []byte // what the server stores, given what it got
}

type object struct {
	data     []byte
	etag     string // quoted
	ts       string
	meta     http.Header
	segments []string // an SLO's segments, container/name
}

func newFakeSwift(t *testing.T) *fakeSwift {
	t.Helper()
	f := &fakeSwift{containers: map[string]string{}, objects: map[string]*object{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v3/auth/tokens", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if !bytes.Contains(b, []byte(`"secret":"good"`)) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("X-Subject-Token", "tok")
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{"token":{"catalog":[{"type":"object-store","endpoints":[{"interface":"public","region":"R","url":"https://%s/v1/AUTH_p"}]}]}}`, r.Host)
	})
	mux.HandleFunc("GET /info", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"swift":{},"slo":{"max_manifest_segments":1000}}`)
	})
	mux.HandleFunc("/v1/AUTH_p/", f.serve)
	f.srv = httptest.NewTLSServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSwift) ca() string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.srv.Certificate().Raw}))
}

func etagOf(b []byte) string {
	s := md5.Sum(b) //nolint:gosec // Swift's ETag
	return hex.EncodeToString(s[:])
}

func (f *fakeSwift) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Auth-Token") != "tok" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	path, _ := url.PathUnescape(strings.TrimPrefix(r.URL.EscapedPath(), "/v1/AUTH_p/"))
	container, name, _ := strings.Cut(path, "/")
	f.mu.Lock()
	defer f.mu.Unlock()
	if name == "" {
		f.container(w, r, container)
		return
	}
	if _, ok := f.containers[container]; !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	key := container + "/" + name
	switch r.Method {
	case http.MethodPut:
		b, _ := io.ReadAll(r.Body)
		f.clock++
		o := &object{ts: fmt.Sprintf("%d.00000", 1700000000+f.clock), meta: http.Header{}}
		for k, v := range r.Header {
			if strings.HasPrefix(k, "X-Object-Meta-") || k == "Content-Type" {
				o.meta[k] = v
			}
		}
		if r.URL.Query().Get("multipart-manifest") == "put" {
			var segs []struct {
				Path string `json:"path"`
				Etag string `json:"etag"`
				Size int64  `json:"size_bytes"`
			}
			if err := json.Unmarshal(b, &segs); err != nil || len(segs) == 0 {
				http.Error(w, "Manifest must be valid JSON", http.StatusBadRequest)
				return
			}
			h := md5.New() //nolint:gosec // Swift's ETag
			for _, s := range segs {
				seg := f.objects[strings.TrimPrefix(s.Path, "/")]
				switch {
				case seg == nil:
					http.Error(w, "Errors:\n"+s.Path+", 404 Not Found", http.StatusBadRequest)
					return
				case `"`+s.Etag+`"` != seg.etag:
					http.Error(w, "Errors:\n"+s.Path+", Etag Mismatch", http.StatusBadRequest)
					return
				case s.Size != int64(len(seg.data)):
					http.Error(w, "Errors:\n"+s.Path+", Size Mismatch", http.StatusBadRequest)
					return
				}
				o.data = append(o.data, seg.data...)
				o.segments = append(o.segments, strings.TrimPrefix(s.Path, "/"))
				h.Write([]byte(s.Etag))
			}
			o.etag = `"` + hex.EncodeToString(h.Sum(nil)) + `"`
		} else {
			if f.putBody != nil {
				b = f.putBody(name, b)
			}
			o.data, o.etag = b, `"`+etagOf(b)+`"`
			if e := r.Header.Get("Etag"); e != "" && `"`+e+`"` != o.etag {
				w.WriteHeader(http.StatusUnprocessableEntity)
				return
			}
		}
		f.objects[key] = o
		w.Header().Set("Etag", strings.Trim(o.etag, `"`))
		w.WriteHeader(http.StatusCreated)
	case http.MethodHead, http.MethodGet:
		o := f.objects[key]
		if o == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if m := r.Header.Get("If-Match"); m != "" && m != o.etag && `"`+m+`"` != o.etag {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		for k, v := range o.meta {
			w.Header()[k] = v
		}
		w.Header().Set("Etag", o.etag)
		w.Header().Set("X-Timestamp", o.ts)
		w.Header().Set("Last-Modified", time.Unix(1700000000+f.clock, 0).UTC().Format(http.TimeFormat))
		if o.segments != nil {
			w.Header().Set("X-Static-Large-Object", "True")
		}
		if r.URL.Query().Get("multipart-manifest") == "get" && o.segments != nil {
			var m []map[string]any
			for _, s := range o.segments {
				seg := f.objects[s]
				m = append(m, map[string]any{"name": "/" + s, "bytes": len(seg.data), "hash": strings.Trim(seg.etag, `"`)})
			}
			_ = json.NewEncoder(w).Encode(m)
			return
		}
		data := o.data
		if rng := r.Header.Get("Range"); rng != "" {
			var a, b int64
			if _, err := fmt.Sscanf(rng, "bytes=%d-%d", &a, &b); err != nil || a >= int64(len(data)) {
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			b = min(b, int64(len(data))-1)
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", a, b, len(data)))
			w.Header().Set("Content-Length", strconv.FormatInt(b-a+1, 10))
			w.WriteHeader(http.StatusPartialContent)
			if r.Method == http.MethodGet {
				_, _ = w.Write(data[a : b+1])
			}
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		if r.Method == http.MethodGet {
			_, _ = w.Write(data)
		}
	case http.MethodDelete:
		o := f.objects[key]
		if o == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.URL.Query().Get("multipart-manifest") == "delete" {
			for _, s := range o.segments {
				delete(f.objects, s)
			}
		}
		delete(f.objects, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (f *fakeSwift) container(w http.ResponseWriter, r *http.Request, name string) {
	switch r.Method {
	case http.MethodPut:
		if _, ok := f.containers[name]; ok {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		f.containers[name] = r.Header.Get("X-Storage-Policy")
		if f.containers[name] == "" {
			f.containers[name] = "Policy-0"
		}
		w.WriteHeader(http.StatusCreated)
	case http.MethodHead:
		p, ok := f.containers[name]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("X-Storage-Policy", p)
		w.WriteHeader(http.StatusNoContent)
	case http.MethodGet:
		if _, ok := f.containers[name]; !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		q := r.URL.Query()
		limit, _ := strconv.Atoi(q.Get("limit"))
		if limit <= 0 {
			limit = 10000
		}
		type entry struct {
			Name  string `json:"name"`
			Bytes int    `json:"bytes"`
			Hash  string `json:"hash"`
		}
		var out []entry
		var names []string
		for k := range f.objects {
			if c, n, _ := strings.Cut(k, "/"); c == name && strings.HasPrefix(n, q.Get("prefix")) && n > q.Get("marker") {
				names = append(names, n)
			}
		}
		sort.Strings(names)
		for _, n := range names {
			if len(out) == limit {
				break
			}
			o := f.objects[name+"/"+n]
			out = append(out, entry{n, len(o.data), strings.Trim(o.etag, `"`)})
		}
		if len(out) == 0 {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_ = json.NewEncoder(w).Encode(out)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}
