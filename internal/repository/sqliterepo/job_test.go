package sqliterepo

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/vance1852/trail-permit-dispatch/internal/apperr"
	"github.com/vance1852/trail-permit-dispatch/internal/domain"
	"github.com/vance1852/trail-permit-dispatch/internal/repository"
)

func (f *fixture) enqueue(kind domain.JobKind, payload string, runAt time.Time) *domain.Job {
	f.t.Helper()
	job := &domain.Job{
		Kind: kind, Payload: payload, MaxAttempts: 3,
		RunAt: runAt, CreatedAt: testNow(), UpdatedAt: testNow(),
	}
	if _, err := f.jobs.Enqueue(context.Background(), job); err != nil {
		f.t.Fatalf("入队作业失败: %v", err)
	}
	return job
}

func TestJobClaimLeasesOnlyDueQueuedJobs(t *testing.T) {
	ctx := context.Background()
	fix := newFixture(t)
	now := testNow()

	due := fix.enqueue(domain.JobNotifyDispatch, `{"party_id":1}`, now.Add(-time.Minute))
	future := fix.enqueue(domain.JobSettleParty, `{"party_id":2}`, now.Add(time.Hour))

	claimed, err := fix.jobs.ClaimDue(ctx, "worker-1", 30*time.Second, now, 10)
	if err != nil {
		t.Fatalf("租约作业失败: %v", err)
	}
	if len(claimed) != 1 || claimed[0].ID != due.ID {
		t.Fatalf("只应领取到期作业，实际 %+v", claimed)
	}
	if claimed[0].Attempts != 1 {
		t.Fatalf("领取时应累加尝试次数，实际 %d", claimed[0].Attempts)
	}
	if claimed[0].State != domain.JobRunning || claimed[0].LockedBy != "worker-1" {
		t.Fatalf("领取后状态或持有者错误: %+v", claimed[0])
	}
	if claimed[0].LockedUntil == nil || !claimed[0].LockedUntil.Equal(now.Add(30*time.Second)) {
		t.Fatalf("租约到期时间错误: %+v", claimed[0].LockedUntil)
	}

	empty, err := fix.jobs.ClaimDue(ctx, "worker-2", 30*time.Second, now, 10)
	if err != nil {
		t.Fatalf("租约作业失败: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("已被租约的作业不应被其他 worker 领取，实际 %+v", empty)
	}

	if _, err := fix.jobs.ClaimDue(ctx, "", 30*time.Second, now, 1); !apperr.Is(err, apperr.CodeInvalidArgument) {
		t.Fatalf("空 worker 标识应被拒绝，实际 %v", err)
	}

	running, err := fix.jobs.CountByState(ctx, domain.JobRunning)
	if err != nil {
		t.Fatalf("统计作业失败: %v", err)
	}
	if running != 1 {
		t.Fatalf("执行中作业应为 1，实际 %d", running)
	}
	pending, err := fix.jobs.PendingKinds(ctx)
	if err != nil {
		t.Fatalf("统计待执行作业失败: %v", err)
	}
	if pending[domain.JobNotifyDispatch] != 1 || pending[domain.JobSettleParty] != 1 {
		t.Fatalf("待执行作业统计错误: %+v", pending)
	}
	if _, err := fix.jobs.ByID(ctx, future.ID); err != nil {
		t.Fatalf("读取作业失败: %v", err)
	}
	if _, err := fix.jobs.ByID(ctx, 9999); !apperr.Is(err, apperr.CodeNotFound) {
		t.Fatalf("不存在的作业应返回 not_found，实际 %v", err)
	}
}

func TestJobFailureRetriesThenParks(t *testing.T) {
	ctx := context.Background()
	fix := newFixture(t)
	now := testNow()
	job := fix.enqueue(domain.JobSettleParty, `{"party_id":1}`, now)

	claimed, err := fix.jobs.ClaimDue(ctx, "worker-1", time.Minute, now, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("租约作业失败: %v", err)
	}
	retryAt := now.Add(4 * time.Second)
	if err := fix.jobs.MarkFailure(ctx, repository.JobFailure{
		JobID: job.ID, Message: "结算网关超时", Permanent: false, RetryAt: retryAt, Now: now,
	}); err != nil {
		t.Fatalf("记录失败状态失败: %v", err)
	}
	retried, err := fix.jobs.ByID(ctx, job.ID)
	if err != nil {
		t.Fatalf("读取作业失败: %v", err)
	}
	if retried.State != domain.JobQueued {
		t.Fatalf("可重试失败应回到队列，实际 %s", retried.State)
	}
	if !retried.RunAt.Equal(retryAt) {
		t.Fatalf("重试时间未按退避设置: %s", retried.RunAt)
	}
	if retried.LockedBy != "" || retried.LockedUntil != nil {
		t.Fatalf("失败后应释放租约: %+v", retried)
	}
	if retried.LastError != "结算网关超时" {
		t.Fatalf("失败原因未记录: %q", retried.LastError)
	}
	if _, err := fix.jobs.ClaimDue(ctx, "worker-1", time.Minute, now, 1); err != nil {
		t.Fatalf("租约作业失败: %v", err)
	}
	// The job is not due yet at now, so it must not be claimable before retryAt.
	early, err := fix.jobs.ClaimDue(ctx, "worker-1", time.Minute, now.Add(time.Second), 1)
	if err != nil {
		t.Fatalf("租约作业失败: %v", err)
	}
	if len(early) != 0 {
		t.Fatalf("退避期内不应重新领取，实际 %+v", early)
	}

	claimed, err = fix.jobs.ClaimDue(ctx, "worker-1", time.Minute, retryAt, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("退避结束后应可领取: %v", err)
	}
	if claimed[0].Attempts != 2 {
		t.Fatalf("重试应累加尝试次数，实际 %d", claimed[0].Attempts)
	}
	if err := fix.jobs.MarkFailure(ctx, repository.JobFailure{
		JobID: job.ID, Message: "结算网关持续不可用", Permanent: true, Now: retryAt,
	}); err != nil {
		t.Fatalf("记录永久失败失败: %v", err)
	}
	parked, err := fix.jobs.ByID(ctx, job.ID)
	if err != nil {
		t.Fatalf("读取作业失败: %v", err)
	}
	if parked.State != domain.JobFailed {
		t.Fatalf("永久失败应停留在 failed，实际 %s", parked.State)
	}
	if err := fix.jobs.MarkDone(ctx, job.ID, retryAt); !apperr.Is(err, apperr.CodeStateInvalid) {
		t.Fatalf("非执行中作业不应可标记完成，实际 %v", err)
	}
	if err := fix.jobs.MarkFailure(ctx, repository.JobFailure{
		JobID: job.ID, Message: "重复记录", Permanent: true, Now: retryAt,
	}); !apperr.Is(err, apperr.CodeStateInvalid) {
		t.Fatalf("非执行中作业不应可记录失败，实际 %v", err)
	}
}

func TestJobLeaseReclaimAfterWorkerLoss(t *testing.T) {
	ctx := context.Background()
	fix := newFixture(t)
	now := testNow()
	job := fix.enqueue(domain.JobNotifyDispatch, `{"party_id":1}`, now)

	if _, err := fix.jobs.ClaimDue(ctx, "worker-crashed", 10*time.Second, now, 1); err != nil {
		t.Fatalf("租约作业失败: %v", err)
	}
	reclaimed, err := fix.jobs.ReclaimExpiredLeases(ctx, now.Add(5*time.Second))
	if err != nil {
		t.Fatalf("回收租约失败: %v", err)
	}
	if reclaimed != 0 {
		t.Fatalf("租约未到期不应回收，实际 %d", reclaimed)
	}
	reclaimed, err = fix.jobs.ReclaimExpiredLeases(ctx, now.Add(11*time.Second))
	if err != nil {
		t.Fatalf("回收租约失败: %v", err)
	}
	if reclaimed != 1 {
		t.Fatalf("应回收 1 个过期租约，实际 %d", reclaimed)
	}
	requeued, err := fix.jobs.ByID(ctx, job.ID)
	if err != nil {
		t.Fatalf("读取作业失败: %v", err)
	}
	if requeued.State != domain.JobQueued || requeued.LockedBy != "" {
		t.Fatalf("回收后应回到队列: %+v", requeued)
	}
	claimed, err := fix.jobs.ClaimDue(ctx, "worker-new", time.Minute, now.Add(11*time.Second), 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("回收后应可被新 worker 领取: %v", err)
	}
	if err := fix.jobs.MarkDone(ctx, job.ID, now.Add(12*time.Second)); err != nil {
		t.Fatalf("标记完成失败: %v", err)
	}
	done, err := fix.jobs.ByID(ctx, job.ID)
	if err != nil {
		t.Fatalf("读取作业失败: %v", err)
	}
	if done.State != domain.JobDone || done.LastError != "" {
		t.Fatalf("完成后状态错误: %+v", done)
	}
}

func TestJobEnqueueValidatesAttemptsAndPayload(t *testing.T) {
	ctx := context.Background()
	fix := newFixture(t)
	invalid := &domain.Job{Kind: domain.JobSweepOverdue, MaxAttempts: 0, RunAt: testNow()}
	if _, err := fix.jobs.Enqueue(ctx, invalid); !apperr.Is(err, apperr.CodeInvalidArgument) {
		t.Fatalf("零重试上限应被拒绝，实际 %v", err)
	}
	job := &domain.Job{Kind: domain.JobSweepOverdue, MaxAttempts: 2, RunAt: testNow(), CreatedAt: testNow(), UpdatedAt: testNow()}
	if _, err := fix.jobs.Enqueue(ctx, job); err != nil {
		t.Fatalf("入队失败: %v", err)
	}
	stored, err := fix.jobs.ByID(ctx, job.ID)
	if err != nil {
		t.Fatalf("读取作业失败: %v", err)
	}
	if stored.Payload != "{}" {
		t.Fatalf("空载荷应写入空 JSON 对象，实际 %q", stored.Payload)
	}
	bogus := &domain.Job{Kind: domain.JobKind("send_sms"), MaxAttempts: 1, RunAt: testNow()}
	if _, err := fix.jobs.Enqueue(ctx, bogus); err == nil {
		t.Fatal("数据库应拒绝未定义的作业类型")
	}
}

func TestStateSurvivesDatabaseRestart(t *testing.T) {
	ctx := context.Background()
	dsn := "file:" + filepath.ToSlash(filepath.Join(t.TempDir(), "restart.sqlite"))
	first := openFixture(t, dsn)
	trail := first.newTrail("AOMEN-RIDGE")
	leader := first.newUser("leader@trail.local", domain.RoleLeader)
	party := first.newParty("TP-20260912-a1", trail, leader.ID, 4)
	window, err := first.permits.EnsureWindow(ctx, trail.ID, "2026-09-12", 12, testNow())
	if err != nil {
		t.Fatalf("开放许可窗口失败: %v", err)
	}
	if err := first.permits.ReserveSeats(ctx, window.ID, window.Version, 4, testNow()); err != nil {
		t.Fatalf("占用名额失败: %v", err)
	}
	if err := first.parties.ApplyStateChange(ctx, repository.PartyStateChange{
		PartyID: party.ID, ExpectedState: domain.PartyDraft, ExpectedVersion: party.Version,
		NextState: domain.PartyPermitReserved, PermitWindowID: &window.ID, Now: testNow(),
	}); err != nil {
		t.Fatalf("状态变更失败: %v", err)
	}
	job := first.enqueue(domain.JobNotifyDispatch, `{"party_id":1}`, testNow())
	if _, err := first.jobs.ClaimDue(ctx, "worker-before-restart", 10*time.Second, testNow(), 1); err != nil {
		t.Fatalf("租约作业失败: %v", err)
	}
	if err := first.db.Close(); err != nil {
		t.Fatalf("关闭数据库失败: %v", err)
	}

	second := openFixture(t, dsn)
	restored, err := second.parties.ByCode(ctx, party.Code)
	if err != nil {
		t.Fatalf("重启后读取队伍失败: %v", err)
	}
	if restored.State != domain.PartyPermitReserved || restored.PermitWindowID == nil {
		t.Fatalf("重启后队伍状态丢失: %+v", restored)
	}
	restoredWindow, err := second.permits.WindowByID(ctx, window.ID)
	if err != nil {
		t.Fatalf("重启后读取窗口失败: %v", err)
	}
	if restoredWindow.QuotaReserved != 4 {
		t.Fatalf("重启后占用名额丢失: %d", restoredWindow.QuotaReserved)
	}
	stuck, err := second.jobs.ByID(ctx, job.ID)
	if err != nil {
		t.Fatalf("重启后读取作业失败: %v", err)
	}
	if stuck.State != domain.JobRunning {
		t.Fatalf("重启前被租约的作业状态应保留，实际 %s", stuck.State)
	}
	reclaimed, err := second.jobs.ReclaimExpiredLeases(ctx, testNow().Add(time.Minute))
	if err != nil {
		t.Fatalf("重启后回收租约失败: %v", err)
	}
	if reclaimed != 1 {
		t.Fatalf("重启后应回收 1 个租约，实际 %d", reclaimed)
	}
	recovered, err := second.jobs.ClaimDue(ctx, "worker-after-restart", time.Minute, testNow().Add(time.Minute), 1)
	if err != nil || len(recovered) != 1 {
		t.Fatalf("重启恢复后作业应可继续执行: %v", err)
	}
}
