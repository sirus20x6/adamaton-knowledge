package routes

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sirupsen/logrus"
)

// figureContentTypes maps a row's raw_format column to a Content-Type.
// Matches the Python _CONTENT_TYPES at figures.py:41 verbatim so the
// browser sees identical headers across the cutover.
var figureContentTypes = map[string]string{
	"pdf":  "application/pdf",
	"png":  "image/png",
	"jpg":  "image/jpeg",
	"jpeg": "image/jpeg",
	"gif":  "image/gif",
	"tiff": "image/tiff",
	"svg":  "image/svg+xml",
	"eps":  "application/postscript",
}

// RegisterFigures wires the three /platform/figures endpoints onto r. The
// thumbnail and raw routes are registered before the list route so that
// gorilla/mux matches them before falling back to the catch-all
// /{doc_id} path. (mux respects registration order for ambiguous paths,
// but path length is also a tiebreaker — both belt and suspenders.)
func RegisterFigures(r *mux.Router, pool *pgxpool.Pool, logger *logrus.Logger) {
	// More specific routes first.
	r.HandleFunc("/platform/figures/{doc_id}/{fig_id}.png",
		figureThumbnailHandler(pool, logger)).Methods(http.MethodGet)
	r.HandleFunc("/platform/figures/{doc_id}/{fig_id}/raw",
		figureRawHandler(pool, logger)).Methods(http.MethodGet)
	r.HandleFunc("/platform/figures/{doc_id}",
		figureListHandler(pool, logger)).Methods(http.MethodGet)
}

// figureListHandler returns the figure metadata for a document. Matches
// the legacy Python response shape: {document_id, count, figures:[...]}
// with octet_length()-sized raw_bytes / thumbnail_bytes fields so the UI
// can show sizes without paying to ship the bytes.
func figureListHandler(pool *pgxpool.Pool, logger *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		docIDStr := mux.Vars(req)["doc_id"]
		docID, err := uuid.Parse(docIDStr)
		if err != nil {
			writeJSON(w, http.StatusUnprocessableEntity,
				errorBody("invalid doc_id: "+err.Error()))
			return
		}

		rows, err := pool.Query(req.Context(), `
			SELECT
			    id, COALESCE(label, ''), COALESCE(section_label, ''),
			    COALESCE(caption_text, ''), COALESCE(caption_tex, ''),
			    COALESCE(src_path, ''), raw_format,
			    octet_length(raw_bytes)       AS raw_bytes,
			    COALESCE(octet_length(thumbnail_png), 0) AS thumbnail_bytes,
			    order_in_doc, created_at
			FROM platform.document_figures
			WHERE document_id = $1
			ORDER BY order_in_doc
		`, docID)
		if err != nil {
			logger.WithError(err).WithField("doc_id", docID).Error("list figures query failed")
			writeJSON(w, http.StatusInternalServerError, errorBody("internal error"))
			return
		}
		defer rows.Close()

		figures := []map[string]any{}
		for rows.Next() {
			var (
				id, label, section, captionText, captionTex, srcPath, rawFormat string
				rawBytes, thumbBytes, orderInDoc                                 int64
				createdAt                                                        time.Time
			)
			if err := rows.Scan(
				&id, &label, &section, &captionText, &captionTex,
				&srcPath, &rawFormat, &rawBytes, &thumbBytes, &orderInDoc, &createdAt,
			); err != nil {
				logger.WithError(err).Error("scan figure row failed")
				writeJSON(w, http.StatusInternalServerError, errorBody("internal error"))
				return
			}
			figures = append(figures, map[string]any{
				"id":              id,
				"label":           label,
				"section_label":   section,
				"caption_text":    captionText,
				"caption_tex":     captionTex,
				"src_path":        srcPath,
				"raw_format":      rawFormat,
				"raw_bytes":       rawBytes,
				"thumbnail_bytes": thumbBytes,
				"order_in_doc":    orderInDoc,
				"created_at":      createdAt.Format(time.RFC3339Nano),
			})
		}
		if err := rows.Err(); err != nil {
			logger.WithError(err).Error("iterate figure rows failed")
			writeJSON(w, http.StatusInternalServerError, errorBody("internal error"))
			return
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"document_id": docID.String(),
			"count":       len(figures),
			"figures":     figures,
		})
	}
}

// figureThumbnailHandler returns the 256-px PNG thumbnail. 404 if the
// row doesn't exist OR has a null thumbnail_png (typical for svg/eps
// sources we can't rasterise locally -- the figures extractor leaves
// those rows with raw_bytes populated and thumbnail_png NULL).
func figureThumbnailHandler(pool *pgxpool.Pool, logger *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		docID, figID, ok := parseFigureIDs(w, req)
		if !ok {
			return
		}

		var thumb []byte
		err := pool.QueryRow(req.Context(), `
			SELECT thumbnail_png
			FROM platform.document_figures
			WHERE id = $1 AND document_id = $2
		`, figID, docID).Scan(&thumb)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeJSON(w, http.StatusNotFound, errorBody("figure not found"))
				return
			}
			logger.WithError(err).Error("thumbnail query failed")
			writeJSON(w, http.StatusInternalServerError, errorBody("internal error"))
			return
		}
		if len(thumb) == 0 {
			writeJSON(w, http.StatusNotFound,
				errorBody("figure has no thumbnail (source not rasterisable)"))
			return
		}

		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "public, max-age=3600, immutable")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(thumb)
	}
}

// figureRawHandler returns the original figure bytes as uploaded to the
// e-print archive (PDF/PNG/JPG/EPS/SVG). Content-Type derived from
// raw_format; falls back to octet-stream so the browser at least
// downloads rather than guessing.
func figureRawHandler(pool *pgxpool.Pool, logger *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		docID, figID, ok := parseFigureIDs(w, req)
		if !ok {
			return
		}

		var (
			raw    []byte
			format string
		)
		err := pool.QueryRow(req.Context(), `
			SELECT raw_bytes, raw_format
			FROM platform.document_figures
			WHERE id = $1 AND document_id = $2
		`, figID, docID).Scan(&raw, &format)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeJSON(w, http.StatusNotFound, errorBody("figure not found"))
				return
			}
			logger.WithError(err).Error("raw figure query failed")
			writeJSON(w, http.StatusInternalServerError, errorBody("internal error"))
			return
		}

		mediaType := figureContentTypes[format]
		if mediaType == "" {
			mediaType = "application/octet-stream"
		}
		w.Header().Set("Content-Type", mediaType)
		w.Header().Set("Cache-Control", "public, max-age=3600, immutable")
		w.Header().Set("Content-Disposition",
			fmt.Sprintf(`inline; filename="%s.%s"`, figID.String(), format))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(raw)
	}
}

// parseFigureIDs pulls doc_id + fig_id from the URL vars and 422s on
// malformed UUIDs. Returns ok=false after writing the response when the
// caller should bail.
func parseFigureIDs(w http.ResponseWriter, req *http.Request) (uuid.UUID, uuid.UUID, bool) {
	vars := mux.Vars(req)
	docID, err := uuid.Parse(vars["doc_id"])
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity,
			errorBody("invalid doc_id: "+err.Error()))
		return uuid.Nil, uuid.Nil, false
	}
	figID, err := uuid.Parse(vars["fig_id"])
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity,
			errorBody("invalid fig_id: "+err.Error()))
		return uuid.Nil, uuid.Nil, false
	}
	return docID, figID, true
}
