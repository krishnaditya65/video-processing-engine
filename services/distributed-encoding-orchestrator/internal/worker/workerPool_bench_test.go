package worker

import (
	"context"
	"testing"
	"time"

	"distributed-encoding-orchestrator/internal/domain"
)

// BenchmarkWorkerPool_DispatchThroughput measures end-to-end task
// dispatch throughput (tasks/sec) for a trivial (near-zero-cost)
// processor across a range of concurrency bounds, giving a rough sense
// of the WorkerPool's own scheduling overhead independent of actual
// ffmpeg encode time. Real-world throughput is dominated by ffmpeg
// encode wall-clock time, not this overhead — see the README's
// throughput-math section.
func BenchmarkWorkerPool_DispatchThroughput(b *testing.B) {
	for _, concurrency := range []int{8, 64, 512} {
		b.Run(concurrencyLabel(concurrency), func(b *testing.B) {
			processor := ChunkEncodingTaskProcessorFunc(func(ctx context.Context, task domain.ChunkEncodingTask) error {
				return nil
			})
			pool := NewWorkerPool(processor, WorkerPoolConfiguration{MaxConcurrentTasks: concurrency})

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				queue := domain.NewJobQueue(1000)
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				tasks := make([]domain.ChunkEncodingTask, 1000)
				for j := range tasks {
					tasks[j] = buildFixtureTask(taskIDFor(j))
				}
				go func() {
					for _, task := range tasks {
						_ = queue.Enqueue(ctx, task)
					}
					queue.Close()
				}()
				b.StartTimer()

				outcomes := pool.RunUntilQueueDrainedOrContextCancelled(ctx, queue)
				for range outcomes {
				}
				cancel()
			}
		})
	}
}

func concurrencyLabel(n int) string {
	return "concurrency-" + itoa(n)
}
