package integration

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"idp-workspace/adapters/blob"
	"idp-workspace/adapters/store/postgres"
)

// objectStore is the test's S3-compatible store (make test-integration starts RustFS); nil when there is none.
func objectStore(t *testing.T) *blob.S3 {
	t.Helper()
	ep := os.Getenv("IDP_TEST_S3_ENDPOINT")
	if ep == "" {
		t.Skip("IDP_TEST_S3_ENDPOINT not set (make test-integration starts RustFS)")
	}
	d := t.TempDir()
	ak, sk := filepath.Join(d, "ak"), filepath.Join(d, "sk")
	_ = os.WriteFile(ak, []byte(os.Getenv("IDP_TEST_S3_ACCESS_KEY")), 0o600)
	_ = os.WriteFile(sk, []byte(os.Getenv("IDP_TEST_S3_SECRET_KEY")), 0o600)
	s, err := blob.New(blob.Config{Endpoint: ep, Bucket: os.Getenv("IDP_TEST_S3_BUCKET"), AccessKeyFile: ak, SecretKeyFile: sk, SSE: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s
}

// D24: file contents in the object store. The whole member-files flow runs on it; then what only an object store needs:
// no content in the database, objects encrypted by the store, a tampered object refused, deleted and expired files'
// objects gone, and objects without a row swept.
func TestFilesInTheObjectStore(t *testing.T) {
	s3 := objectStore(t)
	e := newEnv(t, func(st *postgres.Store) { st.Blobs = s3 })
	e.must(201, "POST", "/api/v1/workspaces", "alice", map[string]any{"name": "us-ops", "displayName": "US operations"})
	e.must(200, "PUT", "/api/v1/workspaces/us-ops/members/bob", "alice", map[string]any{"roles": []string{"approver"}})
	e.must(200, "PUT", "/api/v1/workspaces/us-ops/members/carol", "alice", map[string]any{"roles": []string{}})
	memberFiles(t, e)

	ctx := context.Background()
	owner, err := pgx.Connect(ctx, e.ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close(ctx)
	upload := func(name string, data []byte) string {
		st, out, _ := raw(t, "POST", e.api.URL+"/v1/workspaces/us-ops/files?name="+name, e.tokens["bob"], data)
		if st != 201 {
			t.Fatalf("upload %s: %d %s", name, st, out)
		}
		return string(bytes.Split(bytes.SplitN(out, []byte(`"id":"`), 2)[1], []byte(`"`))[0])
	}
	// bob's quota (6 files) is spent by memberFiles; carol uploads here
	e.tokens["bob"] = e.tokens["carol"]
	doc := docxOf(t, "对象存储")
	id := upload("obj.docx", doc)
	key := postgres.ObjectKey(e.tenantA, id)
	var inDB *[]byte
	var objKey *string
	if err := owner.QueryRow(ctx, `SELECT content, object_key FROM member_files WHERE id = $1`, id).Scan(&inDB, &objKey); err != nil {
		t.Fatal(err)
	}
	if inDB != nil || objKey == nil || *objKey != key {
		t.Fatalf("the database holds the content (%v) or names another object (%v)", inDB != nil, objKey)
	}
	if ok, enc, err := s3.Head(ctx, key); err != nil || !ok || !enc {
		t.Fatalf("the object is not there, encrypted: %v %v %v", ok, enc, err)
	}
	st, got, _ := raw(t, "GET", e.api.URL+"/v1/workspaces/us-ops/files/"+id+"/content", e.tokens["carol"], nil)
	if st != 200 || !bytes.Equal(got, doc) {
		t.Fatalf("download from the object store: %d", st)
	}
	// someone with the store's keys changes the object: the workspace refuses to hand it out
	if err := s3.Put(ctx, key, docxOf(t, "篡改")); err != nil {
		t.Fatal(err)
	}
	if st, body, _ := raw(t, "GET", e.api.URL+"/v1/workspaces/us-ops/files/"+id+"/content", e.tokens["carol"], nil); st != 500 || bytes.Contains(body, []byte("篡改")) {
		t.Fatalf("a tampered object was served: %d", st)
	}
	// delete: row and object
	e.must(204, "DELETE", "/v1/workspaces/us-ops/files/"+id, "carol", nil)
	if ok, _, _ := s3.Head(ctx, key); ok {
		t.Fatal("the deleted file's object is still there")
	}
	// expiry: row and object
	exp := upload("exp.txt", []byte("expires"))
	if _, err := owner.Exec(ctx, `UPDATE member_files SET expires_at = now() - interval '1 minute' WHERE id = $1`, exp); err != nil {
		t.Fatal(err)
	}
	keep := upload("keep.txt", []byte("stays"))
	// an object no row names (an upload whose transaction failed), once it is old enough
	stray := postgres.ObjectKey(e.tenantA, postgres.NewUUID())
	if err := s3.Put(ctx, stray, []byte("stray")); err != nil {
		t.Fatal(err)
	}
	if n, err := e.store.PurgeExpiredFiles(ctx); err != nil || n != 1 {
		t.Fatalf("purge: %d %v", n, err)
	}
	if ok, _, _ := s3.Head(ctx, postgres.ObjectKey(e.tenantA, exp)); ok {
		t.Fatal("the expired file's object is still there")
	}
	if ok, _, _ := s3.Head(ctx, stray); !ok {
		t.Fatal("a young object without a row was swept (it may be an upload in progress)")
	}
	old := postgres.OrphanAge
	postgres.OrphanAge = 0
	defer func() { postgres.OrphanAge = old }()
	e.store.Now = func() time.Time { return time.Now().Add(time.Second) }
	if _, err := e.store.PurgeExpiredFiles(ctx); err != nil {
		t.Fatal(err)
	}
	if ok, _, _ := s3.Head(ctx, stray); ok {
		t.Fatal("the stray object was not swept")
	}
	if ok, _, _ := s3.Head(ctx, postgres.ObjectKey(e.tenantA, keep)); !ok {
		t.Fatal("the sweep deleted a file that has its row")
	}
}
