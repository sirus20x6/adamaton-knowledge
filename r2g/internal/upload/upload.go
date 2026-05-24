// Package upload owns the "user-dragged-a-file-into-the-library-page"
// persistence step. Front-end calls POST /platform/sources/upload first to
// stash the bytes durably, then calls POST /platform/sources/ingest with
// target=file:<key> (or s3://dr-uploads/<key>) so a network retry doesn't
// re-upload.
//
// As of the core/blobstore migration the bytes land in the shared S3 /
// Garage blob store (bucket "dr-uploads") rather than a local filesystem
// directory, so r2g and the platform plugin-host can read the same object
// without a shared docker volume.
//
//   - bucket      = dr-uploads   (blobstore.ConfigFromEnv("dr-uploads"))
//   - object key  = uploads/<uuid>[ext]
//   - DefaultMax  = 256 MiB
//   - allow-list  = pdf, epub, txt, md, markdown, tex, html, htm
//
// Anything outside the allow-list returns ErrUnsupportedExt; anything over
// the size cap returns ErrTooLarge -- callers should map both to 415 and
// 413 respectively.
package upload

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"strings"

	"github.com/google/uuid"

	"github.com/sirus20x6/adamaton-core/blobstore"
)

// KeyPrefix is the object-key prefix for general source uploads in the
// dr-uploads bucket. Keys are of the form "uploads/<uuid>[ext]".
const KeyPrefix = "uploads/"

// DefaultMaxBytes is the per-upload ceiling. 256 MiB matches the Python
// PLATFORM_UPLOAD_MAX_BYTES default.
const DefaultMaxBytes int64 = 256 << 20

// DefaultAllowedExts is the file-extension allow-list. Matches the
// Python _ALLOWED_UPLOAD_EXTS frozenset. Extensions are lowercase and
// include the leading dot.
var DefaultAllowedExts = map[string]bool{
	".pdf":      true,
	".epub":     true,
	".txt":      true,
	".md":       true,
	".markdown": true,
	".tex":      true,
	".html":     true,
	".htm":      true,
}

// Sentinel errors so HTTP handlers can translate to the right status.
var (
	ErrTooLarge       = errors.New("upload: exceeds size limit")
	ErrUnsupportedExt = errors.New("upload: unsupported file extension")
	ErrEmpty          = errors.New("upload: empty file")
)

// Config bundles the policy knobs. Zero values fall back to package
// defaults so callers can pass &Config{} and get the legacy behaviour.
type Config struct {
	MaxBytes    int64
	AllowedExts map[string]bool
}

// Result describes the persisted blob. Key is the object key within the
// dr-uploads bucket (e.g. "uploads/<uuid>.pdf") -- NOT a filesystem path.
// Callers hand this key back to clients and pass it to the ingest path
// (bare, or wrapped as s3://dr-uploads/<key>).
type Result struct {
	Key      string
	Size     int64
	Filename string
}

// Save streams r into the dr-uploads bucket under key "uploads/<uuid>[ext]",
// returning the object key and byte count. filename's extension is checked
// against the allow-list before any bytes are read; ErrUnsupportedExt is
// returned without touching the store.
//
// The whole upload is buffered in memory (bounded by MaxBytes) so the size
// is known before the PUT and the running total can be enforced against the
// cap exactly as the legacy disk loop did (sources.py:294-309). 256 MiB is
// the ceiling, matching the per-upload limit.
func Save(ctx context.Context, cfg Config, store blobstore.Backend, r io.Reader, filename string) (*Result, error) {
	if store == nil {
		return nil, errors.New("upload: blob store is not configured")
	}
	maxBytes := cfg.MaxBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	allowed := cfg.AllowedExts
	if allowed == nil {
		allowed = DefaultAllowedExts
	}

	if filename == "" {
		filename = "upload"
	}
	ext := strings.ToLower(filepath.Ext(filename))
	if ext != "" && !allowed[ext] {
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedExt, ext)
	}

	// Read up to maxBytes+1 so we can detect an over-cap upload without
	// holding more than one extra byte in memory. Mirrors the legacy
	// disk loop's "unlink the partial file and reject" behaviour.
	limited := io.LimitReader(r, maxBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read upload: %w", err)
	}
	total := int64(len(data))
	if total > maxBytes {
		return nil, fmt.Errorf("%w: %d > %d", ErrTooLarge, total, maxBytes)
	}
	if total == 0 {
		return nil, ErrEmpty
	}

	key := path.Join(KeyPrefix, uuid.New().String()+ext)

	if _, err := store.Put(ctx, key, bytes.NewReader(data), total); err != nil {
		return nil, fmt.Errorf("put %s: %w", key, err)
	}

	return &Result{
		Key:      key,
		Size:     total,
		Filename: filename,
	}, nil
}
