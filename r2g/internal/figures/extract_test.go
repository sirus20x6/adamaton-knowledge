package figures

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"image"
	"image/png"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sirupsen/logrus"
)

// minimal 4x4 png to seed the synthetic tarball.
func tinyPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			img.Set(x, y, image.NewUniform(image.Black.At(0, 0)).At(0, 0))
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode tiny png: %v", err)
	}
	return buf.Bytes()
}

func mkTar(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var raw bytes.Buffer
	gz := gzip.NewWriter(&raw)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header %s: %v", name, err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatalf("write body %s: %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gz: %v", err)
	}
	return raw.Bytes()
}

func TestExtractAndPersist_TwoFiguresResolved(t *testing.T) {
	png4 := tinyPNG(t)
	tarball := mkTar(t, map[string][]byte{
		"main.tex":            []byte(`\begin{figure}\includegraphics{a.png}\end{figure}`),
		"a.png":               png4,
		"figures/results.png": png4,
	})
	metas := []FigureMeta{
		{Label: "fig:a", Caption: "first", SrcPaths: []string{"a.png"}},
		{Label: "fig:b", Caption: "second", SrcPaths: []string{"figures/results.png"}},
	}
	var got []FigureRow
	stub := func(_ context.Context, _ *pgxpool.Pool, rows []FigureRow) (int, error) {
		got = append(got, rows...)
		return len(rows), nil
	}
	n, err := ExtractAndPersist(context.Background(), nil, uuid.New(), tarball, metas, logrus.New(), stub)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if n != 2 {
		t.Fatalf("rows: got %d, want 2", n)
	}
	if got[0].SrcPath != "a.png" || got[0].RawFormat != "png" {
		t.Fatalf("row[0]: %+v", got[0])
	}
	if got[1].SrcPath != "figures/results.png" {
		t.Fatalf("row[1].SrcPath: %s", got[1].SrcPath)
	}
	// Dense order_in_doc (parity with Python _resolve_in_tree post-loop).
	if got[0].OrderInDoc != 0 || got[1].OrderInDoc != 1 {
		t.Fatalf("order: %d/%d", got[0].OrderInDoc, got[1].OrderInDoc)
	}
	// PNG thumbnail must be a valid PNG.
	if len(got[0].ThumbnailPNG) == 0 {
		t.Fatal("thumbnail missing for png")
	}
	if _, err := png.Decode(bytes.NewReader(got[0].ThumbnailPNG)); err != nil {
		t.Fatalf("thumbnail not a valid png: %v", err)
	}
}

func TestExtractAndPersist_ExtensionFallback(t *testing.T) {
	png4 := tinyPNG(t)
	tarball := mkTar(t, map[string][]byte{
		"figures/diagram.png": png4,
	})
	// includegraphics{figures/diagram} (no extension) should resolve.
	metas := []FigureMeta{
		{Label: "fig:x", SrcPaths: []string{"figures/diagram"}},
	}
	var got []FigureRow
	stub := func(_ context.Context, _ *pgxpool.Pool, rows []FigureRow) (int, error) {
		got = append(got, rows...)
		return len(rows), nil
	}
	n, err := ExtractAndPersist(context.Background(), nil, uuid.New(), tarball, metas, logrus.New(), stub)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if n != 1 || got[0].SrcPath != "figures/diagram.png" {
		t.Fatalf("fallback miss: n=%d row=%+v", n, got)
	}
}

func TestExtractAndPersist_UnresolvableDropped(t *testing.T) {
	png4 := tinyPNG(t)
	tarball := mkTar(t, map[string][]byte{"a.png": png4})
	metas := []FigureMeta{
		{Label: "fig:nope", SrcPaths: []string{"missing.png"}},
		{Label: "fig:ok", SrcPaths: []string{"a.png"}},
	}
	var got []FigureRow
	stub := func(_ context.Context, _ *pgxpool.Pool, rows []FigureRow) (int, error) {
		got = append(got, rows...)
		return len(rows), nil
	}
	n, err := ExtractAndPersist(context.Background(), nil, uuid.New(), tarball, metas, logrus.New(), stub)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if n != 1 || got[0].Label != "fig:ok" {
		t.Fatalf("unresolvable not silently dropped: %+v", got)
	}
	if got[0].OrderInDoc != 0 {
		t.Fatalf("order_in_doc: got %d, want 0 (dense after drop)", got[0].OrderInDoc)
	}
}

func TestExtractAndPersist_MalformedTarballIsEmpty(t *testing.T) {
	metas := []FigureMeta{{Label: "fig:a", SrcPaths: []string{"a.png"}}}
	stub := func(_ context.Context, _ *pgxpool.Pool, rows []FigureRow) (int, error) {
		t.Fatalf("inserter called with garbage tarball: %d rows", len(rows))
		return 0, nil
	}
	n, err := ExtractAndPersist(context.Background(), nil, uuid.New(), []byte("not a tarball"), metas, logrus.New(), stub)
	if err == nil && n == 0 {
		return // acceptable: tarball unreadable, nothing to insert
	}
	if err != nil {
		// Also acceptable — we just want to make sure we don't blow up
		// or call the inserter with phantom rows.
		return
	}
	t.Fatalf("unexpected: n=%d err=%v", n, err)
}
