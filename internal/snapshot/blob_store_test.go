package snapshot

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

const (
	hashA = "dt01.server_us_ab3xy9kp2q"
	hashB = "dt01.server_eu_0000000001"
	hashC = "dt01.server_ap_0000000002"
	hashD = "dt01.server_oc_00000000aa"
)

func fileURL(dir string) string {
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(dir)}).String()
}

func newFileStore(t *testing.T, dir string) *BlobStore {
	t.Helper()
	store, err := OpenBlobStore(context.Background(), fileURL(dir))
	if err != nil {
		t.Fatalf("OpenBlobStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func storeBackends(t *testing.T) map[string]func(t *testing.T) *BlobStore {
	return map[string]func(t *testing.T) *BlobStore{
		"file": func(t *testing.T) *BlobStore { return newFileStore(t, t.TempDir()) },
		"s3": func(t *testing.T) *BlobStore {
			store, _ := newTestS3Store(t, newS3Fake())
			return store
		},
	}
}

func TestBlobStore_PutAndGet(t *testing.T) {
	for name, open := range storeBackends(t) {
		t.Run(name, func(t *testing.T) {
			store := open(t)
			body := []byte(`{"snapshot_version":1,"sdk_key_id":"dt01.server_us_ab3xy9kp2q","etag":"v1","captured_at":"2026-01-01T00:00:00Z","payload":"dGVzdA=="}`)
			if err := store.PutIfMatch(context.Background(), hashA, body, ""); err != nil {
				t.Fatalf("PutIfMatch: %v", err)
			}
			got, err := store.Get(context.Background(), hashA)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if !bytes.Equal(got, body) {
				t.Errorf("body mismatch: got %q, want %q", got, body)
			}
		})
	}
}

func TestBlobStore_List(t *testing.T) {
	for name, open := range storeBackends(t) {
		t.Run(name, func(t *testing.T) {
			store := open(t)
			for _, k := range []string{hashB, hashC} {
				if err := store.PutIfMatch(context.Background(), k, []byte(`{}`), ""); err != nil {
					t.Fatalf("Put %s: %v", k, err)
				}
			}
			keys, err := store.List(context.Background())
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			found := map[string]bool{}
			for _, k := range keys {
				found[k] = true
			}
			if len(keys) != 2 || !found[hashB] || !found[hashC] {
				t.Errorf("expected [%s %s], got %v", hashB, hashC, keys)
			}
		})
	}
}

func TestBlobStore_ListEmpty(t *testing.T) {
	for name, open := range storeBackends(t) {
		t.Run(name, func(t *testing.T) {
			keys, err := open(t).List(context.Background())
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if len(keys) != 0 {
				t.Errorf("expected empty list, got %v", keys)
			}
		})
	}
}

func TestBlobStore_GetMissing(t *testing.T) {
	for name, open := range storeBackends(t) {
		t.Run(name, func(t *testing.T) {
			if _, err := open(t).Get(context.Background(), hashA); err == nil {
				t.Error("expected error for missing key, got nil")
			}
		})
	}
}

func TestBlobStore_Delete_RemovesObject(t *testing.T) {
	for name, open := range storeBackends(t) {
		t.Run(name, func(t *testing.T) {
			store := open(t)
			if err := store.PutIfMatch(context.Background(), hashA, []byte(`{}`), ""); err != nil {
				t.Fatalf("PutIfMatch: %v", err)
			}
			if err := store.Delete(context.Background(), hashA); err != nil {
				t.Fatalf("Delete: %v", err)
			}
			if _, err := store.Get(context.Background(), hashA); err == nil {
				t.Error("expected error after Delete, got nil")
			}
		})
	}
}

func TestBlobStore_Delete_MissingKeyReturnsNil(t *testing.T) {
	for name, open := range storeBackends(t) {
		t.Run(name, func(t *testing.T) {
			if err := open(t).Delete(context.Background(), hashA); err != nil {
				t.Errorf("Delete on missing key: expected nil, got %v", err)
			}
		})
	}
}

func TestBlobStore_GetVersioned_MissingIsErrNotFound(t *testing.T) {
	for name, open := range storeBackends(t) {
		t.Run(name, func(t *testing.T) {
			if _, _, err := open(t).GetVersioned(context.Background(), hashA); !errors.Is(err, ErrNotFound) {
				t.Errorf("expected ErrNotFound, got %v", err)
			}
		})
	}
}

func TestBlobStore_PutIfMatch(t *testing.T) {
	for name, open := range storeBackends(t) {
		t.Run(name, func(t *testing.T) {
			store := open(t)
			ctx := context.Background()

			// Create-only succeeds once.
			if err := store.PutIfMatch(ctx, hashA, []byte(`v1`), ""); err != nil {
				t.Fatalf("create: %v", err)
			}
			if err := store.PutIfMatch(ctx, hashA, []byte(`dup`), ""); !errors.Is(err, ErrPreconditionFailed) {
				t.Fatalf("second create: expected ErrPreconditionFailed, got %v", err)
			}

			_, v1, err := store.GetVersioned(ctx, hashA)
			if err != nil || v1 == "" {
				t.Fatalf("GetVersioned: version %q, err %v", v1, err)
			}
			if err := store.PutIfMatch(ctx, hashA, []byte(`v2`), v1); err != nil {
				t.Fatalf("update at current version: %v", err)
			}
			// v1 is now stale: a writer that read before the update must lose.
			if err := store.PutIfMatch(ctx, hashA, []byte(`stale`), v1); !errors.Is(err, ErrPreconditionFailed) {
				t.Fatalf("update at stale version: expected ErrPreconditionFailed, got %v", err)
			}

			got, v2, err := store.GetVersioned(ctx, hashA)
			if err != nil {
				t.Fatalf("GetVersioned: %v", err)
			}
			if string(got) != "v2" {
				t.Errorf("body = %q, want v2", got)
			}
			if v2 == v1 {
				t.Errorf("version did not change after write: %q", v2)
			}
		})
	}
}

func TestBlobStore_InvalidKeyID(t *testing.T) {
	store := newFileStore(t, t.TempDir())
	for _, bad := range []string{"abc", "../escape", "dt01.invalid_us_ab3xy9kp2q", "dt01.server_US_ab3xy9kp2q"} {
		if err := store.PutIfMatch(context.Background(), bad, []byte(`{}`), ""); err == nil {
			t.Errorf("PutIfMatch(%q): expected error, got nil", bad)
		}
		if _, err := store.Get(context.Background(), bad); err == nil {
			t.Errorf("Get(%q): expected error, got nil", bad)
		}
		if err := store.Delete(context.Background(), bad); err == nil {
			t.Errorf("Delete(%q): expected error, got nil", bad)
		}
	}
}

func TestBlobStore_File_ListSkipsNonSnapshotFiles(t *testing.T) {
	dir := t.TempDir()
	store := newFileStore(t, dir)
	if err := store.PutIfMatch(context.Background(), hashD, []byte(`{}`), ""); err != nil {
		t.Fatalf("PutIfMatch: %v", err)
	}
	// Ignored: stray temp file, directory, nested object, invalid key ID.
	if err := os.WriteFile(filepath.Join(dir, hashD+".json.18a2b3c4.tmp"), []byte(`{}`), 0o600); err != nil {
		t.Fatalf("write tmp: %v", err)
	}
	if err := os.Mkdir(filepath.Join(dir, "subdir.json"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "subdir.json", hashA+".json"), []byte(`{}`), 0o600); err != nil {
		t.Fatalf("write nested: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatalf("write README.json: %v", err)
	}
	keys, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(keys) != 1 || keys[0] != hashD {
		t.Errorf("expected [%s], got %v", hashD, keys)
	}
}

func TestBlobStore_File_CreatesDirIfMissing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "snapshots")
	store := newFileStore(t, dir)
	if err := store.PutIfMatch(context.Background(), hashB, []byte(`{}`), ""); err != nil {
		t.Fatalf("Put after auto-create: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o700 {
		t.Errorf("dir mode = %04o, want 0700", mode)
	}
}

func TestBlobStore_File_NoSidecarOrTempFiles(t *testing.T) {
	dir := t.TempDir()
	store := newFileStore(t, dir)
	if err := store.PutIfMatch(context.Background(), hashA, []byte(`{}`), ""); err != nil {
		t.Fatalf("PutIfMatch: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != hashA+".json" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("expected only %s.json on disk, got %v", hashA, names)
	}
}

func TestBlobStore_File_Prefix(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenBlobStore(context.Background(), fileURL(dir)+"?prefix=snapshots")
	if err != nil {
		t.Fatalf("OpenBlobStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.PutIfMatch(context.Background(), hashA, []byte(`{}`), ""); err != nil {
		t.Fatalf("PutIfMatch: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "snapshots", hashA+".json")); err != nil {
		t.Errorf("expected object under prefix dir: %v", err)
	}
	keys, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(keys) != 1 || keys[0] != hashA {
		t.Errorf("expected [%s], got %v", hashA, keys)
	}
}

func TestBlobStore_File_ConcurrentPuts(t *testing.T) {
	store := newFileStore(t, t.TempDir())
	const n = 20
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("dt01.server_us_%010d", i)
			if err := store.PutIfMatch(context.Background(), key, []byte(`{}`), ""); err != nil {
				t.Errorf("PutIfMatch %s: %v", key, err)
			}
		}(i)
	}
	wg.Wait()
	keys, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(keys) != n {
		t.Errorf("expected %d keys, got %d", n, len(keys))
	}
}

func TestBlobStore_S3_WritesUnderPrefix(t *testing.T) {
	fake := newS3Fake()
	store, _ := newTestS3Store(t, fake)
	if err := store.PutIfMatch(context.Background(), hashA, []byte(`{}`), ""); err != nil {
		t.Fatalf("PutIfMatch: %v", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if _, ok := fake.objects["snapshots/"+hashA+".json"]; !ok {
		t.Errorf("expected object at snapshots/%s.json, got keys %v", hashA, fake.keys())
	}
}

func TestOpenBlobStore_UnsupportedScheme(t *testing.T) {
	for _, raw := range []string{"ftp://bucket", "mem://", "bucket-without-scheme", "http://example.com/bucket"} {
		_, err := OpenBlobStore(context.Background(), raw)
		if err == nil {
			t.Errorf("OpenBlobStore(%q): expected error, got nil", raw)
			continue
		}
		if !strings.Contains(err.Error(), "unsupported URL scheme") {
			t.Errorf("OpenBlobStore(%q): error %q should name the unsupported scheme", raw, err)
		}
	}
}

func TestNormalizeBlobURL(t *testing.T) {
	cases := []struct {
		in   string
		want url.Values
	}{
		{"s3://b?region=us-east-1", url.Values{"region": {"us-east-1"}}},
		{"s3://b?prefix=snaps", url.Values{"prefix": {"snaps/"}}},
		{"gs://b?prefix=snaps/", url.Values{"prefix": {"snaps/"}}},
		{"file:///data", url.Values{
			"create_dir": {"true"}, "dir_file_mode": {"448"}, "no_tmp_dir": {"true"}, "metadata": {"skip"},
		}},
		{"file:///data?dir_file_mode=488", url.Values{
			"create_dir": {"true"}, "dir_file_mode": {"488"}, "no_tmp_dir": {"true"}, "metadata": {"skip"},
		}},
	}
	for _, tc := range cases {
		got, err := normalizeBlobURL(tc.in)
		if err != nil {
			t.Fatalf("normalizeBlobURL(%q): %v", tc.in, err)
		}
		u, _ := url.Parse(got)
		if u.Query().Encode() != tc.want.Encode() {
			t.Errorf("normalizeBlobURL(%q) query = %q, want %q", tc.in, u.Query().Encode(), tc.want.Encode())
		}
	}
}

func TestExtractPublicID(t *testing.T) {
	const validID = "dt01.server_us_ab3xy9kp2q"
	cases := []struct {
		key  string
		want string
	}{
		{validID + ".json", validID},
		{"nested/" + validID + ".json", ""}, // nested slash rejected
		{".json", ""},                       // empty name
		{validID, ""},                       // missing .json suffix
		{validID + ".json.1a2b.tmp", ""},    // temp file
		{"abc123.json", ""},                 // old-format hash rejected
	}
	for _, tc := range cases {
		if got := extractPublicID(tc.key); got != tc.want {
			t.Errorf("extractPublicID(%q) = %q, want %q", tc.key, got, tc.want)
		}
	}
}

// s3Fake is a minimal in-memory S3-compatible server for tests, including
// conditional PUTs (If-Match / If-None-Match: *).
type s3Fake struct {
	mu      sync.Mutex
	objects map[string][]byte
	etags   map[string]string
	writes  int
}

func newS3Fake() *s3Fake {
	return &s3Fake{objects: make(map[string][]byte), etags: make(map[string]string)}
}

func preconditionFailed(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusPreconditionFailed)
	fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>PreconditionFailed</Code></Error>`)
}

// keys requires f.mu.
func (f *s3Fake) keys() []string {
	var ks []string
	for k := range f.objects {
		ks = append(ks, k)
	}
	return ks
}

func (f *s3Fake) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()

		// Path-style addressing: /<bucket>/<key>.
		bucket, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")

		if key == "" {
			if r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2" {
				f.list(w, bucket, r.URL.Query().Get("prefix"))
				return
			}
			http.Error(w, "missing key", http.StatusBadRequest)
			return
		}

		switch r.Method {
		case http.MethodPut:
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "read error", http.StatusInternalServerError)
				return
			}
			cur, exists := f.etags[key]
			if r.Header.Get("If-None-Match") == "*" && exists {
				preconditionFailed(w)
				return
			}
			if m := r.Header.Get("If-Match"); m != "" && m != cur {
				preconditionFailed(w)
				return
			}
			f.writes++
			f.objects[key] = body
			f.etags[key] = fmt.Sprintf(`"%d"`, f.writes)
			w.Header().Set("ETag", f.etags[key])
			w.WriteHeader(http.StatusOK)

		case http.MethodHead:
			data, ok := f.objects[key]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			w.Header().Set("ETag", f.etags[key])
			w.WriteHeader(http.StatusOK)

		case http.MethodGet:
			data, ok := f.objects[key]
			if !ok {
				w.Header().Set("Content-Type", "application/xml")
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>NoSuchKey</Code><Key>%s</Key></Error>`, key)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			w.Header().Set("ETag", f.etags[key])
			_, _ = w.Write(data)

		case http.MethodDelete:
			delete(f.objects, key)
			delete(f.etags, key)
			w.WriteHeader(http.StatusNoContent)

		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
}

// list serves ListObjectsV2, ignoring the delimiter.
func (f *s3Fake) list(w http.ResponseWriter, bucket, prefix string) {
	type object struct {
		Key          string `xml:"Key"`
		Size         int    `xml:"Size"`
		LastModified string `xml:"LastModified"`
	}
	type listResult struct {
		XMLName  xml.Name `xml:"ListBucketResult"`
		Name     string   `xml:"Name"`
		Prefix   string   `xml:"Prefix"`
		Contents []object `xml:"Contents"`
	}
	result := listResult{Name: bucket, Prefix: prefix}
	for k, v := range f.objects {
		if strings.HasPrefix(k, prefix) {
			result.Contents = append(result.Contents, object{Key: k, Size: len(v), LastModified: "2026-01-01T00:00:00.000Z"})
		}
	}
	w.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(w).Encode(result)
}

func newTestS3Store(t *testing.T, fake *s3Fake) (*BlobStore, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)

	// Isolate from any host AWS config.
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_SESSION_TOKEN", "")
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "none"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "none"))
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")

	q := url.Values{
		"endpoint":       {srv.URL},
		"region":         {"us-east-1"},
		"use_path_style": {"true"},
		"prefix":         {"snapshots"},
	}
	store, err := OpenBlobStore(context.Background(), "s3://test-bucket?"+q.Encode())
	if err != nil {
		t.Fatalf("OpenBlobStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, srv
}
