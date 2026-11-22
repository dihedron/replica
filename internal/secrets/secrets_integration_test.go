//go:build integration

package secrets

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/dihedron/veho/internal/audit"
	"github.com/dihedron/veho/internal/store"
	"github.com/dihedron/veho/internal/store/migrate"
	"github.com/dihedron/veho/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func dsn(t *testing.T, env string) string {
	t.Helper()
	v := os.Getenv(env)
	if v == "" {
		t.Skipf("%s is not set: run with test/lab/lab.sh test", env)
	}
	return v
}

// fixture: the KEKs reset under an unwrap key, two tenants, the pools.
type fixture struct {
	migrator    *pgx.Conn
	api, worker *pgxpool.Pool
	src         FileSource
	t1, t2      uuid.UUID
	audit       *audit.Recorder
}

// keyFile writes a new unwrap key.
func keyFile(t *testing.T) FileSource {
	t.Helper()
	p := filepath.Join(t.TempDir(), "unwrap.key")
	if err := os.WriteFile(p, newKey(), 0o600); err != nil {
		t.Fatal(err)
	}
	return FileSource{p}
}

// setup migrates, takes the lock that serialises the tests of veho.kek (in
// every package), and empties the secrets and the KEKs.
func setup(t *testing.T) fixture {
	t.Helper()
	ctx := context.Background()
	conn, err := store.Connect(ctx, dsn(t, "VEHO_TEST_DATABASE_URL"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(ctx) })
	if _, err := migrate.Apply(ctx, conn, migrations.FS); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, "select pg_advisory_lock(hashtext('veho-test-kek'))"); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"update veho.endpoints set credential_ref = null where credential_ref like 'db://%'",
		"delete from veho.secrets",
		"delete from veho.kek",
	} {
		if _, err := conn.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	f := fixture{migrator: conn, src: keyFile(t), audit: &audit.Recorder{}}
	for env, p := range map[string]**pgxpool.Pool{"VEHO_TEST_API_DATABASE_URL": &f.api, "VEHO_TEST_WORKER_DATABASE_URL": &f.worker} {
		if *p, err = store.Open(ctx, dsn(t, env), store.Options{Name: "test/secrets"}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup((*p).Close)
	}
	for _, id := range []*uuid.UUID{&f.t1, &f.t2} {
		if err := f.api.QueryRow(ctx, "select veho.create_tenant($1)", "test-"+uuid.New().String()).Scan(id); err != nil {
			t.Fatal(err)
		}
	}
	if err := InitKEK(ctx, conn, f.src); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f fixture) store(t *testing.T) *Store {
	t.Helper()
	ring, err := LoadKeyring(context.Background(), f.worker, f.src)
	if err != nil {
		t.Fatal(err)
	}
	return NewStore(ring, f.api, f.worker, f.audit)
}

func fetch(t *testing.T, s *Store, tenant uuid.UUID, name string) string {
	t.Helper()
	v, err := s.Fetch(context.Background(), tenant, RefPrefix+name)
	if err != nil {
		t.Fatalf("fetch %s: %v", name, err)
	}
	defer v.Zero()
	return string(v.Bytes())
}

// SECR-1, SECR-3, SECR-8 on the database; SECR-2: no row holds the value,
// a DEK or a KEK.
func TestPutFetch(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	s := f.store(t)
	if err := InitKEK(ctx, f.migrator, f.src); !errors.Is(err, ErrInitialized) {
		t.Errorf("second init: %v", err)
	}
	m, err := s.Put(ctx, f.t1, "s3", []byte("canary-one"))
	if err != nil || m.Version != 1 || m.KEKVersion != 1 {
		t.Fatalf("put: %+v, %v", m, err)
	}
	if m, err = s.Put(ctx, f.t1, "s3", []byte("canary-two")); err != nil || m.Version != 2 {
		t.Fatalf("replace: %+v, %v", m, err)
	}
	if got := fetch(t, s, f.t1, "s3"); got != "canary-two" {
		t.Errorf("fetch: %q", got)
	}
	if list, err := s.List(ctx, f.t1); err != nil || len(list) != 1 || list[0].Name != "s3" || list[0].Version != 2 {
		t.Errorf("list: %+v, %v", list, err)
	}
	for _, bad := range []struct {
		name  string
		value []byte
	}{{"S3", []byte("x")}, {"", []byte("x")}, {"s3", nil}, {"s3", make([]byte, MaxValueSize+1)}} {
		if _, err := s.Put(ctx, f.t1, bad.name, bad.value); !errors.Is(err, ErrInvalid) {
			t.Errorf("put %q (%d bytes): %v", bad.name, len(bad.value), err)
		}
	}
	if _, err := s.Fetch(ctx, f.t1, "db://none"); !errors.Is(err, ErrUnavailable) || !errors.Is(err, ErrNotFound) {
		t.Errorf("fetch a missing secret: %v", err)
	}
	if _, err := s.Show(ctx, f.t1, "none"); !errors.Is(err, ErrNotFound) {
		t.Errorf("show a missing secret: %v", err)
	}

	// AUDL-3: the puts are audited with their versions, never their values
	ev := f.audit.Events()
	if len(ev) != 2 || ev[0].Action != "SECRET_PUT" || ev[0].Outcome != audit.Success || ev[0].TenantID != f.t1.String() ||
		ev[0].ResourceName != "s3" || ev[1].Details.(map[string]int)["version"] != 2 {
		t.Errorf("events: %+v", ev)
	}
	if j, _ := json.Marshal(ev); strings.Contains(string(j), "canary") {
		t.Error("an event carries the value")
	}

	// SECR-2
	var dump strings.Builder
	for _, table := range []string{"veho.secrets", "veho.kek"} {
		rows, err := f.migrator.Query(ctx, "select t::text from "+table+" t")
		if err != nil {
			t.Fatal(err)
		}
		texts, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		dump.WriteString(strings.Join(texts, "\n"))
	}
	ring := s.kek.(*Keyring)
	for what, needle := range map[string]string{"value": hex.EncodeToString([]byte("canary")), "KEK": hex.EncodeToString(ring.kek[1])} {
		if strings.Contains(dump.String(), needle) {
			t.Errorf("the %s shows in the tables", what)
		}
	}

	// SECR-8: the database unreachable
	dead, err := store.Open(ctx, dsn(t, "VEHO_TEST_WORKER_DATABASE_URL"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	dead.Close()
	if _, err := NewStore(ring, nil, dead, f.audit).Fetch(ctx, f.t1, "db://s3"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("database closed: %v", err)
	}
}

// SECR-4: a tenant sees none of another's secrets; SECR-1 on rows: a row
// copied to another tenant does not decrypt.
func TestIsolation(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	s := f.store(t)
	if _, err := s.Put(ctx, f.t1, "s3", []byte("one")); err != nil {
		t.Fatal(err)
	}
	if list, err := s.List(ctx, f.t2); err != nil || len(list) != 0 {
		t.Errorf("t2 lists %+v, %v", list, err)
	}
	if _, err := s.Show(ctx, f.t2, "s3"); !errors.Is(err, ErrNotFound) {
		t.Errorf("t2 shows t1's secret: %v", err)
	}
	if err := s.Delete(ctx, f.t2, "s3"); !errors.Is(err, ErrNotFound) {
		t.Errorf("t2 deletes t1's secret: %v", err)
	}
	if _, err := s.Fetch(ctx, f.t2, "db://s3"); !errors.Is(err, ErrNotFound) {
		t.Errorf("t2 fetches t1's secret: %v", err)
	}
	if _, err := f.migrator.Exec(ctx, `insert into veho.secrets (tenant_id, name, version, kek_version, wrapped_dek, ciphertext)
		select $1, name, version, kek_version, wrapped_dek, ciphertext from veho.secrets where tenant_id = $2`, f.t2, f.t1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Fetch(ctx, f.t2, "db://s3"); !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrNotFound) {
		t.Errorf("a row copied to t2: %v", err)
	}
	if _, err := s.Put(ctx, f.t2, "s3", []byte("two")); err != nil {
		t.Fatal(err)
	}
	if fetch(t, s, f.t1, "s3") != "one" || fetch(t, s, f.t2, "s3") != "two" {
		t.Error("the tenants' values mix")
	}
}

// SECR-10: a secret an endpoint references cannot be deleted.
func TestDeleteInUse(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	s := f.store(t)
	if _, err := s.Put(ctx, f.t1, "used", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.api.Exec(ctx, "select veho.put_location('lab-a', 'region', null)"); err != nil {
		t.Fatal(err)
	}
	if err := store.InTenant(ctx, f.api, f.t1, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "insert into veho.endpoints (tenant_id, name, location_id, driver, credential_ref) values ($1, 'ep', 'lab-a', 's3', 'db://used')", f.t1)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, f.t1, "used"); !errors.Is(err, ErrInUse) {
		t.Errorf("delete a used secret: %v", err)
	}
	if _, err := s.Put(ctx, f.t1, "free", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, f.t1, "free"); err != nil {
		t.Errorf("delete: %v", err)
	}
	var deletes []string
	for _, e := range f.audit.Events() {
		if e.Action == "SECRET_DELETE" {
			deletes = append(deletes, e.ResourceName+":"+e.Outcome)
		}
	}
	if strings.Join(deletes, " ") != "used:failed free:success" {
		t.Errorf("delete events: %v", deletes)
	}
}

// SECR-5, SECR-12: rotate, the re-wrap, a node that started before the
// rotation.
func TestRotate(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	before := f.store(t) // a node that started before the rotation
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		if _, err := before.Put(ctx, f.t1, name, []byte("old-"+name)); err != nil {
			t.Fatal(err)
		}
	}
	v, err := RotateKEK(ctx, f.migrator, f.src)
	if err != nil || v != 2 {
		t.Fatalf("rotate: %d, %v", v, err)
	}
	m, err := before.Put(ctx, f.t1, "new", []byte("new"))
	if err != nil || m.KEKVersion != 2 {
		t.Fatalf("put after the rotation: %+v, %v", m, err)
	}
	if fetch(t, before, f.t1, "a") != "old-a" || fetch(t, f.store(t), f.t1, "new") != "new" {
		t.Error("fetch across the rotation")
	}
	// an interrupted re-wrap: a row that does not unwrap, last in the
	// order, stops it after the batches before it committed
	if _, err := f.migrator.Exec(ctx, "update veho.secrets set wrapped_dek = '\\x00' where tenant_id = $1 and name = 'e'", f.t1); err != nil {
		t.Fatal(err)
	}
	if _, err := RewrapDEKs(ctx, f.migrator, f.src, 2); err == nil {
		t.Fatal("rewrap of a corrupt row: no error")
	}
	if _, err := f.migrator.Exec(ctx, "delete from veho.secrets where tenant_id = $1 and name = 'e'", f.t1); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := f.migrator.QueryRow(ctx, "select count(*) from veho.secrets where kek_version = 1").Scan(&left); err != nil || left != 0 {
		t.Fatalf("%d DEKs left under version 1 by the batches before the corrupt row (%v)", left, err)
	}
	res, err := RewrapDEKs(ctx, f.migrator, f.src, 2)
	if err != nil || res.Rewrapped != 0 || len(res.Retired) != 1 || res.Retired[0] != 1 {
		t.Fatalf("rewrap resumed: %+v, %v", res, err)
	}
	if res, err = RewrapDEKs(ctx, f.migrator, f.src, 2); err != nil || res.Rewrapped != 0 || len(res.Retired) != 0 {
		t.Errorf("rewrap again: %+v, %v", res, err)
	}
	info, err := ShowKEKs(ctx, f.migrator)
	if err != nil || len(info) != 2 || info[0].State != "retired" || info[0].Secrets != 0 || info[1].State != "active" || info[1].Secrets != 5 {
		t.Errorf("show: %+v, %v", info, err)
	}
	// the node that holds version 1 in memory still reads the re-wrapped rows
	for _, name := range []string{"a", "b", "c", "d"} {
		if got := fetch(t, before, f.t1, name); got != "old-"+name {
			t.Errorf("%s: %q", name, got)
		}
	}
	// a new node loads only the active version
	if fresh := f.store(t); fetch(t, fresh, f.t1, "d") != "old-d" {
		t.Error("a fresh node")
	}
}

// SECR-5: a secret replaced during the re-wrap is left as written.
func TestRewrapSkipsReplaced(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	s := f.store(t)
	if _, err := s.Put(ctx, f.t1, "a", []byte("one")); err != nil {
		t.Fatal(err)
	}
	if _, err := RotateKEK(ctx, f.migrator, f.src); err != nil {
		t.Fatal(err)
	}
	// the re-wrap's conditional update, against a row replaced meanwhile
	var d struct {
		version, kek int
		wrapped      []byte
	}
	if err := f.migrator.QueryRow(ctx, "select version, kek_version, wrapped_dek from veho.secrets where tenant_id = $1", f.t1).Scan(&d.version, &d.kek, &d.wrapped); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(ctx, f.t1, "a", []byte("two")); err != nil {
		t.Fatal(err)
	}
	tag, err := f.migrator.Exec(ctx, `update veho.secrets set kek_version = 2, wrapped_dek = $1
		where tenant_id = $2 and name = 'a' and version = $3 and kek_version = $4`, d.wrapped, f.t1, d.version, d.kek)
	if err != nil || tag.RowsAffected() != 0 {
		t.Fatalf("the conditional update touched the replaced row: %v, %v", tag, err)
	}
	if res, err := RewrapDEKs(ctx, f.migrator, f.src, 10); err != nil || res.Rewrapped != 0 || len(res.Retired) != 1 {
		t.Errorf("rewrap: %+v, %v", res, err)
	}
	if fetch(t, s, f.t1, "a") != "two" {
		t.Error("the replaced value")
	}
}

// SECR-11: of two concurrent rotates, one fails; one active KEK remains.
func TestConcurrentRotate(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	other, err := store.Connect(ctx, dsn(t, "VEHO_TEST_DATABASE_URL"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close(ctx) }()
	// hold the first rotate open until the second has started
	tx, err := f.migrator.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "update veho.kek set state = 'retiring' where state = 'active'"); err != nil {
		t.Fatal(err)
	}
	if err := insertKEKForTest(ctx, tx, f.src, 2); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var second error
	wg.Go(func() { _, second = RotateKEK(ctx, other, f.src) })
	waitBlocked(t, f.migrator, other.PgConn().PID())
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if pgErr, ok := errors.AsType[*pgconn.PgError](second); !ok || pgErr.Code != "23505" {
		t.Errorf("second rotate: %v, want a unique violation", second)
	}
	var active int
	if err := f.migrator.QueryRow(ctx, "select count(*) from veho.kek where state = 'active'").Scan(&active); err != nil || active != 1 {
		t.Errorf("%d active KEKs, %v", active, err)
	}
}

// waitBlocked waits until the backend pid waits for a lock.
func waitBlocked(t *testing.T, conn *pgx.Conn, pid uint32) {
	t.Helper()
	for range 100 {
		var n int
		if err := conn.QueryRow(context.Background(), "select count(*) from pg_locks where pid = $1 and not granted", pid).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the second rotate never waited for the first")
}

func insertKEKForTest(ctx context.Context, tx pgx.Tx, src UnwrapKeySource, v int) error {
	key, err := src.Load(ctx)
	if err != nil {
		return err
	}
	defer clear(key)
	return insertKEK(ctx, tx, key, v)
}

// SECR-6, SECR-7: rekey; the old key no longer loads, the new one does.
func TestRekey(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	s := f.store(t)
	if _, err := s.Put(ctx, f.t1, "a", []byte("value")); err != nil {
		t.Fatal(err)
	}
	if _, err := RotateKEK(ctx, f.migrator, f.src); err != nil {
		t.Fatal(err)
	}
	next := keyFile(t)
	if err := RekeyKEKs(ctx, f.migrator, f.src, f.src); err == nil {
		t.Error("rekey to the same key: no error")
	}
	if err := RekeyKEKs(ctx, f.migrator, next, f.src); err == nil {
		t.Error("rekey with the wrong current key: no error")
	}
	if err := RekeyKEKs(ctx, f.migrator, f.src, next); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKeyring(ctx, f.worker, f.src); err == nil || !strings.Contains(err.Error(), "the key loaded is") {
		t.Errorf("the old key loads: %v", err)
	}
	if _, err := RotateKEK(ctx, f.migrator, f.src); err == nil {
		t.Error("rotate with the old key: no error")
	}
	ring, err := LoadKeyring(ctx, f.worker, next)
	if err != nil {
		t.Fatal(err)
	}
	if got := fetch(t, NewStore(ring, f.api, f.worker, f.audit), f.t1, "a"); got != "value" {
		t.Errorf("after rekey: %q", got)
	}
	// SECR-7: no active KEK
	if _, err := f.migrator.Exec(ctx, "update veho.kek set state = 'retiring'"); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKeyring(ctx, f.worker, next); err == nil {
		t.Error("no active KEK: loads")
	}
	if _, err := s.Put(ctx, f.t1, "b", []byte("x")); err == nil {
		t.Error("put without an active KEK: no error")
	}
}
