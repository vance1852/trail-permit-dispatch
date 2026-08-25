package worker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/vance1852/trail-permit-dispatch/internal/apperr"
	"github.com/vance1852/trail-permit-dispatch/internal/clock"
	"github.com/vance1852/trail-permit-dispatch/internal/domain"
	"github.com/vance1852/trail-permit-dispatch/internal/repository"
)

// fakeJobs is an in-memory job queue used to drive the runner deterministically.
type fakeJobs struct {
	mu       sync.Mutex
	nextID   int64
	jobs     map[int64]*domain.Job
	reclaims int
}

func newFakeJobs() *fakeJobs {
	return &fakeJobs{jobs: make(map[int64]*domain.Job, 4)}
}

func (f *fakeJobs) Enqueue(_ context.Context, job *domain.Job) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	job.ID = f.nextID
	job.State = domain.JobQueued
	stored := *job
	f.jobs[job.ID] = &stored
	return job.ID, nil
}

func (f *fakeJobs) ByID(_ context.Context, id int64) (*domain.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	job, ok := f.jobs[id]
	if !ok {
		return nil, apperr.New(apperr.CodeNotFound, "作业不存在")
	}
	copied := *job
	return &copied, nil
}

func (f *fakeJobs) ClaimDue(_ context.Context, workerID string, lease time.Duration, now time.Time, limit int) ([]domain.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	claimed := make([]domain.Job, 0, limit)
	for _, job := range f.jobs {
		if len(claimed) >= limit {
			break
		}
		if job.State != domain.JobQueued || job.RunAt.After(now) {
			continue
		}
		job.State = domain.JobRunning
		job.Attempts++
		job.LockedBy = workerID
		until := now.Add(lease)
		job.LockedUntil = &until
		claimed = append(claimed, *job)
	}
	return claimed, nil
}

func (f *fakeJobs) MarkDone(_ context.Context, jobID int64, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	job, ok := f.jobs[jobID]
	if !ok || job.State != domain.JobRunning {
		return apperr.New(apperr.CodeStateInvalid, "作业不在执行状态")
	}
	job.State = domain.JobDone
	job.LockedBy = ""
	job.LockedUntil = nil
	return nil
}

func (f *fakeJobs) MarkFailure(_ context.Context, failure repository.JobFailure) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	job, ok := f.jobs[failure.JobID]
	if !ok || job.State != domain.JobRunning {
		return apperr.New(apperr.CodeStateInvalid, "作业不在执行状态")
	}
	job.LastError = failure.Message
	job.LockedBy = ""
	job.LockedUntil = nil
	if failure.Permanent {
		job.State = domain.JobFailed
		return nil
	}
	job.State = domain.JobQueued
	job.RunAt = failure.RetryAt
	return nil
}

func (f *fakeJobs) ReclaimExpiredLeases(_ context.Context, now time.Time) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	reclaimed := 0
	for _, job := range f.jobs {
		if job.State == domain.JobRunning && job.LockedUntil != nil && !job.LockedUntil.After(now) {
			job.State = domain.JobQueued
			job.LockedBy = ""
			job.LockedUntil = nil
			reclaimed++
		}
	}
	f.reclaims++
	return reclaimed, nil
}

func (f *fakeJobs) CountByState(_ context.Context, state domain.JobState) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	total := 0
	for _, job := range f.jobs {
		if job.State == state {
			total++
		}
	}
	return total, nil
}

func (f *fakeJobs) PendingKinds(_ context.Context) (map[domain.JobKind]int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pending := make(map[domain.JobKind]int, 4)
	for _, job := range f.jobs {
		if job.State == domain.JobQueued || job.State == domain.JobRunning {
			pending[job.Kind]++
		}
	}
	return pending, nil
}

// testOptions returns fast, deterministic runner options.
func testOptions() Options {
	return Options{
		WorkerCount:  1,
		PollInterval: 5 * time.Millisecond,
		Lease:        time.Second,
		JobTimeout:   time.Second,
		BackoffBase:  2 * time.Second,
		BackoffMax:   8 * time.Second,
		BatchSize:    4,
	}
}

func newTestRunner(jobs repository.JobRepository, clk clock.Clock) *Runner {
	return New(Deps{Jobs: jobs, Clock: clk, Options: testOptions()})
}

func TestRunnerCompletesJob(t *testing.T) {
	ctx := context.Background()
	jobs := newFakeJobs()
	fixed := clock.NewFixed(time.Date(2026, 9, 11, 8, 0, 0, 0, clock.Zone()))
	runner := newTestRunner(jobs, fixed)

	var executed int
	runner.Register(domain.JobNotifyDispatch, func(context.Context, domain.Job) error {
		executed++
		return nil
	})
	job := &domain.Job{Kind: domain.JobNotifyDispatch, MaxAttempts: 3, RunAt: fixed.Now()}
	if _, err := jobs.Enqueue(ctx, job); err != nil {
		t.Fatalf("入队失败: %v", err)
	}

	processed, err := runner.Poll(ctx, "worker-1")
	if err != nil {
		t.Fatalf("轮询失败: %v", err)
	}
	if processed != 1 || executed != 1 {
		t.Fatalf("作业应被执行一次: processed=%d executed=%d", processed, executed)
	}
	stored, err := jobs.ByID(ctx, job.ID)
	if err != nil {
		t.Fatalf("读取作业失败: %v", err)
	}
	if stored.State != domain.JobDone {
		t.Fatalf("成功执行后应为 done，实际 %s", stored.State)
	}
	if idle, err := runner.Poll(ctx, "worker-1"); err != nil || idle != 0 {
		t.Fatalf("队列为空时不应处理作业: %d (%v)", idle, err)
	}
}

func TestRunnerRetriesWithBackoffThenFailsPermanently(t *testing.T) {
	ctx := context.Background()
	jobs := newFakeJobs()
	fixed := clock.NewFixed(time.Date(2026, 9, 11, 8, 0, 0, 0, clock.Zone()))
	runner := newTestRunner(jobs, fixed)

	var attempts int
	runner.Register(domain.JobSettleParty, func(context.Context, domain.Job) error {
		attempts++
		return apperr.New(apperr.CodeUnavailable, "结算网关暂时不可用")
	})
	var hooked []string
	runner.RegisterFailureHook(domain.JobSettleParty, func(_ context.Context, job domain.Job, cause error) {
		hooked = append(hooked, cause.Error())
	})

	job := &domain.Job{Kind: domain.JobSettleParty, MaxAttempts: 2, RunAt: fixed.Now()}
	if _, err := jobs.Enqueue(ctx, job); err != nil {
		t.Fatalf("入队失败: %v", err)
	}

	if _, err := runner.Poll(ctx, "worker-1"); err != nil {
		t.Fatalf("首次轮询失败: %v", err)
	}
	first, err := jobs.ByID(ctx, job.ID)
	if err != nil {
		t.Fatalf("读取作业失败: %v", err)
	}
	if first.State != domain.JobQueued {
		t.Fatalf("可重试失败应回到队列，实际 %s", first.State)
	}
	expected := fixed.Now().Add(domain.Backoff(1, testOptions().BackoffBase, testOptions().BackoffMax))
	if !first.RunAt.Equal(expected) {
		t.Fatalf("重试时间应按退避策略推迟: 期望 %s 实际 %s", expected, first.RunAt)
	}
	if len(hooked) != 0 {
		t.Fatalf("可重试失败不应触发永久失败钩子: %v", hooked)
	}

	// 退避期内不会被领取。
	if processed, err := runner.Poll(ctx, "worker-1"); err != nil || processed != 0 {
		t.Fatalf("退避期内不应执行作业: %d (%v)", processed, err)
	}
	fixed.Advance(3 * time.Second)
	if _, err := runner.Poll(ctx, "worker-1"); err != nil {
		t.Fatalf("第二次轮询失败: %v", err)
	}
	second, err := jobs.ByID(ctx, job.ID)
	if err != nil {
		t.Fatalf("读取作业失败: %v", err)
	}
	if second.State != domain.JobFailed {
		t.Fatalf("耗尽重试后应永久失败，实际 %s", second.State)
	}
	if attempts != 2 {
		t.Fatalf("处理器应被调用 2 次，实际 %d", attempts)
	}
	if len(hooked) != 1 {
		t.Fatalf("永久失败应触发一次钩子，实际 %d", len(hooked))
	}
}

func TestRunnerTreatsBusinessRejectionAsPermanent(t *testing.T) {
	ctx := context.Background()
	jobs := newFakeJobs()
	fixed := clock.NewFixed(time.Date(2026, 9, 11, 8, 0, 0, 0, clock.Zone()))
	runner := newTestRunner(jobs, fixed)

	runner.Register(domain.JobEscalateIncident, func(context.Context, domain.Job) error {
		return apperr.New(apperr.CodeNotFound, "事故记录不存在")
	})
	job := &domain.Job{Kind: domain.JobEscalateIncident, MaxAttempts: 5, RunAt: fixed.Now()}
	if _, err := jobs.Enqueue(ctx, job); err != nil {
		t.Fatalf("入队失败: %v", err)
	}
	if _, err := runner.Poll(ctx, "worker-1"); err != nil {
		t.Fatalf("轮询失败: %v", err)
	}
	stored, err := jobs.ByID(ctx, job.ID)
	if err != nil {
		t.Fatalf("读取作业失败: %v", err)
	}
	if stored.State != domain.JobFailed {
		t.Fatalf("不可重试的业务错误应立即永久失败，实际 %s", stored.State)
	}
	if stored.Attempts != 1 {
		t.Fatalf("不应继续消耗重试次数，实际 %d", stored.Attempts)
	}
}

func TestRunnerRecoversFromHandlerPanicAndMissingHandler(t *testing.T) {
	ctx := context.Background()
	jobs := newFakeJobs()
	fixed := clock.NewFixed(time.Date(2026, 9, 11, 8, 0, 0, 0, clock.Zone()))
	runner := newTestRunner(jobs, fixed)

	runner.Register(domain.JobSweepOverdue, func(context.Context, domain.Job) error {
		panic("模拟处理器崩溃")
	})
	panicking := &domain.Job{Kind: domain.JobSweepOverdue, MaxAttempts: 1, RunAt: fixed.Now()}
	if _, err := jobs.Enqueue(ctx, panicking); err != nil {
		t.Fatalf("入队失败: %v", err)
	}
	orphan := &domain.Job{Kind: domain.JobExpireSessions, MaxAttempts: 3, RunAt: fixed.Now()}
	if _, err := jobs.Enqueue(ctx, orphan); err != nil {
		t.Fatalf("入队失败: %v", err)
	}

	if _, err := runner.Poll(ctx, "worker-1"); err != nil {
		t.Fatalf("轮询不应因 panic 中断: %v", err)
	}
	crashed, err := jobs.ByID(ctx, panicking.ID)
	if err != nil {
		t.Fatalf("读取作业失败: %v", err)
	}
	if crashed.State != domain.JobFailed {
		t.Fatalf("panic 且耗尽重试后应永久失败，实际 %s", crashed.State)
	}
	if crashed.LastError == "" {
		t.Fatal("panic 应被记录为失败原因")
	}
	unhandled, err := jobs.ByID(ctx, orphan.ID)
	if err != nil {
		t.Fatalf("读取作业失败: %v", err)
	}
	if unhandled.State != domain.JobFailed {
		t.Fatalf("缺少处理器的作业应永久失败，实际 %s", unhandled.State)
	}
}

func TestRunnerStartReclaimsLeasesAndStopsCleanly(t *testing.T) {
	ctx := context.Background()
	jobs := newFakeJobs()
	fixed := clock.NewFixed(time.Date(2026, 9, 11, 8, 0, 0, 0, clock.Zone()))
	runner := newTestRunner(jobs, fixed)

	stale := &domain.Job{Kind: domain.JobNotifyDispatch, MaxAttempts: 3, RunAt: fixed.Now()}
	if _, err := jobs.Enqueue(ctx, stale); err != nil {
		t.Fatalf("入队失败: %v", err)
	}
	if _, err := jobs.ClaimDue(ctx, "dead-worker", time.Millisecond, fixed.Now(), 1); err != nil {
		t.Fatalf("模拟租约失败: %v", err)
	}
	fixed.Advance(time.Second)

	done := make(chan struct{})
	runner.Register(domain.JobNotifyDispatch, func(context.Context, domain.Job) error {
		select {
		case <-done:
		default:
			close(done)
		}
		return nil
	})
	if err := runner.Start(ctx); err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	if err := runner.Start(ctx); !apperr.Is(err, apperr.CodeStateInvalid) {
		t.Fatalf("重复启动应被拒绝，实际 %v", err)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("过期租约的作业应被回收并重新执行")
	}
	runner.Stop()
	runner.Stop()

	stored, err := jobs.ByID(ctx, stale.ID)
	if err != nil {
		t.Fatalf("读取作业失败: %v", err)
	}
	if stored.State != domain.JobDone {
		t.Fatalf("恢复后的作业应执行完成，实际 %s", stored.State)
	}
}

func TestRunnerRequiresHandlersAndHonoursCancellation(t *testing.T) {
	jobs := newFakeJobs()
	fixed := clock.NewFixed(time.Date(2026, 9, 11, 8, 0, 0, 0, clock.Zone()))
	runner := newTestRunner(jobs, fixed)

	if err := runner.Start(context.Background()); !apperr.Is(err, apperr.CodeInternal) {
		t.Fatalf("未注册处理器时应拒绝启动，实际 %v", err)
	}

	var observed error
	runner.Register(domain.JobNotifyDispatch, func(ctx context.Context, _ domain.Job) error {
		observed = ctx.Err()
		return nil
	})
	if _, err := jobs.Enqueue(context.Background(), &domain.Job{
		Kind: domain.JobNotifyDispatch, MaxAttempts: 1, RunAt: fixed.Now(),
	}); err != nil {
		t.Fatalf("入队失败: %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	processed, err := runner.Poll(cancelled, "worker-1")
	if err != nil {
		t.Fatalf("已取消的轮询不应报错: %v", err)
	}
	if processed != 0 {
		t.Fatalf("已取消的上下文不应领取作业，实际 %d", processed)
	}
	if observed != nil {
		t.Fatalf("处理器不应被调用: %v", observed)
	}
}

func TestOptionsDefaults(t *testing.T) {
	normalized := Options{}.withDefaults()
	if normalized.WorkerCount != 2 || normalized.BatchSize != 4 {
		t.Fatalf("默认并发或批量大小错误: %+v", normalized)
	}
	if normalized.BackoffMax < normalized.BackoffBase {
		t.Fatalf("退避上限不应小于基准值: %+v", normalized)
	}
	if normalized.Lease <= 0 || normalized.JobTimeout <= 0 || normalized.PollInterval <= 0 {
		t.Fatalf("默认时长必须为正数: %+v", normalized)
	}
}

func TestTerminalClassification(t *testing.T) {
	permanent := []error{
		apperr.New(apperr.CodeNotFound, "缺失"),
		apperr.New(apperr.CodeInvalidArgument, "参数错误"),
		apperr.New(apperr.CodePermissionDenied, "无权"),
		apperr.New(apperr.CodeStateInvalid, "状态不允许"),
		context.Canceled,
	}
	for _, err := range permanent {
		if !terminal(err) {
			t.Fatalf("%v 应判定为不可重试", err)
		}
	}
	retryable := []error{
		apperr.New(apperr.CodeUnavailable, "依赖不可用"),
		apperr.New(apperr.CodeVersionConflict, "并发冲突"),
		apperr.New(apperr.CodeInternal, "内部错误"),
		errors.New("未分类错误"),
	}
	for _, err := range retryable {
		if terminal(err) {
			t.Fatalf("%v 应允许重试", err)
		}
	}
}

func TestDecodePayload(t *testing.T) {
	parsed, err := decodePayload(domain.Job{Payload: `{"party_id":7,"limit":3}`})
	if err != nil {
		t.Fatalf("解析载荷失败: %v", err)
	}
	if parsed.PartyID != 7 || parsed.Limit != 3 {
		t.Fatalf("载荷解析结果错误: %+v", parsed)
	}
	empty, err := decodePayload(domain.Job{})
	if err != nil {
		t.Fatalf("空载荷应可解析: %v", err)
	}
	if empty.PartyID != 0 {
		t.Fatalf("空载荷不应产生字段值: %+v", empty)
	}
	if _, err := decodePayload(domain.Job{ID: 9, Payload: "not-json"}); !apperr.Is(err, apperr.CodeInvalidArgument) {
		t.Fatalf("非法载荷应返回 invalid_argument，实际 %v", err)
	}
}

var _ repository.JobRepository = (*fakeJobs)(nil)
