// Package upload owns the "user-dragged-a-file-into-the-library-page"
// disk persistence step. Front-end calls POST /platform/sources/upload
// first to stash the bytes durably, then calls POST /platform/sources/ingest
// with target=file:<path> so a network retry doesn't re-upload.
//
// Defaults match the legacy Python (sources.py:53-62):
//
//   - DefaultDir  = /var/lib/dr-uploads   (Pi: docker volume "dr_uploads")
//   - DefaultMax  = 256 MiB
//   - allow-list  = pdf, epub, txt, md, markdown, tex, html, htm
//
// Anything outside the allow-list returns ErrUnsupportedExt; anything over
// the size cap returns ErrTooLarge -- callers should map both to 415 and
// 413 respectively.
package upload

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

// DefaultDir is where uploads land when callers don't override. Mounted
// into both the backend and r2g containers via the "dr_uploads" docker
// volume.
const DefaultDir = "/var/lib/dr-uploads"

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
	ErrTooLarge        = errors.New("upload: exceeds size limit")
	ErrUnsupportedExt  = errors.New("upload: unsupported file extension")
	ErrEmpty           = errors.New("upload: empty file")
)

// Config bundles the policy knobs. Zero values fall back to package
// defaults so callers can pass &Config{} and get the legacy behaviour.
type Config struct {
	Dir         string
	MaxBytes    int64
	AllowedExts map[string]bool
}

// Result describes the persisted file.
type Result struct {
	Path     string
	Size     int64
	Filename string
}

// Save copies r into a uuid-named file under cfg.Dir, returning the
// final path and byte count. filename's extension is checked against
// the allow-list before any bytes are written; ErrUnsupportedExt is
// returned without creating the file.
//
// Reads happen in 1 MiB chunks; if the running total crosses MaxBytes
// the partial file is unlinked and ErrTooLarge is returned. This mirrors
// the Python loop at sources.py:294-309 so reaching the cap costs
// roughly the same disk activity in both implementations.
func Save(ctx context.Context, cfg Config, r io.Reader, filename string) (*Result, error) {
	dir := cfg.Dir
	if dir == "" {
		dir = DefaultDir
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

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", dir, err)
	}

	destName := uuid.New().String() + ext
	destPath := filepath.Join(dir, destName)

	fh, err := os.OpenFile(destPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", destPath, err)
	}
	cleanup := func() {
		_ = fh.Close()
		_ = os.Remove(destPath)
	}

	const bufSize = 1 << 20 // 1 MiB
	buf := make([]byte, bufSize)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			cleanup()
			return nil, err
		}
		n, readErr := r.Read(buf)
		if n > 0 {
			total += int64(n)
			if total > maxBytes {
				cleanup()
				return nil, fmt.Errorf("%w: %d > %d", ErrTooLarge, total, maxBytes)
			}
			if _, werr := fh.Write(buf[:n]); werr != nil {
				cleanup()
				return nil, fmt.Errorf("write %s: %w", destPath, werr)
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			cleanup()
			return nil, fmt.Errorf("read upload: %w", readErr)
		}
	}

	if err := fh.Close(); err != nil {
		_ = os.Remove(destPath)
		return nil, fmt.Errorf("close %s: %w", destPath, err)
	}
	if total == 0 {
		_ = os.Remove(destPath)
		return nil, ErrEmpty
	}

	return &Result{
		Path:     destPath,
		Size:     total,
		Filename: filename,
	}, nil
}
