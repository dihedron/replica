// Package glance is the OpenStack Glance driver (.specs/veho-openstack.md
// §3): a source by image ID, read by ranges, and a destination by image
// name, uploaded with put or glance-direct, hidden until it is committed.
package glance

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gophercloud/gophercloud/v2"

	"github.com/dihedron/veho/pkg/plugin"
	"github.com/dihedron/veho/pkg/plugin/openstack"
)

// Driver is the Glance driver under one platform's settings.
type Driver struct {
	cloud *openstack.Cloud
	// poll is how often Commit asks Glance whether an image is active.
	poll time.Duration
}

// Factory builds the driver from its settings: allow_cidrs and plaintext.
func Factory(raw json.RawMessage) (plugin.Driver, error) {
	var s openstack.Settings
	if err := plugin.DecodeSettings(raw, &s); err != nil {
		return nil, fmt.Errorf("glance: %w", err)
	}
	c, err := openstack.NewCloud(s)
	if err != nil {
		return nil, fmt.Errorf("glance: %w", err)
	}
	return Driver{cloud: c, poll: 2 * time.Second}, nil
}

func (Driver) Name() string      { return "glance" }
func (Driver) Schemes() []string { return []string{"glance"} }

// Capabilities: Glance has no resumable upload, and the bytes of an attempt
// go in order into one request.
func (Driver) Capabilities() plugin.Capabilities {
	return plugin.Capabilities{Source: true, Sink: true, RangeRead: true}
}

//go:embed schema.json
var own []byte

// schema is the endpoint configuration's JSON Schema (PLG-10): own, with
// the cloud's fields.
var schema = func() []byte {
	b, err := openstack.Schema(own)
	if err != nil {
		panic(err) // embedded files: a test catches it
	}
	return b
}()

func (Driver) ConfigSchema() []byte { return schema }

// checkKey: an image ID or name, without control characters, at most 255
// bytes (Glance's limit on names).
func checkKey(key string) error {
	if key == "" || len(key) > 255 || strings.ContainsFunc(key, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return plugin.Wrapf(plugin.Permanent, "glance: %q is not an image ID or name", key)
	}
	return nil
}

// ParseRef parses glance://<image ID or name>: a source is an ID, a
// destination a name.
func (Driver) ParseRef(raw string) (plugin.Ref, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "glance" || u.RawQuery != "" || u.Fragment != "" {
		return plugin.Ref{}, plugin.Wrapf(plugin.Permanent, "glance: %q is not glance://<image>", raw)
	}
	key := u.Host
	if u.Path != "" {
		key += u.Path
	}
	if err := checkKey(key); err != nil {
		return plugin.Ref{}, err
	}
	return plugin.Ref{Key: key}, nil
}

// isID: a key that is a UUID names an image by its ID; destinations may not
// have such names, so a key is never both.
func isID(key string) bool {
	_, err := uuid.Parse(key)
	return err == nil && len(key) == 36
}

type config struct {
	openstack.Endpoint
	Upload            string            `json:"upload"`
	DiskFormat        string            `json:"disk_format"`
	ContainerFormat   string            `json:"container_format"`
	Visibility        string            `json:"visibility"`
	Properties        map[string]string `json:"properties"`
	Stores            []string          `json:"stores"`
	WebDownloadURLTTL string            `json:"web_download_url_ttl"`
}

// reserved are the properties Glance or the driver sets.
var reserved = map[string]bool{
	"id": true, "name": true, "status": true, "visibility": true, "os_hidden": true, "owner": true, "size": true,
	"checksum": true, "os_hash_algo": true, "os_hash_value": true, "disk_format": true, "container_format": true,
	"protected": true, "locations": true, "direct_url": true, "veho_upload": true,
}

// Open authenticates to Keystone and finds Glance in the catalog.
func (d Driver) Open(ctx context.Context, raw plugin.Config, creds plugin.Credentials) (plugin.Client, error) {
	var cfg config
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, plugin.Wrapf(plugin.Permanent, "glance: configuration: %v", err)
	}
	switch cfg.Upload {
	case "":
		cfg.Upload = "put"
	case "put", "glance-direct":
	case "web-download":
		return nil, plugin.Wrapf(plugin.Permanent, "glance: upload web-download is not available yet")
	default:
		return nil, plugin.Wrapf(plugin.Permanent, "glance: upload: want put, glance-direct or web-download")
	}
	if len(cfg.Stores) > 0 && cfg.Upload != "glance-direct" {
		return nil, plugin.Wrapf(plugin.Permanent, "glance: stores need upload glance-direct")
	}
	if cfg.Visibility == "" {
		cfg.Visibility = "private"
	}
	for k := range cfg.Properties {
		if reserved[k] || k == "" || strings.HasPrefix(k, "os_glance") {
			return nil, plugin.Wrapf(plugin.Permanent, "glance: property %q is not allowed", k)
		}
	}
	if err := d.cloud.CheckEndpoint(cfg.Endpoint); err != nil {
		return nil, plugin.Wrap(fmt.Errorf("glance: %w", err), plugin.Permanent)
	}
	sc, err := d.cloud.Connect(ctx, cfg.Endpoint, creds, openstack.Image)
	if err != nil {
		return nil, fmt.Errorf("glance: %w", err)
	}
	project, err := openstack.ProjectID(sc)
	if err != nil {
		return nil, fmt.Errorf("glance: %w", err)
	}
	return &client{sc: sc, cfg: cfg, project: project, poll: d.poll}, nil
}

type client struct {
	sc      *gophercloud.ServiceClient
	cfg     config
	project string
	poll    time.Duration

	once    sync.Once
	minor   int // Glance's most recent v2 minor version
	minorOK error
}

func (c *client) Close() error {
	if t, ok := c.sc.HTTPClient.Transport.(*http.Transport); ok {
		t.CloseIdleConnections()
	}
	return nil
}

// classify is openstack.Classify, with Glance's conflict (an image in a
// state that refuses the call) as Permanent.
func classify(ctx context.Context, op string, err error) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return openstack.Classify(err, "glance: "+op)
}

// call sends a JSON request and decodes the answer into out, if any.
func (c *client) call(ctx context.Context, method, path string, headers map[string]string, in, out any, ok ...int) error {
	opts := &gophercloud.RequestOpts{MoreHeaders: headers, OkCodes: ok}
	switch b := in.(type) {
	case nil:
	case []byte:
		opts.RawBody = bytes.NewReader(b)
	default:
		opts.JSONBody = in
	}
	if out != nil {
		opts.JSONResponse = out
	}
	_, err := c.sc.Request(ctx, method, c.sc.ServiceURL(path), opts)
	return err
}

// version is Glance's most recent v2 minor version, asked once: 2.6 has the
// import methods (Queens), 2.7 os_hidden and the multihash (Rocky).
func (c *client) version(ctx context.Context) (int, error) {
	c.once.Do(func() {
		var doc struct {
			Versions []struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"versions"`
		}
		// the version document is at the service's root, unversioned
		_, err := c.sc.Request(ctx, http.MethodGet, c.sc.Endpoint, &gophercloud.RequestOpts{JSONResponse: &doc, OkCodes: []int{http.StatusOK, http.StatusMultipleChoices}})
		if err != nil {
			c.minorOK = classify(ctx, "versions", err)
			return
		}
		c.minor = -1
		for _, v := range doc.Versions {
			major, minor, ok := strings.Cut(strings.TrimPrefix(v.ID, "v"), ".")
			n, err := strconv.Atoi(minor)
			if ok && err == nil && major == "2" && n > c.minor && (v.Status == "CURRENT" || v.Status == "SUPPORTED") {
				c.minor = n
			}
		}
		if c.minor < 0 {
			c.minorOK = plugin.Wrapf(plugin.Permanent, "glance: no image API v2")
		}
	})
	return c.minor, c.minorOK
}

// image is the part of Glance's image the driver reads.
type image struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Status      string `json:"status"`
	Size        *int64 `json:"size"`
	Checksum    string `json:"checksum"`
	HashAlgo    string `json:"os_hash_algo"`
	HashValue   string `json:"os_hash_value"`
	Owner       string `json:"owner"`
	Upload      string `json:"veho_upload"`
	Hidden      bool   `json:"os_hidden"`
	FailedStore string `json:"os_glance_failed_import"`
}

// version is the image's hash: its data never changes once active.
func (im image) version() string {
	if im.HashAlgo != "" && im.HashValue != "" {
		return im.HashAlgo + ":" + im.HashValue
	}
	if im.Checksum != "" {
		return "md5:" + im.Checksum
	}
	return ""
}

func (im image) info() plugin.ObjectInfo {
	var size int64
	if im.Size != nil {
		size = *im.Size
	}
	return plugin.ObjectInfo{Size: size, Version: im.version()}
}

func (c *client) get(ctx context.Context, id string) (image, error) {
	var im image
	if err := c.call(ctx, http.MethodGet, "images/"+url.PathEscape(id), nil, nil, &im, http.StatusOK); err != nil {
		return image{}, classify(ctx, "image "+id, err)
	}
	return im, nil
}

// named are the project's visible active images of that name.
func (c *client) named(ctx context.Context, name string) ([]image, error) {
	q := url.Values{"name": {name}, "owner": {c.project}, "status": {"active"}}
	var page struct {
		Images []image `json:"images"`
	}
	if err := c.call(ctx, http.MethodGet, "images?"+q.Encode(), nil, nil, &page, http.StatusOK); err != nil {
		return nil, classify(ctx, "images named "+name, err)
	}
	return page.Images, nil
}

// lookup is the image of ref: by ID, or the one visible active image of the
// project with that name.
func (c *client) lookup(ctx context.Context, ref plugin.Ref) (image, error) {
	if err := checkKey(ref.Key); err != nil {
		return image{}, err
	}
	if isID(ref.Key) {
		return c.get(ctx, ref.Key)
	}
	ims, err := c.named(ctx, ref.Key)
	switch {
	case err != nil:
		return image{}, err
	case len(ims) == 0:
		return image{}, fmt.Errorf("glance: %s: %w", ref.Key, plugin.ErrNotFound)
	case len(ims) > 1:
		return image{}, plugin.Wrapf(plugin.Permanent, "glance: %d images are named %s", len(ims), ref.Key)
	}
	return ims[0], nil
}

// --- source -----------------------------------------------------------------

// Stat is the image, which must be active; its ranges are probed, so a
// Glance that ignores them is refused before any transfer.
func (c *client) Stat(ctx context.Context, ref plugin.Ref) (plugin.ObjectInfo, error) {
	im, err := c.lookup(ctx, ref)
	if err != nil {
		return plugin.ObjectInfo{}, err
	}
	if im.Status != "active" {
		return plugin.ObjectInfo{}, plugin.Wrapf(plugin.Permanent, "glance: image %s is %s, not active", im.ID, im.Status)
	}
	if info := im.info(); info.Size > 0 {
		r, err := c.data(ctx, im.ID, 0, 1)
		if err != nil {
			return plugin.ObjectInfo{}, err
		}
		_ = r.Close()
	}
	return im.info(), nil
}

// data is [off, off+length) of the image's data, exactly.
func (c *client) data(ctx context.Context, id string, off, length int64) (io.ReadCloser, error) {
	rng := fmt.Sprintf("bytes=%d-%d", off, off+length-1)
	resp, err := c.sc.Request(ctx, http.MethodGet, c.sc.ServiceURL("images", url.PathEscape(id), "file"),
		&gophercloud.RequestOpts{MoreHeaders: map[string]string{"Range": rng}, OkCodes: []int{http.StatusOK, http.StatusPartialContent}, KeepResponseBody: true})
	if err != nil {
		return nil, classify(ctx, "data of "+id, err)
	}
	if resp.StatusCode != http.StatusPartialContent {
		_ = resp.Body.Close()
		return nil, plugin.Wrapf(plugin.Permanent, "glance: this Glance does not serve ranges of image data (answered %d)", resp.StatusCode)
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Range"), fmt.Sprintf("bytes %d-%d/", off, off+length-1)) || resp.ContentLength != length {
		_ = resp.Body.Close()
		return nil, plugin.Wrapf(plugin.Permanent, "glance: range %s of %s: answered %q", rng, id, resp.Header.Get("Content-Range"))
	}
	return resp.Body, nil
}

// ReadRange checks the image's status and hash before the data request: a
// deactivated, deleted or re-uploaded image is SourceChanged (OS-6). A
// source is an image ID; a name is looked up as Stat does.
func (c *client) ReadRange(ctx context.Context, ref plugin.Ref, off, length int64, ifVersion string) (io.ReadCloser, error) {
	if off < 0 || length < 0 {
		return nil, plugin.Wrapf(plugin.Permanent, "glance: range [%d, +%d)", off, length)
	}
	im, err := c.lookup(ctx, ref)
	switch {
	case errors.Is(err, plugin.ErrNotFound) && ifVersion != "":
		return nil, plugin.Wrapf(plugin.SourceChanged, "glance: image %s is gone", ref.Key)
	case err != nil:
		return nil, err
	case im.Status != "active" && ifVersion != "":
		return nil, plugin.Wrapf(plugin.SourceChanged, "glance: image %s is %s", ref.Key, im.Status)
	case im.Status != "active":
		return nil, plugin.Wrapf(plugin.Permanent, "glance: image %s is %s, not active", ref.Key, im.Status)
	case ifVersion != "" && im.version() != ifVersion:
		return nil, plugin.Wrapf(plugin.SourceChanged, "glance: image %s changed", ref.Key)
	}
	info := im.info()
	if length == 0 {
		if off > info.Size {
			return nil, plugin.Wrapf(plugin.Permanent, "glance: offset %d beyond %d bytes", off, info.Size)
		}
		return io.NopCloser(bytes.NewReader(nil)), nil
	}
	return c.data(ctx, im.ID, off, length)
}

// Delete deletes the image of ref (by ID, or the one of that name).
func (c *client) Delete(ctx context.Context, ref plugin.Ref) error {
	im, err := c.lookup(ctx, ref)
	if errors.Is(err, plugin.ErrNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	return c.remove(ctx, im.ID)
}

// remove deletes an image by ID; one already gone is no error.
func (c *client) remove(ctx context.Context, id string) error {
	err := classify(ctx, "delete "+id, c.call(ctx, http.MethodDelete, "images/"+url.PathEscape(id), nil, nil, nil, http.StatusNoContent))
	if errors.Is(err, plugin.ErrNotFound) {
		return nil
	}
	return err
}
