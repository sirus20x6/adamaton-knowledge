package upload

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/sirus20x6/adamaton-core/blobstore"
)

// memStore is an in-memory blobstore.Backend for tests.
type memStore struct {
	objects map[string][]byte
}

func newMemStore() *memStore { return &memStore{objects: map[string][]byte{}} }

func (m *memStore) EnsureBucket(ctx context.Context) error { return nil }

func (m *memStore) Put(ctx context.Context, key string, r io.Reader, size int64) (blobstore.ObjectRef, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return blobstore.ObjectRef{}, err
	}
	m.objects[key] = b
	return blobstore.ObjectRef{Bucket: "dr-uploads", Key: key, Size: int64(len(b))}, nil
}

func (m *memStore) PutMultipart(ctx context.Context, key string, r io.Reader) (blobstore.ObjectRef, error) {
	return m.Put(ctx, key, r, -1)
}

func (m *memStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	b, ok := m.objects[key]
	if !ok {
		return nil, blobstore.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (m *memStore) Stat(ctx context.Context, key string) (blobstore.ObjectRef, error) {
	b, ok := m.objects[key]
	if !ok {
		return blobstore.ObjectRef{}, blobstore.ErrNotFound
	}
	return blobstore.ObjectRef{Bucket: "dr-uploads", Key: key, Size: int64(len(b))}, nil
}

func (m *memStore) List(ctx context.Context, prefix string, maxKeys int) ([]blobstore.ObjectRef, error) {
	var refs []blobstore.ObjectRef
	for k, b := range m.objects {
		if strings.HasPrefix(k, prefix) {
			refs = append(refs, blobstore.ObjectRef{Bucket: "dr-uploads", Key: k, Size: int64(len(b))})
		}
	}
	return refs, nil
}

func (m *memStore) Delete(ctx context.Context, key string) error {
	delete(m.objects, key)
	return nil
}

func TestSaveHappyPath(t *testing.T) {
	store := newMemStore()
	src := bytes.NewBufferString("hello world")
	res, err := Save(context.Background(), Config{}, store, src, "doc.pdf")
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if res.Size != 11 {
		t.Errorf("size=%d want 11", res.Size)
	}
	if !strings.HasPrefix(res.Key, KeyPrefix) {
		t.Errorf("key %q not under prefix %q", res.Key, KeyPrefix)
	}
	if !strings.HasSuffix(res.Key, ".pdf") {
		t.Errorf("ext on key %q should be .pdf", res.Key)
	}
	got := store.objects[res.Key]
	if string(got) != "hello world" {
		t.Errorf("contents %q want %q", got, "hello world")
	}
}

func TestSaveUnsupportedExt(t *testing.T) {
	store := newMemStore()
	src := bytes.NewBufferString("payload")
	_, err := Save(context.Background(), Config{}, store, src, "evil.exe")
	if !errors.Is(err, ErrUnsupportedExt) {
		t.Fatalf("want ErrUnsupportedExt, got %v", err)
	}
	// Should not have stored any object.
	if len(store.objects) != 0 {
		t.Errorf("expected store to stay empty, got %v", store.objects)
	}
}

func TestSaveTooLarge(t *testing.T) {
	store := newMemStore()
	big := bytes.NewBuffer(make([]byte, 2<<20)) // 2 MiB
	_, err := Save(
		context.Background(),
		Config{MaxBytes: 1 << 20}, // cap at 1 MiB
		store, big, "doc.pdf",
	)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("want ErrTooLarge, got %v", err)
	}
	// Nothing must be stored.
	if len(store.objects) != 0 {
		t.Errorf("expected store to be empty, got %v", store.objects)
	}
}

func TestSaveEmpty(t *testing.T) {
	store := newMemStore()
	_, err := Save(context.Background(), Config{}, store, bytes.NewReader(nil), "doc.pdf")
	if !errors.Is(err, ErrEmpty) {
		t.Fatalf("want ErrEmpty, got %v", err)
	}
}

func TestSaveCustomAllowList(t *testing.T) {
	store := newMemStore()
	cfg := Config{
		AllowedExts: map[string]bool{".csv": true},
	}
	if _, err := Save(context.Background(), cfg, store, bytes.NewBufferString("a,b"), "data.csv"); err != nil {
		t.Fatalf("csv should be allowed: %v", err)
	}
	if _, err := Save(context.Background(), cfg, store, bytes.NewBufferString("hi"), "doc.pdf"); !errors.Is(err, ErrUnsupportedExt) {
		t.Fatalf("pdf should be rejected with custom allow-list, got %v", err)
	}
}

func TestSaveNilStore(t *testing.T) {
	_, err := Save(context.Background(), Config{}, nil, bytes.NewBufferString("x"), "doc.pdf")
	if err == nil {
		t.Fatal("want error when store is nil, got nil")
	}
}
