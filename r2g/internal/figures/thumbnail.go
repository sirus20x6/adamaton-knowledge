// Package figures owns the post-ingest figure-extract path: walking the
// arxiv LaTeX tarball, resolving \includegraphics src_paths (reported by
// latex2text), reading raw image bytes, rendering 256-px PNG thumbnails,
// and persisting rows into platform.document_figures.
//
// Lived in Python at platform/backend/app/ingestion/latex_figures.py
// until 2026-05-15. The pdf2image -> pdftoppm subprocess chain it used
// pegged the FastAPI event loop under reindex pressure and made the
// Library page unloadable. Reimplemented here so it runs in r2g's
// process, next to the rest of the ingest pipeline.
package figures

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/png"

	// Side-effect imports so image.Decode handles the formats embedded
	// in arxiv figures. SVG/EPS are intentionally absent — they get
	// skipped (no thumbnail) just like the Python path did.
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	_ "golang.org/x/image/tiff"

	"github.com/gen2brain/go-fitz"
	"golang.org/x/image/draw"
)

// ThumbnailPx is the square dimension of generated thumbnails. Matches
// the Python value (_THUMBNAIL_PX in latex_figures.py) so old and new
// rows are visually consistent in the Library UI.
const ThumbnailPx = 256

// MaxImagePixels caps raster decodes. PIL warned at ~89M px (its default
// DecompressionBombWarning threshold); the largest figure we observed
// in the corpus was 162M px and pegged the backend for minutes. We
// reject anything bigger than 100M px outright — the thumbnail
// downstream wouldn't have been usable anyway.
const MaxImagePixels = 100_000_000

// ErrOversize is returned by MakeThumbnail for raster images whose
// pixel count exceeds MaxImagePixels. The caller logs and skips the
// thumbnail (the raw_bytes still go into the DB).
var ErrOversize = errors.New("image exceeds pixel cap")

// MakeThumbnail renders a 256-px PNG thumbnail from raw image bytes.
//
// For raw_format == "pdf" the first page is rasterised via go-fitz
// (MuPDF) at the same resolution Python used (dpi 150). For the
// bitmap formats supported by Python (png / jpg / jpeg / gif / tiff)
// we go through stdlib image.Decode. svg / eps are not handled —
// returns (nil, nil) so the caller can persist the raw bytes without
// a thumbnail, matching the Python behaviour at latex_figures.py:368.
func MakeThumbnail(raw []byte, format string) ([]byte, error) {
	switch format {
	case "pdf":
		return thumbnailPDF(raw)
	case "png", "jpg", "jpeg", "gif", "tiff", "tif":
		return thumbnailRaster(raw)
	default:
		// svg / eps / unknown: same as Python (returns None).
		return nil, nil
	}
}

func thumbnailPDF(raw []byte) ([]byte, error) {
	doc, err := fitz.NewFromMemory(raw)
	if err != nil {
		return nil, fmt.Errorf("fitz open: %w", err)
	}
	defer doc.Close()
	if doc.NumPage() == 0 {
		return nil, nil
	}
	// dpi 150 matches pdf2image's convert_from_bytes(dpi=150) in the
	// Python code. go-fitz's ImageDPI takes the DPI directly.
	img, err := doc.ImageDPI(0, 150)
	if err != nil {
		return nil, fmt.Errorf("fitz render page 0: %w", err)
	}
	return encodePNGThumbnail(img)
}

func thumbnailRaster(raw []byte) ([]byte, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	if cfg.Width*cfg.Height > MaxImagePixels {
		return nil, ErrOversize
	}
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("decode image: %w", err)
	}
	return encodePNGThumbnail(img)
}

// encodePNGThumbnail resizes preserving aspect ratio so the longest side
// is ThumbnailPx, then PNG-encodes. CatmullRom matches PIL's default
// "thumbnail" filter (BICUBIC) close enough that visual parity is fine.
func encodePNGThumbnail(src image.Image) ([]byte, error) {
	b := src.Bounds()
	srcW, srcH := b.Dx(), b.Dy()
	if srcW <= 0 || srcH <= 0 {
		return nil, fmt.Errorf("empty image bounds")
	}
	dstW, dstH := scaleToFit(srcW, srcH, ThumbnailPx)
	dst := image.NewRGBA(image.Rect(0, 0, dstW, dstH))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, b, draw.Over, nil)
	var out bytes.Buffer
	if err := png.Encode(&out, dst); err != nil {
		return nil, fmt.Errorf("png encode: %w", err)
	}
	return out.Bytes(), nil
}

func scaleToFit(w, h, max int) (int, int) {
	if w <= max && h <= max {
		return w, h
	}
	if w >= h {
		return max, max * h / w
	}
	return max * w / h, max
}
