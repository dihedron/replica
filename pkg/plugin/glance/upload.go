package glance

import (
	"context"
	"crypto/md5" //nolint:gosec // Glance's checksum before Rocky
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/dihedron/veho/pkg/plugin"
	"github.com/dihedron/veho/pkg/plugin/openstack"
	"github.com/dihedron/veho/pkg/plugin/stream"
)

// token is an upload's identity: Glance cannot resume one, so the token
// only finds the image to keep (committed) or delete (begin again).
type token struct {
	ImageID  string `json:"image_id"`
	Name     string `json:"name"`
	UploadID string `json:"upload_id"`
}

// upload streams the parts of an attempt, in order, into one request.
type upload struct {
	c      *client
	t      token
	hidden bool           // os_hidden, else a temporary name (Glance before Rocky)
	s      *stream.Stream // put, glance-direct: the data request

	mu        sync.Mutex
	sha512    hash.Hash
	md5       hash.Hash
	broken    error // a step after the data failed: the upload must begin again
	committed *plugin.ObjectInfo
	pull      bool // web-download: Glance fetches the data
	pulled    *image
}

// PullTTL: a web-download upload pulls from a URL that must live that long.
func (u *upload) PullTTL() time.Duration {
	if !u.pull {
		return 0
	}
	return u.c.ttl
}

// Pull imports the image from url with web-download and waits for it to be
// active; the image is still hidden, and the reference (its ID) reads it
// back before Commit. The URL is a credential: it is in no error or log.
func (u *upload) Pull(ctx context.Context, src string) (plugin.Ref, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	switch {
	case !u.pull:
		return plugin.Ref{}, plugin.Wrapf(plugin.Permanent, "glance: the endpoint's upload is %s, not web-download", u.c.cfg.Upload)
	case u.committed != nil:
		return plugin.Ref{}, plugin.Wrapf(plugin.ResumeInvalid, "glance: the upload of %s is committed", u.t.Name)
	case u.pulled != nil:
		return plugin.Ref{Key: u.t.ImageID}, nil
	}
	body := map[string]any{"method": map[string]string{"name": "web-download", "uri": src}}
	if err := u.c.call(ctx, http.MethodPost, "images/"+url.PathEscape(u.t.ImageID)+"/import", nil, body, nil, http.StatusAccepted); err != nil {
		return plugin.Ref{}, classify(ctx, "web-download of "+u.t.Name, err)
	}
	im, err := u.active(ctx)
	if err != nil {
		return plugin.Ref{}, err
	}
	u.pulled = &im
	return plugin.Ref{Key: u.t.ImageID}, nil
}

// tempName is an image's name while it uploads, on a Glance without
// os_hidden: it says what it is, and is never a destination's name.
func tempName(uploadID string) string { return "veho-upload-" + uploadID }

// methods checks that Glance offers the endpoint's upload method and stores
// (§3.3): before any image is created.
func (c *client) methods(ctx context.Context, minor int) error {
	method := c.cfg.Upload
	if method == "put" {
		return nil
	}
	if minor < 6 {
		return plugin.Wrapf(plugin.Permanent, "glance: %s needs the image import API (v2.6, Queens); this Glance has v2.%d", method, minor)
	}
	var info struct {
		Methods struct {
			Value []string `json:"value"`
		} `json:"import-methods"`
	}
	if err := c.call(ctx, http.MethodGet, "info/import", nil, nil, &info, http.StatusOK); err != nil {
		return classify(ctx, "import methods", err)
	}
	if !slices.Contains(info.Methods.Value, method) {
		return plugin.Wrapf(plugin.Permanent, "glance: this Glance does not offer %s (it offers %v)", method, info.Methods.Value)
	}
	if len(c.cfg.Stores) == 0 || slices.Equal(c.cfg.Stores, []string{"*"}) {
		return nil
	}
	var stores struct {
		Stores []struct {
			ID string `json:"id"`
		} `json:"stores"`
	}
	if err := c.call(ctx, http.MethodGet, "info/stores", nil, nil, &stores, http.StatusOK); err != nil {
		if code, _ := openstack.StatusOf(err); code == http.StatusNotFound || code == http.StatusBadRequest {
			return plugin.Wrapf(plugin.Permanent, "glance: stores need a Glance with multiple stores (Rocky or later, enabled)")
		}
		return classify(ctx, "stores", err)
	}
	for _, want := range c.cfg.Stores {
		if !slices.ContainsFunc(stores.Stores, func(s struct {
			ID string `json:"id"`
		}) bool {
			return s.ID == want
		}) {
			return plugin.Wrapf(plugin.Permanent, "glance: this Glance has no store %q", want)
		}
	}
	return nil
}

// Begin creates the image, hidden (or under a temporary name), with a new
// ID; its data is sent by the parts.
func (c *client) Begin(ctx context.Context, ref plugin.Ref, info plugin.ObjectInfo, _ plugin.PutOptions) (plugin.Upload, error) {
	if err := checkKey(ref.Key); err != nil {
		return nil, err
	}
	if isID(ref.Key) {
		return nil, plugin.Wrapf(plugin.Permanent, "glance: a destination is an image name, and a name may not be a UUID (%s)", ref.Key)
	}
	if c.cfg.DiskFormat == "" || c.cfg.ContainerFormat == "" {
		return nil, plugin.Wrapf(plugin.Permanent, "glance: a destination needs disk_format and container_format")
	}
	if info.Size < 0 {
		return nil, plugin.Wrapf(plugin.Permanent, "glance: size %d", info.Size)
	}
	minor, err := c.version(ctx)
	if err != nil {
		return nil, err
	}
	if err := c.methods(ctx, minor); err != nil {
		return nil, err
	}
	u := &upload{c: c, hidden: minor >= 7, sha512: sha512.New(), md5: md5.New(), pull: c.cfg.Upload == "web-download"} //nolint:gosec // Glance's checksum
	u.t = token{ImageID: uuid.NewString(), Name: ref.Key, UploadID: uuid.NewString()}
	if !u.pull {
		u.s = stream.New("glance: upload of "+ref.Key, info.Size, io.MultiWriter(u.sha512, u.md5), u.send)
	}
	body := map[string]any{
		"id": u.t.ImageID, "name": ref.Key, "disk_format": c.cfg.DiskFormat, "container_format": c.cfg.ContainerFormat,
		"visibility": c.cfg.Visibility, "veho_upload": u.t.UploadID,
	}
	for k, v := range c.cfg.Properties {
		body[k] = v
	}
	if u.hidden {
		body["os_hidden"] = true
	} else {
		body["name"] = tempName(u.t.UploadID)
	}
	if err := c.call(ctx, http.MethodPost, "images", nil, body, nil, http.StatusCreated); err != nil {
		return nil, classify(ctx, "create image "+ref.Key, err)
	}
	return u, nil
}

// Resume finds the token's image: published, it is the committed object;
// otherwise it is deleted and the upload begins again (ResumeInvalid).
func (c *client) Resume(ctx context.Context, ref plugin.Ref, raw []byte) (plugin.Upload, error) {
	var t token
	if err := json.Unmarshal(raw, &t); err != nil || !isID(t.ImageID) {
		return nil, plugin.Wrapf(plugin.ResumeInvalid, "glance: malformed upload token")
	}
	if t.Name != ref.Key {
		return nil, plugin.Wrapf(plugin.ResumeInvalid, "glance: the upload token is of %s", t.Name)
	}
	im, err := c.get(ctx, t.ImageID)
	switch {
	case errors.Is(err, plugin.ErrNotFound):
		return nil, plugin.Wrapf(plugin.ResumeInvalid, "glance: the image of the upload of %s is gone", t.Name)
	case err != nil:
		return nil, err
	case im.Status == "active" && !im.Hidden && im.Name == t.Name && im.Upload == t.UploadID:
		info := im.info()
		return &upload{c: c, t: t, committed: &info}, nil
	}
	if err := c.remove(ctx, t.ImageID); err != nil {
		return nil, err
	}
	return nil, plugin.Wrapf(plugin.ResumeInvalid, "glance: an upload to Glance cannot resume: begin again")
}

func (u *upload) Token() []byte {
	b, _ := json.Marshal(u.t) // strings only: cannot fail
	return b
}

// send is the data request: PUT of the file (put) or of the staging area
// (glance-direct), of the size announced at Begin, or chunked when none was
// (some Glance deployments refuse chunked uploads: the engine always
// announces it).
func (u *upload) send(ctx context.Context, body io.Reader, length int64) error {
	where := "file"
	if u.c.cfg.Upload == "glance-direct" {
		where = "stage"
	}
	if body == nil {
		body = http.NoBody
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u.c.sc.ServiceURL("images", url.PathEscape(u.t.ImageID), where), body)
	if err != nil {
		return plugin.Wrap(fmt.Errorf("glance: upload: %w", err), plugin.Permanent)
	}
	req.ContentLength = length
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-Auth-Token", u.c.sc.Token())
	resp, err := u.c.sc.HTTPClient.Do(req)
	if err != nil {
		return openstack.Classify(err, "glance: upload of "+u.t.Name)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	_ = resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated:
		return nil
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusConflict:
		return plugin.Wrapf(plugin.ResumeInvalid, "glance: upload of %s: the image is gone or no longer takes data (%d): begin again", u.t.Name, resp.StatusCode)
	case resp.StatusCode == http.StatusRequestEntityTooLarge:
		return plugin.Wrapf(plugin.Permanent, "glance: upload of %s: the image is over the project's quota or Glance's limit", u.t.Name)
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return plugin.Wrapf(plugin.Transient, "glance: upload of %s: not authorised (%d): the token may have expired", u.t.Name, resp.StatusCode)
	case resp.StatusCode >= 500 || resp.StatusCode == http.StatusRequestTimeout:
		return plugin.Wrapf(plugin.Transient, "glance: upload of %s: answered %d", u.t.Name, resp.StatusCode)
	default:
		return plugin.Wrapf(plugin.Permanent, "glance: upload of %s: answered %d", u.t.Name, resp.StatusCode)
	}
}

// fail records that a step after the data failed: the upload must begin
// again.
func (u *upload) fail(err error) error {
	if u.broken == nil {
		u.broken = err
	}
	return err
}

func (u *upload) WritePart(ctx context.Context, partNo int, r io.Reader, length int64) (plugin.PartResult, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	switch {
	case u.pull:
		return plugin.PartResult{}, plugin.Wrapf(plugin.Permanent, "glance: a web-download upload takes no parts")
	case u.committed != nil:
		return plugin.PartResult{}, plugin.Wrapf(plugin.ResumeInvalid, "glance: the upload of %s is committed", u.t.Name)
	case u.broken != nil:
		return plugin.PartResult{}, plugin.Wrapf(plugin.ResumeInvalid, "glance: the upload of %s broke (%v): begin again", u.t.Name, u.broken)
	}
	return u.s.Write(ctx, partNo, r, length)
}

func (u *upload) ListParts(context.Context) ([]plugin.PartResult, error) {
	if u.s == nil {
		return nil, nil
	}
	return u.s.Parts(), nil
}

// Commit ends the data request, imports (glance-direct), waits for the image
// to be active, compares Glance's hash with the bytes sent, publishes the
// image under its name and removes the project's other images of that name.
func (u *upload) Commit(ctx context.Context, parts []plugin.PartResult) (plugin.ObjectInfo, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.committed != nil {
		return *u.committed, nil
	}
	if u.broken != nil {
		return plugin.ObjectInfo{}, plugin.Wrapf(plugin.ResumeInvalid, "glance: the upload of %s broke: begin again", u.t.Name)
	}
	if u.pull { // the engine has read the pulled image back and checked it
		if u.pulled == nil {
			return plugin.ObjectInfo{}, plugin.Wrapf(plugin.ResumeInvalid, "glance: commit of %s before its web-download", u.t.Name)
		}
		return u.publish(ctx, *u.pulled)
	}
	if err := u.s.Close(parts); err != nil {
		return plugin.ObjectInfo{}, err
	}
	if u.c.cfg.Upload == "glance-direct" {
		body := map[string]any{"method": map[string]string{"name": "glance-direct"}}
		switch {
		case slices.Equal(u.c.cfg.Stores, []string{"*"}):
			body["all_stores"] = true
		case len(u.c.cfg.Stores) > 0:
			body["stores"] = u.c.cfg.Stores
		}
		if err := u.c.call(ctx, http.MethodPost, "images/"+url.PathEscape(u.t.ImageID)+"/import", nil, body, nil, http.StatusAccepted); err != nil {
			return plugin.ObjectInfo{}, classify(ctx, "import "+u.t.Name, err)
		}
	}
	im, err := u.active(ctx)
	if err != nil {
		return plugin.ObjectInfo{}, err
	}
	sum, want := hex.EncodeToString(u.md5.Sum(nil)), im.Checksum
	if im.HashAlgo == "sha512" && im.HashValue != "" {
		sum, want = hex.EncodeToString(u.sha512.Sum(nil)), im.HashValue
	}
	if sum != want {
		return plugin.ObjectInfo{}, u.fail(plugin.Wrapf(plugin.Integrity, "glance: %s: Glance's hash is not that of the bytes sent", u.t.Name))
	}
	return u.publish(ctx, im)
}

// publish shows the image under its name and removes the project's other
// images of that name.
func (u *upload) publish(ctx context.Context, im image) (plugin.ObjectInfo, error) {
	older, err := u.c.named(ctx, u.t.Name)
	if err != nil {
		return plugin.ObjectInfo{}, err
	}
	patch := []map[string]any{{"op": "replace", "path": "/os_hidden", "value": false}}
	if !u.hidden {
		patch = []map[string]any{{"op": "replace", "path": "/name", "value": u.t.Name}}
	}
	b, _ := json.Marshal(patch) // cannot fail
	if err := u.c.call(ctx, http.MethodPatch, "images/"+url.PathEscape(u.t.ImageID), map[string]string{"Content-Type": "application/openstack-images-v2.1-json-patch"}, b, nil, http.StatusOK); err != nil {
		return plugin.ObjectInfo{}, classify(ctx, "publish "+u.t.Name, err)
	}
	for _, o := range older {
		if o.ID != u.t.ImageID {
			if err := u.c.remove(ctx, o.ID); err != nil {
				return plugin.ObjectInfo{}, err
			}
		}
	}
	info := im.info()
	u.committed = &info
	return info, nil
}

// active waits for the image to be active, or to fail.
func (u *upload) active(ctx context.Context) (image, error) {
	for {
		im, err := u.c.get(ctx, u.t.ImageID)
		if errors.Is(err, plugin.ErrNotFound) {
			return image{}, plugin.Wrapf(plugin.ResumeInvalid, "glance: the image of %s was deleted during the upload", u.t.Name)
		}
		if err != nil {
			return image{}, err
		}
		switch im.Status {
		case "active":
			return im, nil
		case "killed", "deleted", "pending_delete", "deactivated":
			return image{}, u.fail(plugin.Wrapf(plugin.Transient, "glance: the image of %s is %s", u.t.Name, im.Status))
		}
		if im.FailedStore != "" {
			return image{}, u.fail(plugin.Wrapf(plugin.Transient, "glance: the import of %s failed to the stores %s", u.t.Name, im.FailedStore))
		}
		select {
		case <-ctx.Done():
			return image{}, ctx.Err()
		case <-time.After(u.c.poll):
		}
	}
}

// Abort stops the data request and deletes the image.
func (u *upload) Abort(ctx context.Context) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.committed != nil {
		return nil
	}
	if u.s != nil {
		u.s.Abort()
	}
	return u.c.remove(ctx, u.t.ImageID)
}
