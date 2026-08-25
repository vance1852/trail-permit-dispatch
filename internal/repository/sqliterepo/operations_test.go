package sqliterepo

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/vance1852/trail-permit-dispatch/internal/apperr"
	"github.com/vance1852/trail-permit-dispatch/internal/domain"
	"github.com/vance1852/trail-permit-dispatch/internal/repository"
)

func TestPermitWindowEnsureIsIdempotent(t *testing.T) {
	ctx := context.Background()
	fix := newFixture(t)
	trail := fix.newTrail("AOMEN-RIDGE")

	first, err := fix.permits.EnsureWindow(ctx, trail.ID, "2026-09-12", trail.DailyQuota, testNow())
	if err != nil {
		t.Fatalf("开放许可窗口失败: %v", err)
	}
	second, err := fix.permits.EnsureWindow(ctx, trail.ID, "2026-09-12", 999, testNow())
	if err != nil {
		t.Fatalf("重复开放许可窗口失败: %v", err)
	}
	if first.ID != second.ID {
		t.Fatal("同一线路同一天只能有一个许可窗口")
	}
	if second.QuotaTotal != trail.DailyQuota {
		t.Fatalf("已存在窗口的配额不应被覆盖，实际 %d", second.QuotaTotal)
	}
	if _, err := fix.permits.EnsureWindow(ctx, trail.ID, "2026-09-13", 0, testNow()); !apperr.Is(err, apperr.CodeInvalidArgument) {
		t.Fatalf("零配额应被拒绝，实际 %v", err)
	}
	if _, err := fix.permits.WindowByTrailDay(ctx, trail.ID, "2026-10-01"); !apperr.Is(err, apperr.CodeNotFound) {
		t.Fatalf("未开放的日期应返回 not_found，实际 %v", err)
	}
}

func TestReserveSeatsEnforcesQuotaAndVersion(t *testing.T) {
	ctx := context.Background()
	fix := newFixture(t)
	trail := fix.newTrail("AOMEN-RIDGE")
	window, err := fix.permits.EnsureWindow(ctx, trail.ID, "2026-09-12", 10, testNow())
	if err != nil {
		t.Fatalf("开放许可窗口失败: %v", err)
	}

	if err := fix.permits.ReserveSeats(ctx, window.ID, window.Version, 6, testNow()); err != nil {
		t.Fatalf("首次占用应成功: %v", err)
	}
	err = fix.permits.ReserveSeats(ctx, window.ID, window.Version, 2, testNow())
	if !apperr.Is(err, apperr.CodeVersionConflict) {
		t.Fatalf("使用过期版本号应返回 version_conflict，实际 %v", err)
	}

	current, err := fix.permits.WindowByID(ctx, window.ID)
	if err != nil {
		t.Fatalf("读取窗口失败: %v", err)
	}
	if current.QuotaReserved != 6 || current.Remaining() != 4 {
		t.Fatalf("占用统计错误: %+v", current)
	}
	err = fix.permits.ReserveSeats(ctx, current.ID, current.Version, 5, testNow())
	if !apperr.Is(err, apperr.CodeQuotaExhausted) {
		t.Fatalf("超额占用应返回 quota_exhausted，实际 %v", err)
	}
	if err := fix.permits.ReserveSeats(ctx, current.ID, current.Version, 0, testNow()); !apperr.Is(err, apperr.CodeInvalidArgument) {
		t.Fatalf("零座位应返回 invalid_argument，实际 %v", err)
	}

	if err := fix.permits.ReleaseSeats(ctx, current.ID, 6, testNow()); err != nil {
		t.Fatalf("释放名额失败: %v", err)
	}
	released, err := fix.permits.WindowByID(ctx, current.ID)
	if err != nil {
		t.Fatalf("读取窗口失败: %v", err)
	}
	if released.QuotaReserved != 0 {
		t.Fatalf("释放后占用应归零，实际 %d", released.QuotaReserved)
	}
	if err := fix.permits.ReleaseSeats(ctx, released.ID, 1, testNow()); !apperr.Is(err, apperr.CodeConflict) {
		t.Fatalf("释放超过占用量应被拒绝，实际 %v", err)
	}

	if err := fix.permits.Close(ctx, released.ID, testNow()); err != nil {
		t.Fatalf("关闭窗口失败: %v", err)
	}
	closed, err := fix.permits.WindowByID(ctx, released.ID)
	if err != nil {
		t.Fatalf("读取窗口失败: %v", err)
	}
	if !closed.Closed() {
		t.Fatal("窗口应已关闭")
	}
	if err := fix.permits.ReserveSeats(ctx, closed.ID, closed.Version, 1, testNow()); !apperr.Is(err, apperr.CodeStateInvalid) {
		t.Fatalf("已关闭窗口应返回 state_invalid，实际 %v", err)
	}
	if err := fix.permits.Close(ctx, closed.ID, testNow()); !apperr.Is(err, apperr.CodeStateInvalid) {
		t.Fatalf("重复关闭应返回 state_invalid，实际 %v", err)
	}
}

func TestReserveSeatsUnderConcurrencyNeverOverbooks(t *testing.T) {
	ctx := context.Background()
	fix := newFixture(t)
	trail := fix.newTrail("AOMEN-RIDGE")
	window, err := fix.permits.EnsureWindow(ctx, trail.ID, "2026-09-12", 10, testNow())
	if err != nil {
		t.Fatalf("开放许可窗口失败: %v", err)
	}

	const goroutines = 8
	const seats = 3
	var (
		barrier   sync.WaitGroup
		finished  sync.WaitGroup
		mu        sync.Mutex
		succeeded int
	)
	barrier.Add(1)
	for i := 0; i < goroutines; i++ {
		finished.Add(1)
		go func() {
			defer finished.Done()
			barrier.Wait()
			// Each attempt reads the current version first, exactly like the
			// service layer does, and then tries the conditional update.
			for attempt := 0; attempt < goroutines; attempt++ {
				current, err := fix.permits.WindowByID(ctx, window.ID)
				if err != nil {
					return
				}
				err = fix.permits.ReserveSeats(ctx, current.ID, current.Version, seats, testNow())
				switch {
				case err == nil:
					mu.Lock()
					succeeded++
					mu.Unlock()
					return
				case apperr.Is(err, apperr.CodeVersionConflict):
					continue
				default:
					return
				}
			}
		}()
	}
	barrier.Done()
	finished.Wait()

	final, err := fix.permits.WindowByID(ctx, window.ID)
	if err != nil {
		t.Fatalf("读取窗口失败: %v", err)
	}
	if final.QuotaReserved > final.QuotaTotal {
		t.Fatalf("并发占用超卖: reserved=%d total=%d", final.QuotaReserved, final.QuotaTotal)
	}
	if succeeded*seats != final.QuotaReserved {
		t.Fatalf("成功次数与占用量不一致: %d*%d != %d", succeeded, seats, final.QuotaReserved)
	}
	if succeeded != 3 {
		t.Fatalf("10 个名额每次 3 座应恰好成功 3 次，实际 %d", succeeded)
	}
}

func TestPartyStateChangeIsGuardedByStateAndVersion(t *testing.T) {
	ctx := context.Background()
	fix := newFixture(t)
	trail := fix.newTrail("AOMEN-RIDGE")
	leader := fix.newUser("leader@trail.local", domain.RoleLeader)
	party := fix.newParty("TP-20260912-a1", trail, leader.ID, 4)
	window, err := fix.permits.EnsureWindow(ctx, trail.ID, "2026-09-12", 12, testNow())
	if err != nil {
		t.Fatalf("开放许可窗口失败: %v", err)
	}

	confirmedAt := testNow()
	change := repository.PartyStateChange{
		PartyID:         party.ID,
		ExpectedState:   domain.PartyDraft,
		ExpectedVersion: party.Version,
		NextState:       domain.PartyPermitReserved,
		PermitWindowID:  &window.ID,
		ConfirmedAt:     &confirmedAt,
		Now:             testNow(),
	}
	if err := fix.parties.ApplyStateChange(ctx, change); err != nil {
		t.Fatalf("状态变更失败: %v", err)
	}
	if err := fix.parties.ApplyStateChange(ctx, change); !apperr.Is(err, apperr.CodeVersionConflict) {
		t.Fatalf("重复应用同一变更应返回 version_conflict，实际 %v", err)
	}

	updated, err := fix.parties.ByCode(ctx, party.Code)
	if err != nil {
		t.Fatalf("读取队伍失败: %v", err)
	}
	if updated.State != domain.PartyPermitReserved || updated.Version != party.Version+1 {
		t.Fatalf("状态或版本未更新: %+v", updated)
	}
	if updated.PermitWindowID == nil || *updated.PermitWindowID != window.ID {
		t.Fatal("许可窗口未绑定到队伍")
	}
	if updated.ConfirmedAt == nil || !updated.ConfirmedAt.Equal(confirmedAt) {
		t.Fatalf("确认时间未持久化: %+v", updated.ConfirmedAt)
	}

	wrongState := change
	wrongState.ExpectedState = domain.PartyDraft
	wrongState.ExpectedVersion = updated.Version
	wrongState.NextState = domain.PartyConfirmed
	if err := fix.parties.ApplyStateChange(ctx, wrongState); !apperr.Is(err, apperr.CodeVersionConflict) {
		t.Fatalf("来源状态不匹配应被拒绝，实际 %v", err)
	}
}

func TestPartyMembersAndListingFilters(t *testing.T) {
	ctx := context.Background()
	fix := newFixture(t)
	trail := fix.newTrail("AOMEN-RIDGE")
	other := fix.newTrail("QINGXI-VALLEY")
	leader := fix.newUser("leader@trail.local", domain.RoleLeader)
	second := fix.newUser("second@trail.local", domain.RoleLeader)

	party := fix.newParty("TP-20260912-a1", trail, leader.ID, 3)
	fix.newPartyOn("TP-20260913-b2", other, second.ID, 2, "2026-09-13")

	member := &domain.PartyMember{
		PartyID: party.ID, MemberRef: "HK-001", DisplayName: "周岚",
		Kind: domain.MemberCompanion, WaiverSigned: true, Phone: "13800001234", JoinedAt: testNow(),
	}
	if _, err := fix.parties.AddMember(ctx, member); err != nil {
		t.Fatalf("登记队员失败: %v", err)
	}
	duplicate := *member
	duplicate.ID = 0
	if _, err := fix.parties.AddMember(ctx, &duplicate); !apperr.Is(err, apperr.CodeConflict) {
		t.Fatalf("重复队员编号应返回 conflict，实际 %v", err)
	}
	total, err := fix.parties.CountMembers(ctx, party.ID)
	if err != nil {
		t.Fatalf("统计队员失败: %v", err)
	}
	if total != 1 {
		t.Fatalf("队员数量应为 1，实际 %d", total)
	}

	members, err := fix.parties.Members(ctx, party.ID)
	if err != nil {
		t.Fatalf("读取队员失败: %v", err)
	}
	members[0].DisplayName = "被篡改"
	fresh, err := fix.parties.Members(ctx, party.ID)
	if err != nil {
		t.Fatalf("读取队员失败: %v", err)
	}
	if fresh[0].DisplayName != "周岚" {
		t.Fatal("仓储返回值被调用方修改后污染了后续读取结果")
	}

	page, err := domain.NewPageRequest(1, 10, "hike_day", false, PartySortKeys())
	if err != nil {
		t.Fatalf("构造分页失败: %v", err)
	}
	byLeader, err := fix.parties.List(ctx, domain.PartyFilter{LeaderID: leader.ID}, page)
	if err != nil {
		t.Fatalf("按领队查询失败: %v", err)
	}
	if byLeader.Total != 1 || byLeader.Items[0].Code != party.Code {
		t.Fatalf("领队过滤失效: %+v", byLeader)
	}
	byTrail, err := fix.parties.List(ctx, domain.PartyFilter{TrailID: other.ID}, page)
	if err != nil {
		t.Fatalf("按线路查询失败: %v", err)
	}
	if byTrail.Total != 1 || byTrail.Items[0].TrailID != other.ID {
		t.Fatalf("线路过滤失效: %+v", byTrail)
	}
	byState, err := fix.parties.List(ctx, domain.PartyFilter{States: []domain.PartyState{domain.PartyOnTrail}}, page)
	if err != nil {
		t.Fatalf("按状态查询失败: %v", err)
	}
	if byState.Total != 0 {
		t.Fatalf("状态过滤失效: %+v", byState)
	}
	byDay, err := fix.parties.List(ctx, domain.PartyFilter{HikeDayGE: "2026-09-13"}, page)
	if err != nil {
		t.Fatalf("按日期查询失败: %v", err)
	}
	if byDay.Total != 1 {
		t.Fatalf("日期区间过滤失效: %+v", byDay)
	}
	bySearch, err := fix.parties.List(ctx, domain.PartyFilter{Search: "TP-20260912"}, page)
	if err != nil {
		t.Fatalf("按关键词查询失败: %v", err)
	}
	if bySearch.Total != 1 {
		t.Fatalf("关键词过滤失效: %+v", bySearch)
	}
	wildcard, err := fix.parties.List(ctx, domain.PartyFilter{Search: "%"}, page)
	if err != nil {
		t.Fatalf("通配符查询失败: %v", err)
	}
	if wildcard.Total != 2 {
		t.Fatalf("用户输入的通配符应被转义后退化为空关键词，实际 %d", wildcard.Total)
	}
}

func TestCheckpointReportsAndOnTrailListing(t *testing.T) {
	ctx := context.Background()
	fix := newFixture(t)
	trail := fix.newTrail("AOMEN-RIDGE")
	leader := fix.newUser("leader@trail.local", domain.RoleLeader)
	party := fix.newParty("TP-20260912-a1", trail, leader.ID, 3)
	checkpoint, err := fix.trails.CheckpointBySeq(ctx, trail.ID, 1)
	if err != nil {
		t.Fatalf("读取打点失败: %v", err)
	}

	exists, err := fix.reports.Exists(ctx, party.ID, checkpoint.ID)
	if err != nil {
		t.Fatalf("检查打点失败: %v", err)
	}
	if exists {
		t.Fatal("尚未上报时不应存在打点记录")
	}
	if highest, err := fix.reports.HighestSeq(ctx, party.ID); err != nil || highest != 0 {
		t.Fatalf("无记录时最高序号应为 0，实际 %d (%v)", highest, err)
	}

	report := &domain.CheckpointReport{
		PartyID: party.ID, CheckpointID: checkpoint.ID, Seq: checkpoint.Seq,
		Status: domain.ReportOnTime, HeadCount: 3, Note: "全员到达",
		ReportedAt: testNow(), ReportedBy: leader.ID,
	}
	if _, err := fix.reports.Create(ctx, report); err != nil {
		t.Fatalf("记录打点失败: %v", err)
	}
	again := *report
	again.ID = 0
	if _, err := fix.reports.Create(ctx, &again); !apperr.Is(err, apperr.CodeConflict) {
		t.Fatalf("同一打点重复上报应返回 conflict，实际 %v", err)
	}
	if exists, err = fix.reports.Exists(ctx, party.ID, checkpoint.ID); err != nil || !exists {
		t.Fatalf("上报后应存在记录: %v", err)
	}
	if highest, err := fix.reports.HighestSeq(ctx, party.ID); err != nil || highest != 1 {
		t.Fatalf("最高序号应为 1，实际 %d (%v)", highest, err)
	}
	reports, err := fix.reports.ByParty(ctx, party.ID)
	if err != nil {
		t.Fatalf("读取打点记录失败: %v", err)
	}
	if len(reports) != 1 || reports[0].Status != domain.ReportOnTime || reports[0].Note != "全员到达" {
		t.Fatalf("打点记录内容错误: %+v", reports)
	}

	if err := fix.parties.ApplyStateChange(ctx, repository.PartyStateChange{
		PartyID: party.ID, ExpectedState: domain.PartyDraft, ExpectedVersion: party.Version,
		NextState: domain.PartyPermitReserved, Now: testNow(),
	}); err != nil {
		t.Fatalf("状态变更失败: %v", err)
	}
	onTrail, err := fix.parties.OnTrailParties(ctx, 10)
	if err != nil {
		t.Fatalf("查询在途队伍失败: %v", err)
	}
	if len(onTrail) != 0 {
		t.Fatalf("尚未放行时不应有在途队伍，实际 %d", len(onTrail))
	}
	reserved, err := fix.parties.ByID(ctx, party.ID)
	if err != nil {
		t.Fatalf("读取队伍失败: %v", err)
	}
	if err := fix.parties.ApplyStateChange(ctx, repository.PartyStateChange{
		PartyID: party.ID, ExpectedState: domain.PartyPermitReserved, ExpectedVersion: reserved.Version,
		NextState: domain.PartyConfirmed, Now: testNow(),
	}); err != nil {
		t.Fatalf("状态变更失败: %v", err)
	}
	confirmed, err := fix.parties.ByID(ctx, party.ID)
	if err != nil {
		t.Fatalf("读取队伍失败: %v", err)
	}
	dispatchedAt := testNow()
	if err := fix.parties.ApplyStateChange(ctx, repository.PartyStateChange{
		PartyID: party.ID, ExpectedState: domain.PartyConfirmed, ExpectedVersion: confirmed.Version,
		NextState: domain.PartyOnTrail, DispatchedAt: &dispatchedAt, Now: testNow(),
	}); err != nil {
		t.Fatalf("状态变更失败: %v", err)
	}
	running, err := fix.parties.OnTrailParties(ctx, 10)
	if err != nil {
		t.Fatalf("查询在途队伍失败: %v", err)
	}
	if len(running) != 1 || running[0].Code != party.Code {
		t.Fatalf("在途队伍查询失效: %+v", running)
	}
}

func TestIncidentAndSettlementRepositories(t *testing.T) {
	ctx := context.Background()
	fix := newFixture(t)
	trail := fix.newTrail("AOMEN-RIDGE")
	leader := fix.newUser("leader@trail.local", domain.RoleLeader)
	party := fix.newParty("TP-20260912-a1", trail, leader.ID, 3)

	incident := &domain.Incident{
		PartyID: party.ID, Kind: "checkpoint_overdue", Severity: domain.SeverityMajor,
		State: domain.IncidentOpen, Summary: "未按时上报第二个打点", CheckpointSeq: 2,
		OpenedBy: leader.ID, OpenedAt: testNow(), UpdatedAt: testNow(),
	}
	if _, err := fix.incidents.Create(ctx, incident); err != nil {
		t.Fatalf("创建事故失败: %v", err)
	}
	duplicate := *incident
	duplicate.ID = 0
	if _, err := fix.incidents.Create(ctx, &duplicate); !apperr.Is(err, apperr.CodeConflict) {
		t.Fatalf("同一队伍同类未处置事故应唯一，实际 %v", err)
	}
	found, err := fix.incidents.OpenByPartyKind(ctx, party.ID, "checkpoint_overdue", 2)
	if err != nil {
		t.Fatalf("查询未处置事故失败: %v", err)
	}
	if found.ID != incident.ID {
		t.Fatalf("查询到的事故不一致: %+v", found)
	}
	if _, err := fix.incidents.OpenByPartyKind(ctx, party.ID, "injury", 0); !apperr.Is(err, apperr.CodeNotFound) {
		t.Fatalf("不存在的事故应返回 not_found，实际 %v", err)
	}
	open, err := fix.incidents.CountOpenByParty(ctx, party.ID)
	if err != nil || open != 1 {
		t.Fatalf("未处置事故数量应为 1，实际 %d (%v)", open, err)
	}

	resolvedAt := testNow().Add(time.Hour)
	if err := fix.incidents.Apply(ctx, repository.IncidentUpdate{
		IncidentID: incident.ID, ExpectedVersion: incident.Version,
		NextState: domain.IncidentResolved, EscalationCount: 1,
		Resolution: "队伍已联系并归队", ResolvedAt: &resolvedAt, Now: testNow(),
	}); err != nil {
		t.Fatalf("更新事故失败: %v", err)
	}
	if err := fix.incidents.Apply(ctx, repository.IncidentUpdate{
		IncidentID: incident.ID, ExpectedVersion: incident.Version,
		NextState: domain.IncidentClosed, Now: testNow(),
	}); !apperr.Is(err, apperr.CodeVersionConflict) {
		t.Fatalf("过期版本更新事故应返回 version_conflict，实际 %v", err)
	}
	resolved, err := fix.incidents.ByID(ctx, incident.ID)
	if err != nil {
		t.Fatalf("读取事故失败: %v", err)
	}
	if resolved.State != domain.IncidentResolved || resolved.ResolvedAt == nil {
		t.Fatalf("事故处置结果未持久化: %+v", resolved)
	}
	if resolved.EscalationCount != 1 {
		t.Fatalf("升级次数未持久化: %d", resolved.EscalationCount)
	}
	// The partial unique index only covers unresolved incidents, so a new one
	// may be opened after the previous one was resolved.
	reopened := *incident
	reopened.ID = 0
	reopened.State = domain.IncidentOpen
	if _, err := fix.incidents.Create(ctx, &reopened); err != nil {
		t.Fatalf("处置后应允许再次登记同类事故: %v", err)
	}

	page, err := domain.NewPageRequest(1, 10, "opened_at", true, IncidentSortKeys())
	if err != nil {
		t.Fatalf("构造分页失败: %v", err)
	}
	list, err := fix.incidents.List(ctx, domain.IncidentFilter{
		PartyID: party.ID,
		States:  []domain.IncidentState{domain.IncidentOpen},
	}, page)
	if err != nil {
		t.Fatalf("查询事故失败: %v", err)
	}
	if list.Total != 1 {
		t.Fatalf("按状态过滤事故失效: %+v", list)
	}
	bySeverity, err := fix.incidents.List(ctx, domain.IncidentFilter{
		Severities: []domain.Severity{domain.SeverityCritical},
	}, page)
	if err != nil {
		t.Fatalf("查询事故失败: %v", err)
	}
	if bySeverity.Total != 0 {
		t.Fatalf("按级别过滤事故失效: %+v", bySeverity)
	}

	settlement := &domain.Settlement{
		PartyID: party.ID, Seats: 3, UnitFeeCents: 4500, TotalCents: 22500,
		State: domain.SettlementPending, CreatedAt: testNow(), UpdatedAt: testNow(),
	}
	if _, err := fix.settlements.Create(ctx, settlement); err != nil {
		t.Fatalf("创建结算失败: %v", err)
	}
	second := *settlement
	second.ID = 0
	if _, err := fix.settlements.Create(ctx, &second); !apperr.Is(err, apperr.CodeConflict) {
		t.Fatalf("一个队伍只应有一条结算记录，实际 %v", err)
	}
	settledAt := testNow().Add(2 * time.Hour)
	if err := fix.settlements.Apply(ctx, repository.SettlementUpdate{
		SettlementID: settlement.ID, ExpectedVersion: settlement.Version,
		NextState: domain.SettlementSettled, Reference: "PF-abcd", SettledAt: &settledAt, Now: testNow(),
	}); err != nil {
		t.Fatalf("更新结算失败: %v", err)
	}
	stored, err := fix.settlements.ByPartyID(ctx, party.ID)
	if err != nil {
		t.Fatalf("读取结算失败: %v", err)
	}
	if stored.State != domain.SettlementSettled || stored.Reference != "PF-abcd" || stored.SettledAt == nil {
		t.Fatalf("结算结果未持久化: %+v", stored)
	}
	settlementPage, err := domain.NewPageRequest(1, 10, "created_at", false, SettlementSortKeys())
	if err != nil {
		t.Fatalf("构造分页失败: %v", err)
	}
	settled, err := fix.settlements.List(ctx, domain.SettlementFilter{
		States: []domain.SettlementState{domain.SettlementSettled},
	}, settlementPage)
	if err != nil {
		t.Fatalf("查询结算失败: %v", err)
	}
	if settled.Total != 1 {
		t.Fatalf("结算状态过滤失效: %+v", settled)
	}
}

func TestAuditRepositoryFiltersAndPagination(t *testing.T) {
	ctx := context.Background()
	fix := newFixture(t)
	actor := fix.newUser("ranger@trail.local", domain.RoleRanger)

	for i := 0; i < 5; i++ {
		event := &domain.AuditEvent{
			RequestID: "req-1", ActorID: actor.ID, ActorRole: domain.RoleRanger,
			Action: "party.approve", ObjectType: "party", ObjectID: "TP-1",
			Result: domain.AuditSuccess, Detail: "复核通过", CreatedAt: testNow().Add(time.Duration(i) * time.Minute),
		}
		if _, err := fix.audits.Append(ctx, event); err != nil {
			t.Fatalf("写入审计失败: %v", err)
		}
	}
	failure := &domain.AuditEvent{
		RequestID: "req-2", ActorID: actor.ID, ActorRole: domain.RoleRanger,
		Action: "party.dispatch", ObjectType: "party", ObjectID: "TP-2",
		Result: domain.AuditFailure, Detail: "状态不允许", CreatedAt: testNow(),
	}
	if _, err := fix.audits.Append(ctx, failure); err != nil {
		t.Fatalf("写入审计失败: %v", err)
	}

	page, err := domain.NewPageRequest(2, 2, "created_at", false, AuditSortKeys())
	if err != nil {
		t.Fatalf("构造分页失败: %v", err)
	}
	byAction, err := fix.audits.List(ctx, domain.AuditFilter{Action: "party.approve"}, page)
	if err != nil {
		t.Fatalf("查询审计失败: %v", err)
	}
	if byAction.Total != 5 || len(byAction.Items) != 2 || byAction.TotalPages != 3 {
		t.Fatalf("审计分页错误: total=%d items=%d pages=%d", byAction.Total, len(byAction.Items), byAction.TotalPages)
	}
	firstPage, err := domain.NewPageRequest(1, 10, "created_at", false, AuditSortKeys())
	if err != nil {
		t.Fatalf("构造分页失败: %v", err)
	}
	failures, err := fix.audits.List(ctx, domain.AuditFilter{Result: domain.AuditFailure}, firstPage)
	if err != nil {
		t.Fatalf("查询审计失败: %v", err)
	}
	if failures.Total != 1 || failures.Items[0].Action != "party.dispatch" {
		t.Fatalf("按结果过滤审计失效: %+v", failures)
	}
	byObject, err := fix.audits.CountByObject(ctx, "party", "TP-1")
	if err != nil {
		t.Fatalf("统计对象审计失败: %v", err)
	}
	if byObject != 5 {
		t.Fatalf("对象审计数量应为 5，实际 %d", byObject)
	}
}

func TestIdempotencyRecords(t *testing.T) {
	ctx := context.Background()
	fix := newFixture(t)
	user := fix.newUser("leader@trail.local", domain.RoleLeader)

	record := &repository.IdempotencyRecord{
		Scope: "party.permit_request", Key: "key-1", ActorID: user.ID,
		RequestHash: "hash-1", ResponseBody: `{"code":"TP-1"}`,
		CreatedAt: testNow(), ExpiresAt: testNow().Add(time.Hour),
	}
	if _, err := fix.idem.Save(ctx, record); err != nil {
		t.Fatalf("保存幂等记录失败: %v", err)
	}
	duplicate := *record
	duplicate.ID = 0
	if _, err := fix.idem.Save(ctx, &duplicate); !apperr.Is(err, apperr.CodeConflict) {
		t.Fatalf("同一幂等键应唯一，实际 %v", err)
	}
	found, err := fix.idem.Find(ctx, "party.permit_request", "key-1", user.ID)
	if err != nil {
		t.Fatalf("读取幂等记录失败: %v", err)
	}
	if found.ResponseBody != `{"code":"TP-1"}` || found.RequestHash != "hash-1" {
		t.Fatalf("幂等记录内容错误: %+v", found)
	}
	if _, err := fix.idem.Find(ctx, "party.permit_request", "key-1", user.ID+1); !apperr.Is(err, apperr.CodeNotFound) {
		t.Fatalf("不同操作者的幂等键应互相隔离，实际 %v", err)
	}
	deleted, err := fix.idem.DeleteExpired(ctx, testNow().Add(2*time.Hour))
	if err != nil {
		t.Fatalf("清理幂等记录失败: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("应清理 1 条过期记录，实际 %d", deleted)
	}
}
