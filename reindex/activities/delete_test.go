package activities

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"go.temporal.io/sdk/temporal"
)

// delete_test.go pins DeleteR2RDocument's retry-safety contract — the
// other half of the queue.go:142-147 TODO. The activity backs paper.go's
// best-effort old-doc cleanup, which Temporal may deliver more than once
// (retries on transient platform failures, plus the rewire-no-op cleanup
// path that deletes the *new* doc). So the activity must be safe to call
// repeatedly and must classify HTTP outcomes correctly:
//
//   - 404 → success (the doc is already gone; a retry that races the
//     delete must not fail the workflow).
//   - 2xx → success.
//   - 5xx → a *retryable* error (plain error, NOT NonRetryable) so
//     Temporal retries a flaky platform.
//   - empty doc id → non-retryable BadInput (never burns a retry).

func newDeleteActivities(t *testing.T, h http.HandlerFunc) (*IngestActivities, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &IngestActivities{
		HTTPClient: srv.Client(),
		R2GURL:     srv.URL,
	}, srv
}

func TestDeleteR2RDocument_404IsSuccess(t *testing.T) {
	a, _ := newDeleteActivities(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	if err := a.DeleteR2RDocument(context.Background(), uuid.NewString()); err != nil {
		t.Fatalf("404 should be treated as success, got %v", err)
	}
}

func TestDeleteR2RDocument_2xxIsSuccess(t *testing.T) {
	a, _ := newDeleteActivities(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("method = %s, want DELETE", r.Method)
		}
		w.WriteHeader(http.StatusOK)
	})
	if err := a.DeleteR2RDocument(context.Background(), uuid.NewString()); err != nil {
		t.Fatalf("2xx should be success, got %v", err)
	}
}

func TestDeleteR2RDocument_5xxIsRetryable(t *testing.T) {
	a, _ := newDeleteActivities(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})
	err := a.DeleteR2RDocument(context.Background(), uuid.NewString())
	if err == nil {
		t.Fatal("5xx should error, got nil")
	}
	// Must NOT be a non-retryable application error — Temporal should retry.
	var appErr *temporal.ApplicationError
	if errors.As(err, &appErr) && appErr.NonRetryable() {
		t.Fatalf("5xx error must be retryable, got non-retryable: %v", err)
	}
}

func TestDeleteR2RDocument_EmptyIDIsNonRetryable(t *testing.T) {
	a := &IngestActivities{}
	err := a.DeleteR2RDocument(context.Background(), "")
	var appErr *temporal.ApplicationError
	if !errors.As(err, &appErr) || !appErr.NonRetryable() || appErr.Type() != "BadInput" {
		t.Fatalf("empty id = %v, want non-retryable BadInput", err)
	}
}

// TestDeleteR2RDocument_RepeatedDeleteIdempotent simulates the realistic
// retry sequence: the first DELETE hits an existing doc (200), every
// subsequent DELETE finds it already gone (404). All calls must succeed,
// so a duplicate Temporal delivery never turns a completed cleanup into a
// workflow failure.
func TestDeleteR2RDocument_RepeatedDeleteIdempotent(t *testing.T) {
	var calls int32
	a, _ := newDeleteActivities(t, func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			w.WriteHeader(http.StatusOK) // first delete: doc existed
			return
		}
		w.WriteHeader(http.StatusNotFound) // already gone
	})

	id := uuid.NewString()
	for i := 0; i < 4; i++ {
		if err := a.DeleteR2RDocument(context.Background(), id); err != nil {
			t.Fatalf("repeated delete attempt %d failed: %v", i, err)
		}
	}
	if atomic.LoadInt32(&calls) != 4 {
		t.Fatalf("server saw %d calls, want 4", calls)
	}
}
