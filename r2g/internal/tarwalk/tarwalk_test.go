package tarwalk

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"sort"
	"testing"
)

type member struct {
	name     string
	body     string
	typeflag byte
	linkname string
}

// makeTar builds a tar (optionally gzipped) from the given members.
func makeTar(t *testing.T, gzipped bool, members []member) []byte {
	t.Helper()
	var raw bytes.Buffer
	var tw *tar.Writer
	var gz *gzip.Writer
	if gzipped {
		gz = gzip.NewWriter(&raw)
		tw = tar.NewWriter(gz)
	} else {
		tw = tar.NewWriter(&raw)
	}
	for _, m := range members {
		tf := m.typeflag
		if tf == 0 {
			tf = tar.TypeReg
		}
		hdr := &tar.Header{
			Name:     m.name,
			Mode:     0o644,
			Size:     int64(len(m.body)),
			Typeflag: tf,
			Linkname: m.linkname,
		}
		if tf != tar.TypeReg && tf != tar.TypeRegA {
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header %q: %v", m.name, err)
		}
		if hdr.Size > 0 {
			if _, err := tw.Write([]byte(m.body)); err != nil {
				t.Fatalf("write body %q: %v", m.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if gzipped {
		if err := gz.Close(); err != nil {
			t.Fatalf("gzip close: %v", err)
		}
	}
	return raw.Bytes()
}

func TestReadAll_GzippedAndPlain(t *testing.T) {
	members := []member{
		{name: "a.tex", body: "alpha"},
		{name: "dir/b.png", body: "beta"},
	}
	for _, gzipped := range []bool{true, false} {
		name := "plain"
		if gzipped {
			name = "gzipped"
		}
		t.Run(name, func(t *testing.T) {
			raw := makeTar(t, gzipped, members)
			out, err := ReadAll(raw, 0)
			if err != nil {
				t.Fatalf("ReadAll: %v", err)
			}
			if len(out) != 2 {
				t.Fatalf("got %d members, want 2: %v", len(out), keys(out))
			}
			if string(out["a.tex"]) != "alpha" {
				t.Errorf("a.tex = %q, want alpha", out["a.tex"])
			}
			if string(out["dir/b.png"]) != "beta" {
				t.Errorf("dir/b.png = %q, want beta", out["dir/b.png"])
			}
		})
	}
}

// TestReadAll_SkipsNonRegular drops dirs, symlinks, hardlinks.
func TestReadAll_SkipsNonRegular(t *testing.T) {
	members := []member{
		{name: "keep.txt", body: "data"},
		{name: "adir/", typeflag: tar.TypeDir},
		{name: "link", typeflag: tar.TypeSymlink, linkname: "keep.txt"},
		{name: "hard", typeflag: tar.TypeLink, linkname: "keep.txt"},
	}
	out, err := ReadAll(makeTar(t, true, members), 0)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("got %v, want only keep.txt", keys(out))
	}
	if _, ok := out["keep.txt"]; !ok {
		t.Errorf("keep.txt missing: %v", keys(out))
	}
}

// TestReadAll_PathTraversalDropped rejects escaping/absolute names.
func TestReadAll_PathTraversalDropped(t *testing.T) {
	members := []member{
		{name: "../escape.txt", body: "x"},
		{name: "/abs.txt", body: "x"},
		{name: "ok.txt", body: "y"},
		{name: "nested/../still_ok.txt", body: "z"}, // cleans to still_ok.txt
	}
	out, err := ReadAll(makeTar(t, true, members), 0)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if _, ok := out["ok.txt"]; !ok {
		t.Errorf("ok.txt should be present: %v", keys(out))
	}
	if _, ok := out["still_ok.txt"]; !ok {
		t.Errorf("nested/../still_ok.txt should clean to still_ok.txt: %v", keys(out))
	}
	for _, bad := range []string{"../escape.txt", "/abs.txt", "escape.txt", "abs.txt"} {
		if _, ok := out[bad]; ok {
			t.Errorf("traversal entry %q should be dropped", bad)
		}
	}
}

// TestReadAll_MaxMemberCap skips members over the per-member ceiling.
func TestReadAll_MaxMemberCap(t *testing.T) {
	members := []member{
		{name: "small.txt", body: "ab"},       // 2 bytes
		{name: "big.txt", body: "abcdefghij"}, // 10 bytes
	}
	out, err := ReadAll(makeTar(t, true, members), 5)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if _, ok := out["small.txt"]; !ok {
		t.Errorf("small.txt under cap should be kept: %v", keys(out))
	}
	if _, ok := out["big.txt"]; ok {
		t.Errorf("big.txt over cap should be skipped: %v", keys(out))
	}
}

// TestReadAll_DefaultCapWhenZeroOrNegative confirms the fallback constant.
func TestReadAll_DefaultCapWhenZeroOrNegative(t *testing.T) {
	raw := makeTar(t, true, []member{{name: "f.txt", body: "hi"}})
	for _, cap := range []int64{0, -1} {
		out, err := ReadAll(raw, cap)
		if err != nil {
			t.Fatalf("ReadAll(cap=%d): %v", cap, err)
		}
		if string(out["f.txt"]) != "hi" {
			t.Errorf("cap=%d: f.txt = %q, want hi", cap, out["f.txt"])
		}
	}
}

// TestReadAll_TooSmall errors on a body that can't hold a magic prefix.
func TestReadAll_TooSmall(t *testing.T) {
	_, err := ReadAll([]byte{0x1f}, 0)
	if err == nil {
		t.Fatal("expected error for too-small archive")
	}
}

// TestReadAll_BadGzip errors when gzip magic is present but the stream is
// corrupt.
func TestReadAll_BadGzip(t *testing.T) {
	_, err := ReadAll([]byte{0x1f, 0x8b, 0x00, 0x00, 0x00}, 0)
	if err == nil {
		t.Fatal("expected gzip reader error")
	}
}

// TestReadAll_TruncatedTar surfaces a read error when the archive is cut
// off inside a member's body (io.ReadFull short read).
func TestReadAll_TruncatedTar(t *testing.T) {
	// A 512-byte tar header followed by a (declared) 600-byte body, but we
	// only keep the header + a sliver of the body so ReadFull underruns.
	full := makeTar(t, false, []member{{name: "a.txt", body: string(bytes.Repeat([]byte("x"), 600))}})
	truncated := full[:512+100]
	_, err := ReadAll(truncated, 0)
	if err == nil {
		t.Fatal("expected error on truncated tar body")
	}
}

func TestWalkNames(t *testing.T) {
	members := []member{
		{name: "a.tex", body: "1234"},
		{name: "b.png", body: "0123456789"},
		{name: "adir/", typeflag: tar.TypeDir},
		{name: "../escape", body: "x"},
		{name: "link", typeflag: tar.TypeSymlink, linkname: "a.tex"},
	}
	entries, err := WalkNames(makeTar(t, true, members))
	if err != nil {
		t.Fatalf("WalkNames: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2: %+v", len(entries), entries)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	if entries[0].Name != "a.tex" || entries[0].Size != 4 {
		t.Errorf("entry[0] = %+v, want {a.tex 4}", entries[0])
	}
	if entries[1].Name != "b.png" || entries[1].Size != 10 {
		t.Errorf("entry[1] = %+v, want {b.png 10}", entries[1])
	}
}

// TestWalkNames_KeepsOversized: WalkNames does not apply a size cap, so a
// large declared size is still listed (only ReadAll skips it).
func TestWalkNames_KeepsOversized(t *testing.T) {
	big := bytes.Repeat([]byte("x"), 1024)
	entries, err := WalkNames(makeTar(t, true, []member{{name: "big.bin", body: string(big)}}))
	if err != nil {
		t.Fatalf("WalkNames: %v", err)
	}
	if len(entries) != 1 || entries[0].Size != 1024 {
		t.Errorf("entries = %+v, want one 1024-byte entry", entries)
	}
}

func TestWalkNames_BadGzip(t *testing.T) {
	_, err := WalkNames([]byte{0x1f, 0x8b, 0x00, 0x00})
	if err == nil {
		t.Fatal("expected gzip error")
	}
}

func TestSafeName(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"a/b.txt", "a/b.txt", true},
		{"./a.txt", "a.txt", true},
		{"a/../b.txt", "b.txt", true},
		{"../escape", "", false},
		{"/abs", "", false},
		{"../../deep/escape", "", false},
		{"normal.tex", "normal.tex", true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, ok := safeName(tc.in)
			if ok != tc.ok || got != tc.want {
				t.Errorf("safeName(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
