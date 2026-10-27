// Package checkpoint implements checkpoint-resume support for
// preemptible workers: a JobCheckpointStore records, durably (from the
// orchestrator's point of view — the in-memory implementation here is
// durable only for the lifetime of the process, but the interface is
// deliberately shaped so a real deployment could back it with Redis,
// DynamoDB, or a local WAL file without any caller-visible change), which
// individual ChunkEncodingTask units of an EncodingJob have already
// completed successfully.
//
// # Why this exists
//
// A fleet-scale encoding orchestrator typically runs its worker fleet on
// preemptible/spot compute to control cost (see internal/cost for the
// accounting side of that trade-off). A spot node can be reclaimed by the
// cloud provider at any moment, mid-job. Without checkpointing, losing a
// node mid-job would force the whole EncodingJob — potentially thousands
// of ChunkEncodingTask units — to restart from scratch. With
// checkpointing, worker.Dispatcher consults the JobCheckpointStore before
// (re-)dispatching a job's tasks and skips any chunk already recorded as
// completed, so only the chunks that were genuinely in flight (or not yet
// started) at the moment of preemption need to be (re)encoded.
//
// # Design pattern
//
// JobCheckpointStore is a small Repository-style interface (a documented
// variant of the Strategy pattern already used elsewhere in this
// codebase for ffmpeg.CodecArgumentBuilder and ffmpeg.SubprocessRunner):
// worker.Dispatcher depends only on the interface, and this package
// supplies InMemoryJobCheckpointStore as the concrete implementation used
// both in production (single-process deployments) and in tests. A real
// multi-node deployment would supply a different implementation (backed
// by shared storage reachable from every worker node) satisfying the same
// interface, with zero changes to Dispatcher.
package checkpoint

import (
	"context"
	"sync"
	"time"
)

// ChunkCompletionRecord captures that one chunk-encoding task of one
// EncodingJob has completed successfully, including enough detail for an
// operator (or the Dispatcher, on resume) to know where its output landed
// and when it finished.
type ChunkCompletionRecord struct {
	// JobIdentifier is the parent EncodingJob's JobIdentifier.
	JobIdentifier string
	// ChunkIdentifier is the completed task's
	// domain.ChunkEncodingTask.TaskIdentifier — the natural primary key
	// for resume lookups, since it is deterministically derived from
	// (jobIdentifier, sceneIndex, chunkIndexWithinScene, renditionLabel)
	// by domain.FormatChunkTaskIdentifier and is therefore identical
	// across a job's first attempt and any later resume attempt.
	ChunkIdentifier string
	// OutputChunkFilePath is where the completed chunk's encoded output
	// was written, so a resumed job can be assembled/verified without
	// re-deriving the path.
	OutputChunkFilePath string
	// CompletedAt is when the completion was recorded.
	CompletedAt time.Time
}

// JobCheckpointStore persists per-chunk completion state for
// preemptible-worker resume support. Implementations must be safe for
// concurrent use: worker.Dispatcher's outcome-processing loop and any
// number of concurrently-dispatching jobs may call these methods from
// different goroutines at once.
type JobCheckpointStore interface {
	// RecordChunkCompleted durably records that chunkIdentifier (belonging
	// to jobIdentifier) has completed successfully, with its output
	// written to outputChunkFilePath. Calling it more than once for the
	// same (jobIdentifier, chunkIdentifier) pair is idempotent — the
	// later call's outputChunkFilePath/timestamp simply overwrites the
	// earlier one, which matters because a task's final retry attempt
	// might legitimately be recorded twice under at-least-once delivery
	// semantics in a real distributed implementation.
	RecordChunkCompleted(ctx context.Context, jobIdentifier string, chunkIdentifier string, outputChunkFilePath string) error

	// IsChunkCompleted reports whether chunkIdentifier has already been
	// recorded as completed for jobIdentifier.
	IsChunkCompleted(ctx context.Context, jobIdentifier string, chunkIdentifier string) (bool, error)

	// ListCompletedChunks returns every ChunkCompletionRecord recorded so
	// far for jobIdentifier, keyed by ChunkIdentifier, so a resuming
	// Dispatcher can filter its freshly fanned-out task list down to only
	// the tasks that still need to run. Returns an empty (non-nil) map if
	// no chunks have been recorded for jobIdentifier.
	ListCompletedChunks(ctx context.Context, jobIdentifier string) (map[string]ChunkCompletionRecord, error)

	// ClearJob deletes all recorded completions for jobIdentifier, e.g.
	// once a job finishes entirely and its checkpoint state is no longer
	// needed, or if an operator wants to force a full re-encode.
	ClearJob(ctx context.Context, jobIdentifier string) error
}

// InMemoryJobCheckpointStore is the in-process JobCheckpointStore
// implementation: a mutex-guarded nested map (jobIdentifier ->
// chunkIdentifier -> ChunkCompletionRecord). It is durable only for the
// lifetime of the owning process — a real preemptible-worker deployment
// spanning multiple orchestrator processes/nodes would back
// JobCheckpointStore with shared storage instead — but it is sufficient
// for a single-process orchestrator instance and is what this package's
// tests use to prove checkpoint-resume correctness end to end.
type InMemoryJobCheckpointStore struct {
	mutex sync.RWMutex
	// completedChunksByJob maps jobIdentifier -> chunkIdentifier ->
	// ChunkCompletionRecord.
	completedChunksByJob map[string]map[string]ChunkCompletionRecord
}

// NewInMemoryJobCheckpointStore constructs an empty InMemoryJobCheckpointStore.
func NewInMemoryJobCheckpointStore() *InMemoryJobCheckpointStore {
	return &InMemoryJobCheckpointStore{
		completedChunksByJob: make(map[string]map[string]ChunkCompletionRecord),
	}
}

// RecordChunkCompleted implements JobCheckpointStore.
func (store *InMemoryJobCheckpointStore) RecordChunkCompleted(_ context.Context, jobIdentifier string, chunkIdentifier string, outputChunkFilePath string) error {
	store.mutex.Lock()
	defer store.mutex.Unlock()

	chunkMap, ok := store.completedChunksByJob[jobIdentifier]
	if !ok {
		chunkMap = make(map[string]ChunkCompletionRecord)
		store.completedChunksByJob[jobIdentifier] = chunkMap
	}
	chunkMap[chunkIdentifier] = ChunkCompletionRecord{
		JobIdentifier:       jobIdentifier,
		ChunkIdentifier:     chunkIdentifier,
		OutputChunkFilePath: outputChunkFilePath,
		CompletedAt:         time.Now(),
	}
	return nil
}

// IsChunkCompleted implements JobCheckpointStore.
func (store *InMemoryJobCheckpointStore) IsChunkCompleted(_ context.Context, jobIdentifier string, chunkIdentifier string) (bool, error) {
	store.mutex.RLock()
	defer store.mutex.RUnlock()

	chunkMap, ok := store.completedChunksByJob[jobIdentifier]
	if !ok {
		return false, nil
	}
	_, completed := chunkMap[chunkIdentifier]
	return completed, nil
}

// ListCompletedChunks implements JobCheckpointStore.
func (store *InMemoryJobCheckpointStore) ListCompletedChunks(_ context.Context, jobIdentifier string) (map[string]ChunkCompletionRecord, error) {
	store.mutex.RLock()
	defer store.mutex.RUnlock()

	result := make(map[string]ChunkCompletionRecord)
	for chunkIdentifier, record := range store.completedChunksByJob[jobIdentifier] {
		result[chunkIdentifier] = record
	}
	return result, nil
}

// ClearJob implements JobCheckpointStore.
func (store *InMemoryJobCheckpointStore) ClearJob(_ context.Context, jobIdentifier string) error {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	delete(store.completedChunksByJob, jobIdentifier)
	return nil
}
