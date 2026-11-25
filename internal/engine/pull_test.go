package engine

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/dihedron/veho/pkg/plugin"
)

// memClient serves objects from memory; Presign, when set, makes it a
// Presigner (memPresigner).
type memClient struct {
	plugin.Client
	objects map[string][]byte
}

func (c memClient) ReadRange(_ context.Context, ref plugin.Ref, off, length int64, _ string) (io.ReadCloser, error) {
	b, ok := c.objects[ref.Key]
	if !ok {
		return nil, plugin.ErrNotFound
	}
	end := min(off+length, int64(len(b)))
	return io.NopCloser(bytes.NewReader(b[off:end])), nil
}

type memPresigner struct {
	memClient
	err error
}

func (c memPresigner) PresignRead(_ context.Context, ref plugin.Ref, _ string, ttl time.Duration) (string, error) {
	return "mem://" + ref.Key + "?" + ttl.String(), c.err
}

// memPuller copies the URL's object into dst under "pulled", changed by
// mangle when set.
type memPuller struct {
	src, dst memClient
	mangle   func([]byte) []byte
	err      error
}

func (p memPuller) PullTTL() time.Duration { return time.Hour }

func (p memPuller) Pull(_ context.Context, url string) (plugin.Ref, error) {
	if p.err != nil {
		return plugin.Ref{}, p.err
	}
	if url != "mem://obj?1h0m0s" {
		return plugin.Ref{}, errors.New("unexpected URL " + url)
	}
	b := append([]byte{}, p.src.objects["obj"]...)
	if p.mangle != nil {
		b = p.mangle(b)
	}
	p.dst.objects["pulled"] = b
	return plugin.Ref{Key: "pulled"}, nil
}

func TestPullChecked(t *testing.T) {
	data := bytes.Repeat([]byte("0123456789"), 1000)
	a := attempt{sourceInfo: sourceInfo{Size: int64(len(data)), PartSize: 3000}, SourcePath: "obj"}
	sums, _ := readSums(context.Background(), memClient{objects: map[string][]byte{"obj": data}}, plugin.Ref{Key: "obj"}, a, "")
	good, _ := digest(a.PartSize, a.Size, sums)
	ctx := context.Background()

	run := func(src plugin.Client, p memPuller, recorded string, record func(string) (string, error)) ([][]byte, error) {
		return pullChecked(ctx, src, p.dst, p, a, recorded, record)
	}
	fresh := func() (memPresigner, memPuller) {
		src := memPresigner{memClient: memClient{objects: map[string][]byte{"obj": data}}}
		return src, memPuller{src: src.memClient, dst: memClient{objects: map[string][]byte{}}}
	}

	// no digest recorded: the source's is, then the copy matches it
	src, p := fresh()
	var stored string
	got, err := run(src, p, "", func(d string) (string, error) { stored = d; return d, nil })
	if err != nil || stored != good || len(got) != 4 {
		t.Fatalf("first copy: %d sums, %q, %v", len(got), stored, err)
	}
	// a digest recorded by another destination meanwhile wins
	src, p = fresh()
	if _, err := run(src, p, "", func(string) (string, error) { return "other", nil }); plugin.ClassOf(err) != plugin.Integrity {
		t.Fatalf("recorded meanwhile: %v", err)
	}
	// recorded: the source is not read again
	src, p = fresh()
	if _, err := run(src, p, good, func(string) (string, error) { t.Fatal("recorded again"); return "", nil }); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		mangle func([]byte) []byte
		want   plugin.Class
	}{
		"other bytes": {func(b []byte) []byte { b[5] ^= 1; return b }, plugin.Integrity},
		"short":       {func(b []byte) []byte { return b[:len(b)-1] }, plugin.Integrity},
	} {
		src, p := fresh()
		p.mangle = tc.mangle
		if _, err := run(src, p, good, nil); plugin.ClassOf(err) != tc.want {
			t.Errorf("%s: %v", name, err)
		}
	}
	// failures of the source, the presign, the pull, the record
	src, p = fresh()
	if _, err := run(src.memClient, p, good, nil); plugin.ClassOf(err) != plugin.Permanent {
		t.Errorf("no presigner: %v", err)
	}
	src.err = plugin.Wrapf(plugin.Permanent, "no URL")
	if _, err := run(src, p, good, nil); plugin.ClassOf(err) != plugin.Permanent {
		t.Errorf("presign: %v", err)
	}
	src, p = fresh()
	p.err = plugin.Wrapf(plugin.Transient, "glance down")
	if _, err := run(src, p, good, nil); plugin.ClassOf(err) != plugin.Transient {
		t.Errorf("pull: %v", err)
	}
	src, p = fresh()
	delete(src.objects, "obj")
	if _, err := run(src, p, "", nil); !errors.Is(err, plugin.ErrNotFound) {
		t.Errorf("source gone: %v", err)
	}
	src, p = fresh()
	if _, err := run(src, p, "", func(string) (string, error) { return "", errors.New("db down") }); err == nil {
		t.Error("record failure ignored")
	}
}
