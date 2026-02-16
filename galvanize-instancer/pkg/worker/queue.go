package worker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

const (
	// Redis key for the job queue
	jobQueueKey = "galvanize:jobs"
	// Redis key for in-progress jobs (for reliability)
	processingKey = "galvanize:processing"
)

// Queue manages the Redis-based job queue
type Queue struct {
	client *redis.Client
	logger *zap.SugaredLogger
}

// QueueConfig holds Redis connection settings
type QueueConfig struct {
	Addr     string
	Password string
	DB       int
}

// NewQueue creates a new Redis-backed job queue
func NewQueue(cfg QueueConfig, logger *zap.SugaredLogger) (*Queue, error) {
	client := redis.NewClient(&redis.Options{
		Addr:     cfg.Addr,
		Password: cfg.Password,
		DB:       cfg.DB,
	})

	// Test connection
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("failed to connect to Redis: %w", err)
	}

	logger.Infof("Connected to Redis at %s", cfg.Addr)

	return &Queue{
		client: client,
		logger: logger,
	}, nil
}

// Enqueue adds a job to the queue
func (q *Queue) Enqueue(ctx context.Context, job *Job) error {
	data, err := job.Marshal()
	if err != nil {
		return fmt.Errorf("failed to marshal job: %w", err)
	}

	// Use LPUSH to add to the left (head) of the list
	// Workers will BRPOPLPUSH from the right (tail) for FIFO ordering
	if err := q.client.LPush(ctx, jobQueueKey, data).Err(); err != nil {
		return fmt.Errorf("failed to enqueue job: %w", err)
	}

	q.logger.Debugf("Enqueued job: %s", job.ID)
	return nil
}

// Dequeue blocks until a job is available, then returns it
// The job is moved to a processing list for reliability
func (q *Queue) Dequeue(ctx context.Context, workerID string) (*Job, error) {
	// BRPOPLPUSH: atomically pop from queue and push to processing list
	// This ensures jobs aren't lost if a worker crashes
	processingListKey := processingKey + ":" + workerID

	// Use a short timeout (1 second) so we can check context cancellation
	// The caller should loop and retry if context.DeadlineExceeded is returned
	result, err := q.client.BRPopLPush(ctx, jobQueueKey, processingListKey, 1*time.Second).Result()
	if err != nil {
		// redis.Nil is returned when timeout expires with no data
		if errors.Is(err, redis.Nil) {
			return nil, context.DeadlineExceeded
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, fmt.Errorf("failed to dequeue job: %w", err)
	}

	job, err := UnmarshalJob([]byte(result))
	if err != nil {
		// Remove malformed job from processing list
		q.client.LRem(ctx, processingListKey, 1, result)
		return nil, fmt.Errorf("failed to unmarshal job: %w", err)
	}

	return job, nil
}

// Complete marks a job as completed and removes it from the processing list
func (q *Queue) Complete(ctx context.Context, workerID string, job *Job) error {
	processingListKey := processingKey + ":" + workerID

	data, err := job.Marshal()
	if err != nil {
		return fmt.Errorf("failed to marshal job for removal: %w", err)
	}

	if err := q.client.LRem(ctx, processingListKey, 1, data).Err(); err != nil {
		return fmt.Errorf("failed to remove job from processing list: %w", err)
	}

	q.logger.Debugf("Completed job: %s", job.ID)
	return nil
}

// Requeue moves a job back to the main queue (for retries)
func (q *Queue) Requeue(ctx context.Context, workerID string, job *Job) error {
	processingListKey := processingKey + ":" + workerID

	// First remove from processing list
	data, err := job.Marshal()
	if err != nil {
		return fmt.Errorf("failed to marshal job: %w", err)
	}

	if err := q.client.LRem(ctx, processingListKey, 1, data).Err(); err != nil {
		q.logger.Warnf("Failed to remove job from processing list during requeue: %v", err)
	}

	// Increment retry count and re-enqueue
	job.Retries++
	return q.Enqueue(ctx, job)
}

// Fail marks a job as failed and removes it from processing (no retry)
func (q *Queue) Fail(ctx context.Context, workerID string, job *Job) error {
	return q.Complete(ctx, workerID, job) // Just remove it
}

// QueueLength returns the number of jobs waiting in the queue
func (q *Queue) QueueLength(ctx context.Context) (int64, error) {
	return q.client.LLen(ctx, jobQueueKey).Result()
}

// Close closes the Redis connection
func (q *Queue) Close() error {
	return q.client.Close()
}
