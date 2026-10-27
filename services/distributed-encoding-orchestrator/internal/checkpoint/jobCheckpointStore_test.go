package checkpoint

import (
	"context"
	"testing"
)

// TestInMemoryJobCheckpointStore_RecordAndQueryRoundTrip verifies the
// basic record/query contract in isolation from the Dispatcher.
func TestInMemoryJobCheckpointStore_RecordAndQueryRoundTrip(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryJobCheckpointStore()

	if completed, err := store.IsChunkCompleted(ctx, "job-1", "chunk-a"); err != nil || completed {
		t.Fatalf("expected chunk-a not completed before recording, got completed=%v err=%v", completed, err)
	}

	if err := store.RecordChunkCompleted(ctx, "job-1", "chunk-a", "/out/chunk-a.ts"); err != nil {
		t.Fatalf("RecordChunkCompleted returned error: %v", err)
	}

	completed, err := store.IsChunkCompleted(ctx, "job-1", "chunk-a")
	if err != nil {
		t.Fatalf("IsChunkCompleted returned error: %v", err)
	}
	if !completed {
		t.Fatal("expected chunk-a to be completed after recording")
	}

	// A different job must not see job-1's completions.
	if completed, err := store.IsChunkCompleted(ctx, "job-2", "chunk-a"); err != nil || completed {
		t.Fatalf("expected job-2/chunk-a not completed (isolated namespace), got completed=%v err=%v", completed, err)
	}
}

// TestInMemoryJobCheckpointStore_ListCompletedChunksReturnsAllRecordsForJob
// verifies ListCompletedChunks returns every recorded chunk for a job and
// none from other jobs.
func TestInMemoryJobCheckpointStore_ListCompletedChunksReturnsAllRecordsForJob(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryJobCheckpointStore()

	_ = store.RecordChunkCompleted(ctx, "job-1", "chunk-a", "/out/a.ts")
	_ = store.RecordChunkCompleted(ctx, "job-1", "chunk-b", "/out/b.ts")
	_ = store.RecordChunkCompleted(ctx, "job-2", "chunk-c", "/out/c.ts")

	records, err := store.ListCompletedChunks(ctx, "job-1")
	if err != nil {
		t.Fatalf("ListCompletedChunks returned error: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("expected 2 records for job-1, got %d: %#v", len(records), records)
	}
	if records["chunk-a"].OutputChunkFilePath != "/out/a.ts" {
		t.Fatalf("unexpected output path for chunk-a: %#v", records["chunk-a"])
	}
	if records["chunk-b"].OutputChunkFilePath != "/out/b.ts" {
		t.Fatalf("unexpected output path for chunk-b: %#v", records["chunk-b"])
	}

	// Mutating the returned map must not affect the store's internal state
	// (defensive copy contract).
	delete(records, "chunk-a")
	stillThere, _ := store.IsChunkCompleted(ctx, "job-1", "chunk-a")
	if !stillThere {
		t.Fatal("expected mutating the returned map to not affect the store's internal state")
	}
}

// TestInMemoryJobCheckpointStore_ListCompletedChunksForUnknownJobReturnsEmptyNonNilMap
// verifies the documented empty-but-non-nil contract for an unknown job.
func TestInMemoryJobCheckpointStore_ListCompletedChunksForUnknownJobReturnsEmptyNonNilMap(t *testing.T) {
	store := NewInMemoryJobCheckpointStore()
	records, err := store.ListCompletedChunks(context.Background(), "no-such-job")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if records == nil {
		t.Fatal("expected a non-nil empty map")
	}
	if len(records) != 0 {
		t.Fatalf("expected 0 records, got %d", len(records))
	}
}

// TestInMemoryJobCheckpointStore_ClearJobRemovesAllRecordsForThatJobOnly
// verifies ClearJob scopes deletion to exactly the requested job.
func TestInMemoryJobCheckpointStore_ClearJobRemovesAllRecordsForThatJobOnly(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryJobCheckpointStore()

	_ = store.RecordChunkCompleted(ctx, "job-1", "chunk-a", "/out/a.ts")
	_ = store.RecordChunkCompleted(ctx, "job-2", "chunk-b", "/out/b.ts")

	if err := store.ClearJob(ctx, "job-1"); err != nil {
		t.Fatalf("ClearJob returned error: %v", err)
	}

	if completed, _ := store.IsChunkCompleted(ctx, "job-1", "chunk-a"); completed {
		t.Fatal("expected job-1/chunk-a to be cleared")
	}
	if completed, _ := store.IsChunkCompleted(ctx, "job-2", "chunk-b"); !completed {
		t.Fatal("expected job-2/chunk-b to be unaffected by clearing job-1")
	}
}

// TestInMemoryJobCheckpointStore_RecordChunkCompletedIsIdempotent verifies
// that recording the same chunk twice (e.g. an at-least-once-delivery
// duplicate completion notification) simply overwrites rather than erroring
// or creating a duplicate entry.
func TestInMemoryJobCheckpointStore_RecordChunkCompletedIsIdempotent(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryJobCheckpointStore()

	_ = store.RecordChunkCompleted(ctx, "job-1", "chunk-a", "/out/a-v1.ts")
	_ = store.RecordChunkCompleted(ctx, "job-1", "chunk-a", "/out/a-v2.ts")

	records, _ := store.ListCompletedChunks(ctx, "job-1")
	if len(records) != 1 {
		t.Fatalf("expected exactly 1 record after duplicate completion, got %d", len(records))
	}
	if records["chunk-a"].OutputChunkFilePath != "/out/a-v2.ts" {
		t.Fatalf("expected the later record to win, got %#v", records["chunk-a"])
	}
}
