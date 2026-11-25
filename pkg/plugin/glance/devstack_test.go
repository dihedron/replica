//go:build devstack

package glance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/dihedron/veho/pkg/plugin"
	"github.com/dihedron/veho/pkg/plugin/conformance"
	"github.com/dihedron/veho/pkg/plugin/openstack"
)

// OS-T3 and OS-T4 on DevStack: the conformance suite with each upload
// method (glance-direct to a chosen store).
func TestDevStackConformance(t *testing.T) {
	s, ep, creds := openstack.DevStack(t)
	settings, _ := json.Marshal(s)
	d, err := Factory(settings)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []config{
		{Upload: "put"},
		{Upload: "glance-direct", Stores: []string{"cheap"}},
	} {
		t.Run(m.Upload, func(t *testing.T) {
			m.Endpoint, m.DiskFormat, m.ContainerFormat = ep, "raw", "bare"
			cfg, _ := json.Marshal(m)
			conformance.Run(t, conformance.Target{
				Driver: d, Config: cfg, Credentials: creds["application_credential"],
				Prefix: fmt.Sprintf("veho-conformance-%s-%d-", m.Upload, time.Now().UnixNano()), PartSize: 1 << 20,
				BadCredentials: plugin.Credentials{"application_credential_id": "nobody", "application_credential_secret": "wrong"},
			})
		})
	}
}

// OS-T5 on DevStack: Glance fetches the image from an HTTP source on the
// lab host's address (web-download), it is read back while hidden, then
// published.
func TestDevStackWebDownload(t *testing.T) {
	s, ep, creds := openstack.DevStack(t)
	host := os.Getenv("VEHO_DEVSTACK_HOST_IP")
	if host == "" {
		t.Skip("no VEHO_DEVSTACK_HOST_IP")
	}
	data := bytes.Repeat([]byte("veho web-download "), 200000)
	ln, err := net.Listen("tcp", host+":0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "img", time.Time{}, bytes.NewReader(data))
	}), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	settings, _ := json.Marshal(s)
	d, err := Factory(settings)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := json.Marshal(config{Endpoint: ep, Upload: "web-download", DiskFormat: "raw", ContainerFormat: "bare"})
	ctx := context.Background()
	c, err := d.Open(ctx, cfg, creds["application_credential"])
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	name := fmt.Sprintf("veho-web-download-%d", time.Now().UnixNano())
	u, err := c.Begin(ctx, plugin.Ref{Key: name}, plugin.ObjectInfo{Size: int64(len(data))}, plugin.PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := u.(plugin.Puller).Pull(ctx, "http://"+ln.Addr().String()+"/img")
	if err != nil {
		_ = u.Abort(ctx)
		t.Fatal(err)
	}
	r, err := c.ReadRange(ctx, ref, 0, int64(len(data)), "")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(r)
	_ = r.Close()
	if !bytes.Equal(got, data) {
		t.Fatalf("read back %d bytes, differing", len(got))
	}
	if _, err := c.Stat(ctx, plugin.Ref{Key: name}); !errors.Is(err, plugin.ErrNotFound) {
		t.Fatalf("visible before commit: %v", err)
	}
	if _, err := u.Commit(ctx, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Delete(context.Background(), plugin.Ref{Key: name}) })
	if info, err := c.Stat(ctx, plugin.Ref{Key: name}); err != nil || info.Size != int64(len(data)) {
		t.Fatalf("stat: %+v, %v", info, err)
	}
}
