package snapshot

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// s3Fake is a minimal in-memory S3-compatible server for tests.
type s3Fake struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func newS3Fake() *s3Fake {
	return &s3Fake{objects: make(map[string][]byte)}
}

func (f *s3Fake) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()

		// Strip leading slash and split into bucket + key.
		path := strings.TrimPrefix(r.URL.Path, "/")
		parts := strings.SplitN(path, "/", 2)
		bucket := parts[0]
		_ = bucket

		switch r.Method {
		case http.MethodPut:
			if len(parts) < 2 {
				http.Error(w, "missing key", http.StatusBadRequest)
				return
			}
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "read error", http.StatusInternalServerError)
				return
			}
			f.objects[parts[1]] = body
			w.WriteHeader(http.StatusOK)

		case http.MethodGet:
			if r.URL.Query().Get("list-type") == "2" {
				// ListObjectsV2
				prefix := r.URL.Query().Get("prefix")
				type Object struct {
					Key string `xml:"Key"`
				}
				type ListResult struct {
					XMLName  xml.Name `xml:"ListBucketResult"`
					Name     string   `xml:"Name"`
					Contents []Object `xml:"Contents"`
				}
				var result ListResult
				result.Name = bucket
				for k := range f.objects {
					key := k
					if prefix != "" && !strings.HasPrefix(key, prefix) {
						continue
					}
					// S3 List returns keys without the bucket name.
					result.Contents = append(result.Contents, Object{Key: key})
				}
				w.Header().Set("Content-Type", "application/xml")
				_ = xml.NewEncoder(w).Encode(result)
				return
			}
			if len(parts) < 2 {
				http.Error(w, "missing key", http.StatusBadRequest)
				return
			}
			data, ok := f.objects[parts[1]]
			if !ok {
				// Return S3-style NoSuchKey XML.
				w.Header().Set("Content-Type", "application/xml")
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>NoSuchKey</Code><Key>%s</Key></Error>`, parts[1])
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(data)

		case http.MethodDelete:
			if len(parts) < 2 {
				http.Error(w, "missing key", http.StatusBadRequest)
				return
			}
			delete(f.objects, parts[1])
			w.WriteHeader(http.StatusNoContent)

		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	return mux
}

func newTestS3Store(t *testing.T, fake *s3Fake) (*S3Store, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)

	store, err := NewS3Store(context.Background(), S3Config{
		Endpoint:        srv.URL,
		Bucket:          "test-bucket",
		Region:          "us-east-1",
		Prefix:          "snapshots",
		AccessKeyID:     "test",
		SecretAccessKey: "test",
	})
	if err != nil {
		t.Fatalf("NewS3Store: %v", err)
	}
	return store, srv
}

func TestS3Store_PutAndGet(t *testing.T) {
	fake := newS3Fake()
	store, _ := newTestS3Store(t, fake)

	body := []byte(`{"snapshot_version":1,"sdk_key_hash":"abc","etag":"v1","captured_at":"2026-01-01T00:00:00Z","payload":"dGVzdA=="}`)
	if err := store.Put(context.Background(), "abc", body); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := store.Get(context.Background(), "abc")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("body mismatch: got %q, want %q", got, body)
	}
}

func TestS3Store_List(t *testing.T) {
	fake := newS3Fake()
	store, _ := newTestS3Store(t, fake)

	if err := store.Put(context.Background(), "key1", []byte(`{}`)); err != nil {
		t.Fatalf("Put key1: %v", err)
	}
	if err := store.Put(context.Background(), "key2", []byte(`{}`)); err != nil {
		t.Fatalf("Put key2: %v", err)
	}

	keys, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	found := map[string]bool{}
	for _, k := range keys {
		found[k] = true
	}
	if !found["key1"] || !found["key2"] {
		t.Errorf("expected key1 and key2 in list, got %v", keys)
	}
}

func TestS3Store_GetMissing(t *testing.T) {
	fake := newS3Fake()
	store, _ := newTestS3Store(t, fake)

	_, err := store.Get(context.Background(), "nonexistent")
	if err == nil {
		t.Error("expected error for missing key, got nil")
	}
}

func TestS3Store_ListEmpty(t *testing.T) {
	fake := newS3Fake()
	store, _ := newTestS3Store(t, fake)

	keys, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(keys) != 0 {
		t.Errorf("expected empty list, got %v", keys)
	}
}

func TestS3Store_Delete_RemovesObject(t *testing.T) {
	fake := newS3Fake()
	store, _ := newTestS3Store(t, fake)

	if err := store.Put(context.Background(), "key1", []byte(`{}`)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := store.Delete(context.Background(), "key1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, err := store.Get(context.Background(), "key1")
	if err == nil {
		t.Error("expected error after Delete, got nil")
	}
}

func TestS3Store_Delete_MissingKeyReturnsNil(t *testing.T) {
	fake := newS3Fake()
	store, _ := newTestS3Store(t, fake)

	if err := store.Delete(context.Background(), "nonexistent"); err != nil {
		t.Errorf("Delete on missing key: expected nil, got %v", err)
	}
}

func TestExtractKeyHash(t *testing.T) {
	cases := []struct {
		key    string
		prefix string
		want   string
	}{
		{"snapshots/abc123.json", "snapshots/", "abc123"},
		{"abc123.json", "", "abc123"},
		{"snapshots/nested/key.json", "snapshots/", ""},
		{"snapshots/", "snapshots/", ""},
	}
	for _, tc := range cases {
		got := extractKeyHash(tc.key, tc.prefix)
		if got != tc.want {
			t.Errorf("extractKeyHash(%q, %q) = %q, want %q", tc.key, tc.prefix, got, tc.want)
		}
	}
}
