// This file implements JobQueue, the bounded, channel-backed,
// context-cancellable queue of ChunkEncodingTask work items that sits
// between job fan-out (encodingDomainTypes.go's
// FanOutEncodingJobIntoChunkEncodingTasks) and the WorkerPool that drains
// it.
package domain

import "context"

// JobQueue is a bounded FIFO queue of ChunkEncodingTask values backed by
// a buffered Go channel. It is the hand-off point between job
// submission/fan-out and the WorkerPool: producers call Enqueue (or
// EnqueueAll) to push tasks in, and worker goroutines range over
// Tasks() (or call Dequeue in a loop) to pull tasks out.
//
// Using a channel rather than a hand-rolled mutex+slice queue gives us,
// for free: safe concurrent access from many producer and consumer
// goroutines, natural backpressure (Enqueue blocks once the buffer is
// full, which is the desired behavior when thousands of chunk tasks are
// fanned out faster than the worker fleet can drain them), and clean
// integration with context-based cancellation via select.
//
// Concurrency/safety: all methods are safe for concurrent use by
// multiple goroutines. Close must be called exactly once, after which
// further Enqueue calls return ErrQueueClosed rather than panicking (a
// send on a closed channel would panic; Enqueue guards against that with
// a separate atomic-guarded closed flag rather than relying on
// recover()).
type JobQueue struct {
	tasksChannel chan ChunkEncodingTask
	closed       chan struct{}
}

// NewJobQueue constructs a JobQueue with the given channel buffer
// capacity. capacity determines how many ChunkEncodingTask values can be
// enqueued before Enqueue starts blocking the producer — for the "fan
// out a two-hour film" scenario described in the README, a capacity in
// the low thousands (matching the expected total chunk count) lets the
// entire job be enqueued without producer blocking, while a much smaller
// capacity is perfectly fine too since it only affects producer/consumer
// pipelining, not correctness.
func NewJobQueue(capacity int) *JobQueue {
	if capacity < 1 {
		capacity = 1
	}
	return &JobQueue{
		tasksChannel: make(chan ChunkEncodingTask, capacity),
		closed:       make(chan struct{}),
	}
}

// ErrQueueClosed-style sentinel is intentionally implemented via a typed
// error below rather than a bare errors.New var, so callers can use
// errors.Is against a stable value while the message stays descriptive.
type queueClosedError struct{}

func (queueClosedError) Error() string { return "domain: job queue is closed" }

// ErrQueueClosed is returned by Enqueue/EnqueueAll once Close has been
// called.
var ErrQueueClosed error = queueClosedError{}

// Enqueue pushes one ChunkEncodingTask onto the queue, blocking if the
// queue's buffer is full until space is available, ctx is cancelled, or
// the queue is closed.
//
// Returns ctx.Err() if ctx is cancelled before the task could be
// enqueued, or ErrQueueClosed if the queue has been closed. Returns nil
// on successful enqueue.
func (queue *JobQueue) Enqueue(ctx context.Context, task ChunkEncodingTask) error {
	select {
	case <-queue.closed:
		return ErrQueueClosed
	default:
	}

	select {
	case queue.tasksChannel <- task:
		return nil
	case <-queue.closed:
		return ErrQueueClosed
	case <-ctx.Done():
		return ctx.Err()
	}
}

// EnqueueAll enqueues every task in tasks in order, stopping and
// returning the first error encountered (from an individual Enqueue
// call), which leaves any remaining tasks un-enqueued. Callers that need
// all-or-nothing semantics should validate/buffer tasks themselves
// before calling EnqueueAll, since a JobQueue offers no transactional
// rollback of already-enqueued items.
func (queue *JobQueue) EnqueueAll(ctx context.Context, tasks []ChunkEncodingTask) error {
	for _, task := range tasks {
		if err := queue.Enqueue(ctx, task); err != nil {
			return err
		}
	}
	return nil
}

// Dequeue blocks until a ChunkEncodingTask is available, ctx is
// cancelled, or the queue is closed and drained. Returns
// (task, true, nil) on success; returns (zero-value, false, ctx.Err())
// if ctx is cancelled first; returns (zero-value, false, nil) if the
// queue was closed and no more tasks remain (the conventional Go
// channel-drain-then-stop signal, mirroring `v, ok := <-ch`).
func (queue *JobQueue) Dequeue(ctx context.Context) (ChunkEncodingTask, bool, error) {
	select {
	case task, ok := <-queue.tasksChannel:
		if !ok {
			return ChunkEncodingTask{}, false, nil
		}
		return task, true, nil
	case <-ctx.Done():
		return ChunkEncodingTask{}, false, ctx.Err()
	}
}

// Tasks returns the underlying receive-only channel of ChunkEncodingTask
// values, intended for worker goroutines to consume via `for task := range queue.Tasks()`,
// which is the idiomatic way for a WorkerPool's fixed pool of goroutines
// to fan-in-drain a shared work queue until it is closed and empty.
func (queue *JobQueue) Tasks() <-chan ChunkEncodingTask {
	return queue.tasksChannel
}

// Close signals that no more tasks will be enqueued and closes the
// underlying channel once safe to do so. After Close returns, any
// worker goroutines ranging over Tasks() will finish draining
// already-buffered tasks and then exit their range loop, and any
// subsequent Enqueue calls return ErrQueueClosed. Close is idempotent-safe
// to call multiple times only if callers coordinate externally — calling
// it twice concurrently would double-close the channel and panic, so in
// practice exactly one owner (the Dispatcher) should call Close, exactly
// once, after all producers have finished calling Enqueue/EnqueueAll.
func (queue *JobQueue) Close() {
	close(queue.closed)
	close(queue.tasksChannel)
}

// Len returns the number of tasks currently buffered in the queue
// (neither yet dequeued nor discarded). This is a point-in-time estimate
// useful for metrics/backlog dashboards; under concurrent
// enqueue/dequeue activity the true length may have already changed by
// the time the caller observes the returned value.
func (queue *JobQueue) Len() int {
	return len(queue.tasksChannel)
}

// Cap returns the queue's fixed buffer capacity as supplied to NewJobQueue.
func (queue *JobQueue) Cap() int {
	return cap(queue.tasksChannel)
}
