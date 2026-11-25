//go:build integration

package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/dihedron/veho/pkg/plugin"
)

// presigner is the file driver with PresignRead: a test:// URL of the file.
type presigner struct{ plugin.Driver }

func (presigner) Name() string { return "presign" }

func (p presigner) Open(ctx context.Context, cfg plugin.Config, creds plugin.Credentials) (plugin.Client, error) {
	var c struct {
		Root string `json:"root"`
	}
	_ = json.Unmarshal(cfg, &c)
	cl, err := p.Driver.Open(ctx, cfg, creds)
	return presignClient{Client: cl, root: c.Root}, err
}

type presignClient struct {
	plugin.Client
	root string
}

func (c presignClient) PresignRead(_ context.Context, ref plugin.Ref, _ string, ttl time.Duration) (string, error) {
	if ttl != time.Hour {
		return "", plugin.Wrapf(plugin.Permanent, "ttl %s", ttl)
	}
	return "test://" + filepath.Join(c.root, ref.Key), nil
}

// puller is a destination that pulls, as Glance's web-download: Pull reads
// the URL's file (corrupted when corrupt is set), the pulled bytes are read
// back from memory, and Commit writes them to the file endpoint.
type puller struct {
	plugin.Driver
	mu      sync.Mutex
	pulled  map[string][]byte
	corrupt bool
}

func (*puller) Name() string { return "puller" }

func (*puller) Capabilities() plugin.Capabilities {
	return plugin.Capabilities{Source: true, Sink: true, RangeRead: true}
}

func (p *puller) Open(ctx context.Context, cfg plugin.Config, creds plugin.Credentials) (plugin.Client, error) {
	cl, err := p.Driver.Open(ctx, cfg, creds)
	return pullClient{Client: cl, p: p}, err
}

type pullClient struct {
	plugin.Client
	p *puller
}

func (c pullClient) ReadRange(ctx context.Context, ref plugin.Ref, off, length int64, ifVersion string) (io.ReadCloser, error) {
	if id, ok := strings.CutPrefix(ref.Key, "pulled:"); ok {
		c.p.mu.Lock()
		b := c.p.pulled[id]
		c.p.mu.Unlock()
		return io.NopCloser(bytes.NewReader(b[off : off+length])), nil
	}
	return c.Client.ReadRange(ctx, ref, off, length, ifVersion)
}

func (c pullClient) Begin(ctx context.Context, ref plugin.Ref, info plugin.ObjectInfo, opts plugin.PutOptions) (plugin.Upload, error) {
	u, err := c.Client.Begin(ctx, ref, info, opts)
	return &pullUpload{Upload: u, p: c.p, id: uuid.New().String()}, err
}

type pullUpload struct {
	plugin.Upload
	p  *puller
	id string
}

func (u *pullUpload) PullTTL() time.Duration { return time.Hour }

func (u *pullUpload) Pull(_ context.Context, url string) (plugin.Ref, error) {
	b, err := os.ReadFile(strings.TrimPrefix(url, "test://"))
	if err != nil {
		return plugin.Ref{}, plugin.Wrap(err, plugin.Permanent)
	}
	u.p.mu.Lock()
	defer u.p.mu.Unlock()
	if u.p.corrupt && len(b) > 0 {
		b[len(b)/2] ^= 1
	}
	u.p.pulled[u.id] = b
	return plugin.Ref{Key: "pulled:" + u.id}, nil
}

func (u *pullUpload) Commit(ctx context.Context, _ []plugin.PartResult) (plugin.ObjectInfo, error) {
	u.p.mu.Lock()
	b := u.p.pulled[u.id]
	u.p.mu.Unlock()
	r, err := u.Upload.WritePart(ctx, 0, bytes.NewReader(b), int64(len(b)))
	if err != nil {
		return plugin.ObjectInfo{}, err
	}
	return u.Upload.Commit(ctx, []plugin.PartResult{r})
}

// OS-T5: a destination that pulls gets the source's URL; the job's digest
// is recorded from the source before the pulled copy is read back; a copy
// with other bytes fails as Integrity.
func TestPull(t *testing.T) {
	f := setup(t)
	srcDir := f.fileEndpoint(t, "source", "presign", "lab-a")
	dst := f.fileEndpoint(t, "glance", "puller", "lab-b")
	f.launch(t)
	j := f.submit(t, srcDir, 9*mib+7, destPolicy(1, "", "glance"))
	j.run(t, "glance")
	s := j.state(t)
	if s.State != "SUCCEEDED" || s.Transfers["glance"] != "SUCCEEDED" || len(s.Digest) != 64 {
		t.Fatalf("job: %+v", s)
	}
	same(t, j.src, filepath.Join(dst, "copy", filepath.Base(j.src)))

	f.puller.mu.Lock()
	f.puller.corrupt = true
	f.puller.mu.Unlock()
	j = f.submit(t, srcDir, 3*mib, destPolicy(1, "", "glance"))
	j.run(t, "glance")
	if s := j.state(t); s.Transfers["glance"] != "FAILED" || s.LastError["glance"] != "Integrity" || s.Attempts["glance"] != maxIntegrity {
		t.Fatalf("a corrupted pull: %+v", s)
	}
	if _, err := os.Stat(filepath.Join(dst, "copy", filepath.Base(j.src))); !os.IsNotExist(err) {
		t.Fatalf("the corrupted copy is in place: %v", err)
	}
}
