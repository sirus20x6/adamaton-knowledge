package routes

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sirupsen/logrus"

	"github.com/sirus20x6/adamaton-knowledge/r2g/chunking"
	"github.com/sirus20x6/adamaton-knowledge/r2g/embeddings"
	"github.com/sirus20x6/adamaton-knowledge/r2g/internal/ingestpipeline"
)

// Max payload r2g will accept on /v3/documents. 50 MB covers all the
// arxiv markdown/PDF bodies we've seen on this corpus with headroom.
// Anything larger is almost certainly a misuse (e.g. a video transcript
// the user actually wants on a separate path).
const maxIngestBytes = 50 << 20

// defaultOwnerID is the deepresearch superuser UUID used by R2R when
// auth is disabled (require_authentication=false in platform.toml).
// r2g defaults to it when callers don't supply one so the rows it
// writes don't trip R2R's pydantic UUID-is-required validation if
// any /v3/* endpoint we haven't ported tries to read them back.
// Override with R2G_DEFAULT_OWNER_ID env.
var defaultOwnerID = os.Getenv("R2G_DEFAULT_OWNER_ID")

// RegisterIngest wires POST /v3/documents.
//
// Accepted request shapes:
//
//   - multipart/form-data with a `file` part. Form fields read: id,
//     metadata, collection_ids, ingestion_mode, run_with_orchestration.
//     Unknown fields are silently ignored.
//
//   - application/json with `{raw_text, id?, metadata?, collection_ids?}`.
//
// The actual ingest work lives in internal/ingestpipeline; this handler
// is the HTTP-shape adapter for that pipeline. /platform/sources/ingest
// shares the same pipeline via routes/sources.go.
func RegisterIngest(
	r *mux.Router,
	pool *pgxpool.Pool,
	embedder *embeddings.Client,
	chunker *chunking.Chunker,
	logger *logrus.Logger,
) {
	r.HandleFunc("/v3/documents", func(w http.ResponseWriter, req *http.Request) {
		ct := req.Header.Get("Content-Type")
		var in ingestRaw
		var err error
		switch {
		case strings.HasPrefix(ct, "multipart/form-data"):
			in, err = parseMultipart(req)
		case strings.HasPrefix(ct, "application/json"):
			in, err = parseJSON(req)
		default:
			writeJSON(w, http.StatusUnsupportedMediaType, errorBody(
				"content-type must be multipart/form-data or application/json"))
			return
		}
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errorBody(err.Error()))
			return
		}

		if len(in.body) == 0 {
			writeJSON(w, http.StatusBadRequest, errorBody("empty body"))
			return
		}
		if len(in.body) > maxIngestBytes {
			writeJSON(w, http.StatusRequestEntityTooLarge,
				errorBody(fmt.Sprintf("body exceeds %d bytes", maxIngestBytes)))
			return
		}

		// Default owner_id to the platform's superuser UUID when callers
		// don't pin one. See defaultOwnerID comment above.
		if in.ownerID == nil && defaultOwnerID != "" {
			if id, err := uuid.Parse(defaultOwnerID); err == nil {
				in.ownerID = &id
			}
		}

		res, err := ingestpipeline.Run(req.Context(), pool, embedder, chunker, ingestpipeline.Input{
			DocumentID:    in.id,
			Title:         in.title,
			ContentType:   in.contentType,
			Body:          in.body,
			Metadata:      in.metadata,
			CollectionIDs: in.collectionIDs,
			OwnerID:       in.ownerID,
			DocumentType:  in.documentType,
		}, logger)
		if err != nil {
			writePipelineError(w, logger, in.id, err)
			return
		}

		// R2R's simple-mode response shape.
		writeJSON(w, http.StatusOK, map[string]any{
			"results": map[string]any{
				"message":     "Document created and ingested successfully.",
				"document_id": res.DocumentID.String(),
				"task_id":     nil,
			},
		})
	}).Methods(http.MethodPost)
}

// writePipelineError translates pipeline sentinel errors to HTTP status
// codes. Called from any handler that funnels through ingestpipeline.Run.
func writePipelineError(w http.ResponseWriter, logger *logrus.Logger, docID uuid.UUID, err error) {
	switch {
	case errors.Is(err, ingestpipeline.ErrUnsupportedType):
		writeJSON(w, http.StatusUnsupportedMediaType, errorBody(err.Error()))
	case errors.Is(err, ingestpipeline.ErrParseFailed):
		writeJSON(w, http.StatusUnprocessableEntity, errorBody(err.Error()))
	case errors.Is(err, ingestpipeline.ErrZeroChunks):
		writeJSON(w, http.StatusUnprocessableEntity, errorBody(err.Error()))
	case errors.Is(err, ingestpipeline.ErrChunkFailed):
		logger.WithError(err).WithField("document_id", docID).Error("chunk failed")
		writeJSON(w, http.StatusInternalServerError, errorBody("chunk error"))
	case errors.Is(err, ingestpipeline.ErrEmbedFailed):
		logger.WithError(err).WithField("document_id", docID).Error("embed failed")
		writeJSON(w, http.StatusBadGateway, errorBody(err.Error()))
	case errors.Is(err, ingestpipeline.ErrAlreadyIngested):
		writeJSON(w, http.StatusConflict, errorBody("document already ingested"))
	case errors.Is(err, ingestpipeline.ErrPersistFailed):
		logger.WithError(err).WithField("document_id", docID).Error("persist failed")
		writeJSON(w, http.StatusInternalServerError, errorBody("ingest error"))
	default:
		logger.WithError(err).WithField("document_id", docID).Error("unexpected pipeline error")
		writeJSON(w, http.StatusInternalServerError, errorBody("ingest error"))
	}
}

// ingestRaw is the normalized output of parseMultipart / parseJSON. It
// holds everything the persist + chunk step needs without re-coupling
// the route to the two request shapes.
type ingestRaw struct {
	id            uuid.UUID
	title         string
	contentType   string
	body          []byte
	metadata      map[string]any
	collectionIDs []uuid.UUID
	ownerID       *uuid.UUID
	documentType  string
}

func parseMultipart(req *http.Request) (ingestRaw, error) {
	if err := req.ParseMultipartForm(maxIngestBytes); err != nil {
		return ingestRaw{}, fmt.Errorf("parse multipart: %w", err)
	}
	file, hdr, err := req.FormFile("file")
	if err != nil {
		return ingestRaw{}, fmt.Errorf("file field missing: %w", err)
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, maxIngestBytes+1))
	if err != nil {
		return ingestRaw{}, fmt.Errorf("read file: %w", err)
	}

	in := ingestRaw{
		body:        body,
		contentType: hdr.Header.Get("Content-Type"),
		title:       hdr.Filename,
		metadata:    map[string]any{},
	}
	if v := req.FormValue("id"); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			return ingestRaw{}, fmt.Errorf("invalid id: %w", err)
		}
		in.id = id
	} else {
		in.id = uuid.New()
	}
	if v := req.FormValue("metadata"); v != "" {
		if err := json.Unmarshal([]byte(v), &in.metadata); err != nil {
			return ingestRaw{}, fmt.Errorf("metadata not valid json: %w", err)
		}
	}
	if v := req.FormValue("collection_ids"); v != "" {
		var ids []string
		if err := json.Unmarshal([]byte(v), &ids); err != nil {
			return ingestRaw{}, fmt.Errorf("collection_ids not valid json array: %w", err)
		}
		for _, s := range ids {
			id, err := uuid.Parse(s)
			if err != nil {
				return ingestRaw{}, fmt.Errorf("invalid collection_id %q: %w", s, err)
			}
			in.collectionIDs = append(in.collectionIDs, id)
		}
	}
	return in, nil
}

func parseJSON(req *http.Request) (ingestRaw, error) {
	var body struct {
		RawText       string         `json:"raw_text"`
		ID            string         `json:"id"`
		Title         string         `json:"title"`
		Metadata      map[string]any `json:"metadata"`
		CollectionIDs []string       `json:"collection_ids"`
		DocumentType  string         `json:"document_type"`
	}
	if err := json.NewDecoder(io.LimitReader(req.Body, maxIngestBytes+1)).Decode(&body); err != nil {
		return ingestRaw{}, fmt.Errorf("decode json: %w", err)
	}
	if body.RawText == "" {
		return ingestRaw{}, fmt.Errorf("raw_text is required for application/json ingests")
	}
	in := ingestRaw{
		body:         []byte(body.RawText),
		contentType:  "text/plain",
		title:        body.Title,
		metadata:     body.Metadata,
		documentType: body.DocumentType,
	}
	if in.metadata == nil {
		in.metadata = map[string]any{}
	}
	if body.ID != "" {
		id, err := uuid.Parse(body.ID)
		if err != nil {
			return ingestRaw{}, fmt.Errorf("invalid id: %w", err)
		}
		in.id = id
	} else {
		in.id = uuid.New()
	}
	for _, s := range body.CollectionIDs {
		id, err := uuid.Parse(s)
		if err != nil {
			return ingestRaw{}, fmt.Errorf("invalid collection_id %q: %w", s, err)
		}
		in.collectionIDs = append(in.collectionIDs, id)
	}
	return in, nil
}
