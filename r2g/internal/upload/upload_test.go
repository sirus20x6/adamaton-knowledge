package upload

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSaveHappyPath(t *testing.T) {
	dir := t.TempDir()
	src := bytes.NewBufferString("hello world")
	res, err := Save(context.Background(), Config{Dir: dir}, src, "doc.pdf")
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if res.Size != 11 {
		t.Errorf("size=%d want 11", res.Size)
	}
	if !strings.HasPrefix(res.Path, dir) {
		t.Errorf("path %q not under dir %q", res.Path, dir)
	}
	if filepath.Ext(res.Path) != ".pdf" {
		t.Errorf("ext on path %q should be .pdf", res.Path)
	}
	got, err := os.ReadFile(res.Path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != "hello world" {
		t.Errorf("contents %q want %q", got, "hello world")
	}
}

func TestSaveUnsupportedExt(t *testing.T) {
	dir := t.TempDir()
	src := bytes.NewBufferString("payload")
	_, err := Save(context.Background(), Config{Dir: dir}, src, "evil.exe")
	if !errors.Is(err, ErrUnsupportedExt) {
		t.Fatalf("want ErrUnsupportedExt, got %v", err)
	}
	// Should not have created any files.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("expected dir to stay empty, got %v", entries)
	}
}

func TestSaveTooLarge(t *testing.T) {
	dir := t.TempDir()
	big := bytes.NewBuffer(make([]byte, 2<<20)) // 2 MiB
	_, err := Save(
		context.Background(),
		Config{Dir: dir, MaxBytes: 1 << 20}, // cap at 1 MiB
		big, "doc.pdf",
	)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("want ErrTooLarge, got %v", err)
	}
	// Partial file must be unlinked.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("expected dir to be cleaned up, got %v", entries)
	}
}

func TestSaveEmpty(t *testing.T) {
	dir := t.TempDir()
	_, err := Save(context.Background(), Config{Dir: dir}, bytes.NewReader(nil), "doc.pdf")
	if !errors.Is(err, ErrEmpty) {
		t.Fatalf("want ErrEmpty, got %v", err)
	}
}

func TestSaveCreatesDir(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "nested", "dir")
	src := bytes.NewBufferString("x")
	res, err := Save(context.Background(), Config{Dir: target}, src, "doc.txt")
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if !strings.HasPrefix(res.Path, target) {
		t.Errorf("path %q not under target %q", res.Path, target)
	}
}

func TestSaveCustomAllowList(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		Dir:         dir,
		AllowedExts: map[string]bool{".csv": true},
	}
	if _, err := Save(context.Background(), cfg, bytes.NewBufferString("a,b"), "data.csv"); err != nil {
		t.Fatalf("csv should be allowed: %v", err)
	}
	if _, err := Save(context.Background(), cfg, bytes.NewBufferString("hi"), "doc.pdf"); !errors.Is(err, ErrUnsupportedExt) {
		t.Fatalf("pdf should be rejected with custom allow-list, got %v", err)
	}
}
