package glance

import (
	"bytes"
	"crypto/md5" //nolint:gosec // Glance's checksum
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeGlance is a Keystone and a Glance in one server. minor is the image
// API's v2 minor version: 6 is Queens (no os_hidden, no multihash), 7 and
// later Rocky.
type fakeGlance struct {
	srv *httptest.Server

	mu      sync.Mutex
	minor   int
	ranges  bool // the download honours Range
	methods []string
	stores  []string // nil: no multi-store
	images  map[string]map[string]any
	data    map[string][]byte
	staged  map[string][]byte
	corrupt bool // stores other bytes than it got
	puts    int
}

func newFakeGlance(t *testing.T, minor int) *fakeGlance {
	t.Helper()
	f := &fakeGlance{minor: minor, ranges: true, methods: []string{"glance-direct", "web-download"},
		images: map[string]map[string]any{}, data: map[string][]byte{}, staged: map[string][]byte{}}
	if minor >= 7 {
		f.stores = []string{"fast", "cheap"}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v3/auth/tokens", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if !bytes.Contains(b, []byte(`"secret":"good"`)) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("X-Subject-Token", "tok")
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{"token":{"project":{"id":"p1","name":"p"},"catalog":[{"type":"image","endpoints":[{"interface":"public","region":"R","url":"https://%s/image"}]}]}}`, r.Host)
	})
	mux.HandleFunc("/image/", f.serve)
	f.srv = httptest.NewTLSServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGlance) ca() string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.srv.Certificate().Raw}))
}

// add puts an active image there, as another client would.
func (f *fakeGlance) add(id, name string, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.images[id] = map[string]any{"id": id, "name": name, "status": "queued", "owner": "p1", "os_hidden": false}
	f.activate(id, data)
}

func (f *fakeGlance) activate(id string, data []byte) {
	if f.corrupt {
		data = append(append([]byte{}, data...), 'x')
	}
	im := f.images[id]
	m := md5.Sum(data) //nolint:gosec // Glance's checksum
	im["checksum"], im["size"], im["status"] = hex.EncodeToString(m[:]), len(data), "active"
	if f.minor >= 7 {
		s := sha512.Sum512(data)
		im["os_hash_algo"], im["os_hash_value"] = "sha512", hex.EncodeToString(s[:])
	}
	f.data[id] = data
}

func (f *fakeGlance) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/image/" || r.URL.Path == "/image" {
		w.WriteHeader(http.StatusMultipleChoices)
		_, _ = fmt.Fprintf(w, `{"versions":[{"id":"v2.%d","status":"CURRENT"},{"id":"v2.1","status":"SUPPORTED"}]}`, f.minor)
		return
	}
	if r.Header.Get("X-Auth-Token") != "tok" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/image/v2/")
	f.mu.Lock()
	defer f.mu.Unlock()
	parts := strings.Split(path, "/")
	switch {
	case path == "info/import" && f.minor >= 6:
		_ = json.NewEncoder(w).Encode(map[string]any{"import-methods": map[string]any{"value": f.methods}})
	case path == "info/stores" && f.stores != nil:
		var s []map[string]string
		for _, id := range f.stores {
			s = append(s, map[string]string{"id": id})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"stores": s})
	case path == "images" && r.Method == http.MethodPost:
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		id, _ := body["id"].(string)
		if _, ok := body["os_hidden"]; ok && f.minor < 7 {
			http.Error(w, "Additional properties are not allowed ('os_hidden' was unexpected)", http.StatusBadRequest)
			return
		}
		if _, ok := f.images[id]; ok || id == "" {
			w.WriteHeader(http.StatusConflict)
			return
		}
		body["status"], body["owner"] = "queued", "p1"
		if body["os_hidden"] == nil {
			body["os_hidden"] = false
		}
		f.images[id] = body
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(body)
	case path == "images" && r.Method == http.MethodGet:
		q := r.URL.Query()
		var out []map[string]any
		for _, im := range f.images {
			if im["name"] == q.Get("name") && im["owner"] == q.Get("owner") && im["status"] == q.Get("status") && im["os_hidden"] != true {
				out = append(out, im)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"images": out})
	case len(parts) >= 2 && parts[0] == "images":
		im := f.images[parts[1]]
		if im == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		f.image(w, r, parts[1], im, strings.Join(parts[2:], "/"))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeGlance) image(w http.ResponseWriter, r *http.Request, id string, im map[string]any, sub string) {
	switch {
	case sub == "" && r.Method == http.MethodGet:
		_ = json.NewEncoder(w).Encode(im)
	case sub == "" && r.Method == http.MethodDelete:
		delete(f.images, id)
		delete(f.data, id)
		w.WriteHeader(http.StatusNoContent)
	case sub == "" && r.Method == http.MethodPatch:
		if r.Header.Get("Content-Type") != "application/openstack-images-v2.1-json-patch" {
			w.WriteHeader(http.StatusUnsupportedMediaType)
			return
		}
		var ops []struct {
			Op, Path string
			Value    any
		}
		_ = json.NewDecoder(r.Body).Decode(&ops)
		for _, o := range ops {
			im[strings.TrimPrefix(o.Path, "/")] = o.Value
		}
		_ = json.NewEncoder(w).Encode(im)
	case (sub == "file" || sub == "stage") && r.Method == http.MethodPut:
		if im["status"] != "queued" {
			w.WriteHeader(http.StatusConflict)
			return
		}
		f.puts++
		f.mu.Unlock()
		b, err := io.ReadAll(r.Body)
		f.mu.Lock()
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if f.images[id] == nil { // deleted during the upload
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if sub == "stage" {
			f.staged[id] = b
			im["status"] = "uploading"
		} else {
			f.activate(id, b)
		}
		w.WriteHeader(http.StatusNoContent)
	case sub == "import" && r.Method == http.MethodPost:
		var body struct {
			Method struct {
				Name string `json:"name"`
			} `json:"method"`
			Stores []string `json:"stores"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		for _, s := range body.Stores {
			if !strings.Contains(strings.Join(f.stores, ","), s) {
				w.WriteHeader(http.StatusConflict)
				return
			}
		}
		im["veho_stores"] = strings.Join(body.Stores, ",")
		f.activate(id, f.staged[id])
		w.WriteHeader(http.StatusAccepted)
	case sub == "file" && r.Method == http.MethodGet:
		data, ok := f.data[id]
		if !ok {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var a, b int64
		if rng := r.Header.Get("Range"); rng != "" && f.ranges {
			if _, err := fmt.Sscanf(rng, "bytes=%d-%d", &a, &b); err != nil || a >= int64(len(data)) {
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			b = min(b, int64(len(data))-1)
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", a, b, len(data)))
			w.Header().Set("Content-Length", strconv.FormatInt(b-a+1, 10))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(data[a : b+1])
			return
		}
		_, _ = w.Write(data)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}
