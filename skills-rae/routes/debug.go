package routes

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sirupsen/logrus"
)

// Debug endpoints feed the SPA's Skills page (Subunits + Communities
// tabs). They're read-only and unauth'd within the trust boundary
// (Pi-local + Caddy bearer); not exposed publicly.
//
//   GET /v1/rae/communities                  -> []CommunityRow
//   GET /v1/rae/skill/{id}/subunits         -> []SubunitRow

type CommunityRow struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	MemberCount int    `json:"member_count"`
	ComputedAt  string `json:"computed_at"`
}

type SubunitRow struct {
	ID        string `json:"id"`
	Content   string `json:"content"`
	Kind      string `json:"kind"`
	Length    int    `json:"length"`
	IDF       float64 `json:"idf"`
	LineNo    int    `json:"line_no"`
	EdgeCount int    `json:"edge_count"`
}

// CommunitySkillRow is the per-member shape returned by the
// "drill into community" endpoint. Stays narrow so the dialog can
// list a few hundred rows without shipping the full Skill record.
type CommunitySkillRow struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Description   string `json:"description"`
	Origin        string `json:"origin"`
	DriftDetected bool   `json:"drift_detected"`
	RAEIndexedAt  string `json:"rae_indexed_at,omitempty"`
}

func RegisterDebug(r *mux.Router, pool *pgxpool.Pool, logger *logrus.Logger) {
	r.HandleFunc("/v1/rae/communities", func(w http.ResponseWriter, req *http.Request) {
		ctx, cancel := context.WithTimeout(req.Context(), 5*time.Second)
		defer cancel()
		rows, err := pool.Query(ctx, `
			SELECT id, label, member_count, computed_at
			  FROM evo.skill_communities
			 ORDER BY member_count DESC, label`)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		defer rows.Close()
		out := []CommunityRow{}
		for rows.Next() {
			var c CommunityRow
			var t time.Time
			if err := rows.Scan(&c.ID, &c.Label, &c.MemberCount, &t); err != nil {
				writeErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			c.ComputedAt = t.UTC().Format(time.RFC3339)
			out = append(out, c)
		}
		writeJSON(w, http.StatusOK, out)
	}).Methods(http.MethodGet)

	r.HandleFunc("/v1/rae/community/{id}/skills", func(w http.ResponseWriter, req *http.Request) {
		raw := mux.Vars(req)["id"]
		commID, err := uuid.Parse(raw)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid community id")
			return
		}
		ctx, cancel := context.WithTimeout(req.Context(), 5*time.Second)
		defer cancel()
		rows, err := pool.Query(ctx, `
			SELECT id, name, description, origin, drift_detected, rae_indexed_at
			  FROM evo.skills
			 WHERE community_id = $1
			 ORDER BY name`, commID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		defer rows.Close()
		out := []CommunitySkillRow{}
		for rows.Next() {
			var r CommunitySkillRow
			var indexed *time.Time
			if err := rows.Scan(&r.ID, &r.Name, &r.Description, &r.Origin, &r.DriftDetected, &indexed); err != nil {
				writeErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			if indexed != nil {
				r.RAEIndexedAt = indexed.UTC().Format(time.RFC3339)
			}
			out = append(out, r)
		}
		writeJSON(w, http.StatusOK, out)
	}).Methods(http.MethodGet)

	r.HandleFunc("/v1/rae/skill/{id}/subunits", func(w http.ResponseWriter, req *http.Request) {
		raw := mux.Vars(req)["id"]
		skillID, err := uuid.Parse(raw)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid skill id")
			return
		}
		ctx, cancel := context.WithTimeout(req.Context(), 5*time.Second)
		defer cancel()
		rows, err := pool.Query(ctx, `
			SELECT u.id, u.content, u.kind, u.length, u.idf, e.line_no,
			       (SELECT COUNT(*) FROM evo.skill_subunit_edges WHERE subunit_id = u.id)
			  FROM evo.skill_subunit_edges e
			  JOIN evo.skill_subunits u ON u.id = e.subunit_id
			 WHERE e.skill_id = $1
			 ORDER BY u.idf DESC, u.length ASC`, skillID)
		if err != nil {
			logger.WithError(err).Error("subunits query failed")
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		defer rows.Close()
		out := []SubunitRow{}
		for rows.Next() {
			var s SubunitRow
			var line *int
			if err := rows.Scan(&s.ID, &s.Content, &s.Kind, &s.Length, &s.IDF, &line, &s.EdgeCount); err != nil {
				writeErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			if line != nil {
				s.LineNo = *line
			}
			out = append(out, s)
		}
		_ = json.NewEncoder
		writeJSON(w, http.StatusOK, out)
	}).Methods(http.MethodGet)
}
