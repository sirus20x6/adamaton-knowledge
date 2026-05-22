package activities

import (
	"context"
	"errors"
	"testing"

	"go.temporal.io/sdk/temporal"
)

// ChunkEmbedPersist must reject bad input non-retryably so Temporal doesn't
// burn its retry budget on inputs that can never succeed. These guard the
// validation paths that return before any network/DB work.

func TestChunkEmbedPersist_BadUUID(t *testing.T) {
	d := &EmbedDeps{}
	_, err := d.ChunkEmbedPersist(context.Background(), ChunkEmbedPersistInput{
		DocumentID: "not-a-uuid",
		Text:       "some text",
	})
	requireNonRetryable(t, err, "BadInput")
}

func TestChunkEmbedPersist_NoEmbedder(t *testing.T) {
	// Valid UUID, but no embedding sidecar configured (Embedder nil).
	d := &EmbedDeps{}
	_, err := d.ChunkEmbedPersist(context.Background(), ChunkEmbedPersistInput{
		DocumentID: "11111111-1111-1111-1111-111111111111",
		Text:       "some text",
	})
	requireNonRetryable(t, err, "NoEmbedder")
}

func requireNonRetryable(t *testing.T, err error, wantType string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var appErr *temporal.ApplicationError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *temporal.ApplicationError, got %T: %v", err, err)
	}
	if !appErr.NonRetryable() {
		t.Fatalf("expected non-retryable error, got retryable: %v", err)
	}
	if appErr.Type() != wantType {
		t.Fatalf("expected error type %q, got %q", wantType, appErr.Type())
	}
}
