// Package worker executes the background job queue: dispatch notifications,
// incident escalation, permit settlement, overdue sweeps and session expiry.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/vance1852/trail-permit-dispatch/internal/apperr"
	"github.com/vance1852/trail-permit-dispatch/internal/clock"
	"github.com/vance1852/trail-permit-dispatch/internal/domain"
	"github.com/vance1852/trail-permit-dispatch/internal/repository"
)

// Handler executes one job. Returning nil marks the job as done.
type Handler func(ctx context.Context, job domain.Job) error

// FailureHook is invoked once a job failed permanently, so a business object can
// be parked in a consistent state instead of silently staying pending.
type FailureHook func(ctx context.Context, job domain.Job, cause error)

// Options carries the tunable parameters of the runner.
type Options struct {
	WorkerCount  int
	PollInterval time.Duration
	Lease        time.Duration
	JobTimeout   time.Duration
	BackoffBase  time.Duration
	BackoffMax   time.Duration
	BatchSize    int
}

// withDefaults fills unset options with safe values.
func (o Options) withDefaults() Options {
	if o.WorkerCount <= 0 {
		o.WorkerCount = 2
	}
	if o.PollInterval <= 0 {
		o.PollInterval = 500 * time.Millisecond
	}
	if o.Lease <= 0 {
		o.Lease = 30 * time.Second
	}
	if o.JobTimeout <= 0 {
		o.JobTimeout = 20 * time.Second
	}
	if o.BackoffBase <= 0 {
		o.BackoffBase = 2 * time.Second
	}
	if o.BackoffMax < o.BackoffBase {
		o.BackoffMax = 20 * o.BackoffBase
	}
	if o.BatchSize <= 0 {
		o.BatchSize = 4
	}
	return o
}

// Deps carries the collaborators of the runner.
type Deps struct {
	Jobs    repository.JobRepository
	Clock   clock.Clock
	Logger  *slog.Logger
	Options Options
}

// Runner leases and executes jobs until it is stopped.
type Runner struct {
	jobs     repository.JobRepository
	clock    clock.Clock
	logger   *slog.Logger
	options  Options
	handlers map[domain.JobKind]Handler
	hooks    map[domain.JobKind]FailureHook

	mu      sync.Mutex
	cancel  context.CancelFunc
	wait    sync.WaitGroup
	running bool
}

// New builds a job runner.
func New(deps Deps) *Runner {
	clk := deps.Clock
	if clk == nil {
		clk = clock.System{}
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Runner{
		jobs:     deps.Jobs,
		clock:    clk,
		logger:   logger,
		options:  deps.Options.withDefaults(),
		handlers: make(map[domain.JobKind]Handler, 5),
		hooks:    make(map[domain.JobKind]FailureHook, 2),
	}
}

// Register binds a handler to a job kind.
func (r *Runner) Register(kind domain.JobKind, handler Handler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handlers[kind] = handler
}

// RegisterFailureHook binds a permanent failure hook to a job kind.
func (r *Runner) RegisterFailureHook(kind domain.JobKind, hook FailureHook) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hooks[kind] = hook
}

// Start reclaims leases left behind by a previous process and then launches the
// worker goroutines. Calling Start twice is rejected.
func (r *Runner) Start(ctx context.Context) error {
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return apperr.New(apperr.CodeStateInvalid, "后台执行器已经在运行")
	}
	if len(r.handlers) == 0 {
		r.mu.Unlock()
		return apperr.New(apperr.CodeInternal, "尚未注册任何后台作业处理器")
	}
	r.running = true
	runCtx, cancel := context.WithCancel(ctx)
	r.cancel = cancel
	count := r.options.WorkerCount
	r.mu.Unlock()

	reclaimed, err := r.jobs.ReclaimExpiredLeases(runCtx, r.clock.Now())
	if err != nil {
		cancel()
		r.mu.Lock()
		r.running = false
		r.mu.Unlock()
		return err
	}
	if reclaimed > 0 {
		r.logger.Info("重启后回收过期作业租约", slog.Int("reclaimed", reclaimed))
	}

	for i := 0; i < count; i++ {
		workerID := fmt.Sprintf("worker-%d", i+1)
		r.wait.Add(1)
		go func() {
			defer r.wait.Done()
			r.loop(runCtx, workerID)
		}()
	}
	return nil
}

// Stop cancels the workers and waits for the in flight jobs to return.
func (r *Runner) Stop() {
	r.mu.Lock()
	cancel := r.cancel
	r.cancel = nil
	wasRunning := r.running
	r.running = false
	r.mu.Unlock()
	if !wasRunning {
		return
	}
	if cancel != nil {
		cancel()
	}
	r.wait.Wait()
}

// loop polls the queue until the context is cancelled.
func (r *Runner) loop(ctx context.Context, workerID string) {
	ticker := time.NewTicker(r.options.PollInterval)
	defer ticker.Stop()
	for {
		processed, err := r.Poll(ctx, workerID)
		switch {
		case err != nil && ctx.Err() == nil:
			r.logger.Error("后台作业轮询失败", slog.String("worker", workerID), slog.String("error", err.Error()))
		case processed > 0:
			// Keep draining while work is available.
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Poll claims and executes at most one batch of due jobs.
func (r *Runner) Poll(ctx context.Context, workerID string) (int, error) {
	if ctx.Err() != nil {
		return 0, nil
	}
	if _, err := r.jobs.ReclaimExpiredLeases(ctx, r.clock.Now()); err != nil {
		return 0, err
	}
	claimed, err := r.jobs.ClaimDue(ctx, workerID, r.options.Lease, r.clock.Now(), r.options.BatchSize)
	if err != nil {
		return 0, err
	}
	for i := range claimed {
		r.execute(ctx, claimed[i])
	}
	return len(claimed), nil
}

// execute runs one job under its own timeout and records the outcome.
func (r *Runner) execute(ctx context.Context, job domain.Job) {
	r.mu.Lock()
	handler := r.handlers[job.Kind]
	hook := r.hooks[job.Kind]
	r.mu.Unlock()

	if handler == nil {
		r.fail(ctx, job, nil, apperr.Newf(apperr.CodeInternal, "作业类型 %s 没有处理器", job.Kind), true)
		return
	}
	jobCtx, cancel := context.WithTimeout(ctx, r.options.JobTimeout)
	defer cancel()

	err := runHandler(jobCtx, handler, job)
	if err == nil {
		if markErr := r.jobs.MarkDone(context.WithoutCancel(ctx), job.ID, r.clock.Now()); markErr != nil {
			r.logger.Error("标记作业完成失败",
				slog.Int64("job_id", job.ID), slog.String("error", markErr.Error()))
		}
		return
	}
	permanent := job.Attempts >= job.MaxAttempts || terminal(err)
	r.fail(ctx, job, hook, err, permanent)
}

// runHandler isolates a handler panic so one bad job cannot take the process down.
func runHandler(ctx context.Context, handler Handler, job domain.Job) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = apperr.Newf(apperr.CodeInternal, "作业处理器发生 panic: %v", recovered)
		}
	}()
	return handler(ctx, job)
}

// fail records a failed attempt, either parking the job or scheduling a retry.
func (r *Runner) fail(ctx context.Context, job domain.Job, hook FailureHook, cause error, permanent bool) {
	now := r.clock.Now()
	retryAt := now.Add(domain.Backoff(job.Attempts, r.options.BackoffBase, r.options.BackoffMax))
	bookkeeping := context.WithoutCancel(ctx)
	if err := r.jobs.MarkFailure(bookkeeping, repository.JobFailure{
		JobID:     job.ID,
		Message:   cause.Error(),
		Permanent: permanent,
		RetryAt:   retryAt,
		Now:       now,
	}); err != nil {
		r.logger.Error("记录作业失败状态失败",
			slog.Int64("job_id", job.ID), slog.String("error", err.Error()))
		return
	}
	attrs := []any{
		slog.Int64("job_id", job.ID),
		slog.String("kind", string(job.Kind)),
		slog.Int("attempts", job.Attempts),
		slog.Bool("permanent", permanent),
		slog.String("error", cause.Error()),
	}
	if permanent {
		r.logger.Error("后台作业永久失败", attrs...)
		if hook != nil {
			hook(bookkeeping, job, cause)
		}
		return
	}
	r.logger.Warn("后台作业将重试", append(attrs, slog.Time("retry_at", retryAt))...)
}

// terminal reports whether retrying the job with the same input is pointless.
func terminal(err error) bool {
	switch apperr.CodeOf(err) {
	case apperr.CodeNotFound, apperr.CodeInvalidArgument, apperr.CodePermissionDenied, apperr.CodeStateInvalid:
		return true
	default:
		return errors.Is(err, context.Canceled)
	}
}

// PendingSummary reports the queue depth per job kind for the readiness probe.
func (r *Runner) PendingSummary(ctx context.Context) (map[domain.JobKind]int, error) {
	return r.jobs.PendingKinds(ctx)
}
