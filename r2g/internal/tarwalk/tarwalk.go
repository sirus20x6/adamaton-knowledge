// Package tarwalk reads gzipped (or plain) tar archives into memory with
// path-traversal protection and a per-member size cap. Two callers:
//
//   - internal/figures resolves \includegraphics src paths to bytes and
//     needs the full {name -> contents} map.
//   - internal/arxiv inspects an e-print archive's filename list to pick
//     the main .tex file and only needs name + size.
//
// One package, two shapes: ReadAll builds the full map (memory-bound by
// MaxMemberBytes); WalkNames yields headers only without buffering
// contents.
package tarwalk

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
)

// DefaultMaxMemberBytes is the per-member ceiling used by ReadAll when
// the caller doesn't override it. Matches the legacy Python path's
// 32 MiB limit on raw figure bytes.
const DefaultMaxMemberBytes = 32 << 20

// Entry is the header surface WalkNames yields. Size is the declared
// uncompressed size from the tar header; oversized members are kept in
// the list (callers may want to log them) but never read.
type Entry struct {
	Name string
	Size int64
}

// ReadAll loads every regular file in raw into a name->contents map.
// raw may be a gzipped tar or a plain tar; the reader sniffs the gzip
// magic and falls back. maxMember caps per-member bytes; zero falls back
// to DefaultMaxMemberBytes. Members exceeding the cap are silently
// skipped, the same as the legacy Python _safe_extract path.
//
// Names are path.Clean'd; entries with leading "../" or absolute paths
// are dropped. Symlinks and hardlinks are dropped.
func ReadAll(raw []byte, maxMember int64) (map[string][]byte, error) {
	if maxMember <= 0 {
		maxMember = DefaultMaxMemberBytes
	}
	r, err := decompress(raw)
	if err != nil {
		return nil, err
	}
	out := map[string][]byte{}
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return out, fmt.Errorf("tar next: %w", err)
		}
		if !isRegular(hdr) {
			continue
		}
		name, ok := safeName(hdr.Name)
		if !ok {
			continue
		}
		if hdr.Size > maxMember {
			continue
		}
		buf := make([]byte, hdr.Size)
		if _, err := io.ReadFull(tr, buf); err != nil {
			return out, fmt.Errorf("read tar member %q: %w", name, err)
		}
		out[name] = buf
	}
	return out, nil
}

// WalkNames returns the regular-file entries in raw without buffering
// their contents. Suitable for callers that only need to inspect the
// archive's shape (file list, sizes).
func WalkNames(raw []byte) ([]Entry, error) {
	r, err := decompress(raw)
	if err != nil {
		return nil, err
	}
	var out []Entry
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return out, fmt.Errorf("tar next: %w", err)
		}
		if !isRegular(hdr) {
			continue
		}
		name, ok := safeName(hdr.Name)
		if !ok {
			continue
		}
		out = append(out, Entry{Name: name, Size: hdr.Size})
	}
	return out, nil
}

// decompress returns a Reader over the decompressed body. Tries gzip
// first; falls back to plain tar if the gzip header is missing. Some
// arxiv e-prints are uncompressed tars masquerading under a .gz URL.
func decompress(raw []byte) (io.Reader, error) {
	if len(raw) < 2 {
		return nil, fmt.Errorf("archive too small (%d bytes)", len(raw))
	}
	if raw[0] == 0x1f && raw[1] == 0x8b {
		gz, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, fmt.Errorf("gzip reader: %w", err)
		}
		return gz, nil
	}
	return bytes.NewReader(raw), nil
}

func isRegular(hdr *tar.Header) bool {
	return hdr.Typeflag == tar.TypeReg || hdr.Typeflag == tar.TypeRegA
}

// safeName normalises a tar member's path and rejects anything that
// would escape a notional extraction root. Even though tarwalk does
// no filesystem I/O, callers (figures) treat the name as a map key --
// a hostile "../config.txt" entry could collide with a legitimate
// filename otherwise.
func safeName(name string) (string, bool) {
	clean := path.Clean(name)
	if strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") {
		return "", false
	}
	return clean, true
}
