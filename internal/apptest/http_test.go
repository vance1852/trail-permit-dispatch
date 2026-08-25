package apptest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vance1852/trail-permit-dispatch/internal/apperr"
	"github.com/vance1852/trail-permit-dispatch/internal/domain"
)

// apiResponse mirrors the JSON envelope of the platform.
type apiResponse struct {
	Data  json.RawMessage `json:"data"`
	Error *struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		Field     string `json:"field"`
		RequestID string `json:"request_id"`
	} `json:"error"`
}

// httpFixture wraps the harness with a live HTTP server.
type httpFixture struct {
	*harness
	server *httptest.Server
}

func newHTTPFixture(t *testing.T) *httpFixture {
	t.Helper()
	h := newHarness(t)
	server := httptest.NewServer(h.app.HTTP.Handler())
	t.Cleanup(server.Close)
	return &httpFixture{harness: h, server: server}
}

// call performs an API request and decodes the envelope.
func (f *httpFixture) call(method, path, token string, body any, headers map[string]string) (*http.Response, apiResponse) {
	f.t.Helper()
	var reader *bytes.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			f.t.Fatalf("序列化请求体失败: %v", err)
		}
		reader = bytes.NewReader(encoded)
	} else {
		reader = bytes.NewReader(nil)
	}
	request, err := http.NewRequest(method, f.server.URL+path, reader)
	if err != nil {
		f.t.Fatalf("构造请求失败: %v", err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response, err := f.server.Client().Do(request)
	if err != nil {
		f.t.Fatalf("发送请求失败: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	var envelope apiResponse
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		f.t.Fatalf("解析响应失败: %v", err)
	}
	return response, envelope
}

func (f *httpFixture) mustSucceed(method, path, token string, body any, expected int) apiResponse {
	f.t.Helper()
	response, envelope := f.call(method, path, token, body, nil)
	if response.StatusCode != expected {
		f.t.Fatalf("%s %s 期望状态码 %d，实际 %d，错误 %+v", method, path, expected, response.StatusCode, envelope.Error)
	}
	return envelope
}

func TestHTTPLoginAndSessionRevocation(t *testing.T) {
	f := newHTTPFixture(t)

	envelope := f.mustSucceed(http.MethodPost, "/api/v1/auth/login", "", map[string]string{
		"email": leaderEmail, "password": leaderPassword,
	}, http.StatusOK)
	var login struct {
		Token   string `json:"token"`
		Account struct {
			Role string `json:"role"`
		} `json:"account"`
	}
	if err := json.Unmarshal(envelope.Data, &login); err != nil {
		t.Fatalf("解析登录响应失败: %v", err)
	}
	if login.Token == "" || login.Account.Role != string(domain.RoleLeader) {
		t.Fatalf("登录响应内容错误: %+v", login)
	}

	f.mustSucceed(http.MethodGet, "/api/v1/auth/me", login.Token, nil, http.StatusOK)

	response, failure := f.call(http.MethodPost, "/api/v1/auth/login", "", map[string]string{
		"email": leaderEmail, "password": "wrong-password-1",
	}, nil)
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("错误密码应返回 401，实际 %d", response.StatusCode)
	}
	if failure.Error == nil || failure.Error.Code != string(apperr.CodeUnauthenticated) {
		t.Fatalf("错误密码的错误码不正确: %+v", failure.Error)
	}

	f.mustSucceed(http.MethodPost, "/api/v1/auth/logout", login.Token, nil, http.StatusOK)
	revoked, _ := f.call(http.MethodGet, "/api/v1/auth/me", login.Token, nil, nil)
	if revoked.StatusCode != http.StatusUnauthorized {
		t.Fatalf("退出后令牌应失效，实际 %d", revoked.StatusCode)
	}
}

func TestHTTPAuthorizationBoundaries(t *testing.T) {
	f := newHTTPFixture(t)
	leaderToken := f.token(leaderEmail, leaderPassword)
	rangerToken := f.token(rangerEmail, rangerPassword)

	anonymous, envelope := f.call(http.MethodGet, "/api/v1/parties", "", nil, nil)
	if anonymous.StatusCode != http.StatusUnauthorized {
		t.Fatalf("匿名访问应返回 401，实际 %d", anonymous.StatusCode)
	}
	if envelope.Error == nil || envelope.Error.RequestID == "" {
		t.Fatalf("错误响应应包含请求标识: %+v", envelope.Error)
	}

	bogus, _ := f.call(http.MethodGet, "/api/v1/parties", "not-a-real-token", nil, nil)
	if bogus.StatusCode != http.StatusUnauthorized {
		t.Fatalf("伪造令牌应返回 401，实际 %d", bogus.StatusCode)
	}

	forbidden, forbiddenBody := f.call(http.MethodPost, "/api/v1/trails", leaderToken, validTrailPayload(), nil)
	if forbidden.StatusCode != http.StatusForbidden {
		t.Fatalf("领队访问线路管理接口应返回 403，实际 %d", forbidden.StatusCode)
	}
	if forbiddenBody.Error.Code != string(apperr.CodePermissionDenied) {
		t.Fatalf("越权错误码不正确: %+v", forbiddenBody.Error)
	}

	rangerForbidden, _ := f.call(http.MethodPost, "/api/v1/parties", rangerToken, map[string]any{}, nil)
	if rangerForbidden.StatusCode != http.StatusForbidden {
		t.Fatalf("线路管理员不应建队，实际 %d", rangerForbidden.StatusCode)
	}

	auditForLeader, _ := f.call(http.MethodGet, "/api/v1/audit-events", leaderToken, nil, nil)
	if auditForLeader.StatusCode != http.StatusForbidden {
		t.Fatalf("领队不应读取审计，实际 %d", auditForLeader.StatusCode)
	}
	f.mustSucceed(http.MethodGet, "/api/v1/audit-events", rangerToken, nil, http.StatusOK)
}

func TestHTTPErrorContract(t *testing.T) {
	f := newHTTPFixture(t)
	token := f.token(leaderEmail, leaderPassword)

	missing, body := f.call(http.MethodGet, "/api/v1/parties/TP-does-not-exist", token, nil, nil)
	if missing.StatusCode != http.StatusNotFound || body.Error.Code != string(apperr.CodeNotFound) {
		t.Fatalf("未知队伍应返回 404/not_found，实际 %d %+v", missing.StatusCode, body.Error)
	}

	invalid, invalidBody := f.call(http.MethodPost, "/api/v1/parties", token, map[string]any{
		"trail_code":    valleyTrail,
		"hike_day":      f.hikeDay(1),
		"planned_start": "not-a-timestamp",
		"planned_end":   f.plannedStart(1, 15).Format(time.RFC3339),
		"size":          3,
		"contact_phone": "13800001234",
	}, nil)
	if invalid.StatusCode != http.StatusBadRequest {
		t.Fatalf("非法时间应返回 400，实际 %d", invalid.StatusCode)
	}
	if invalidBody.Error.Field != "planned_start" {
		t.Fatalf("错误响应应定位到字段: %+v", invalidBody.Error)
	}

	unknownField, _ := f.call(http.MethodPost, "/api/v1/auth/login", "", map[string]any{
		"email": leaderEmail, "password": leaderPassword, "remember": true,
	}, nil)
	if unknownField.StatusCode != http.StatusBadRequest {
		t.Fatalf("未知字段应返回 400，实际 %d", unknownField.StatusCode)
	}

	emptyBody, _ := f.call(http.MethodPost, "/api/v1/auth/login", "", nil, nil)
	if emptyBody.StatusCode != http.StatusBadRequest {
		t.Fatalf("空请求体应返回 400，实际 %d", emptyBody.StatusCode)
	}

	badSort, _ := f.call(http.MethodGet, "/api/v1/parties?sort_by=leader_id", token, nil, nil)
	if badSort.StatusCode != http.StatusBadRequest {
		t.Fatalf("白名单外排序字段应返回 400，实际 %d", badSort.StatusCode)
	}
	badPage, _ := f.call(http.MethodGet, "/api/v1/parties?size=1000", token, nil, nil)
	if badPage.StatusCode != http.StatusBadRequest {
		t.Fatalf("超限分页应返回 400，实际 %d", badPage.StatusCode)
	}

	response, _ := f.call(http.MethodGet, "/api/v1/auth/me", token, nil, map[string]string{"X-Request-ID": "req-fixed-123"})
	if got := response.Header.Get("X-Request-ID"); got != "req-fixed-123" {
		t.Fatalf("请求标识应被回显，实际 %q", got)
	}
	if got := response.Header.Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Fatalf("响应类型错误: %q", got)
	}
}

func TestHTTPHealthAndReadiness(t *testing.T) {
	f := newHTTPFixture(t)

	live := f.mustSucceed(http.MethodGet, "/healthz", "", nil, http.StatusOK)
	var liveBody struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(live.Data, &liveBody); err != nil {
		t.Fatalf("解析存活响应失败: %v", err)
	}
	if liveBody.Status != "alive" {
		t.Fatalf("存活检查响应错误: %+v", liveBody)
	}

	ready := f.mustSucceed(http.MethodGet, "/readyz", "", nil, http.StatusOK)
	var readyBody struct {
		Status        string         `json:"status"`
		SchemaVersion int            `json:"schema_version"`
		Trails        int            `json:"trails"`
		Rangers       int            `json:"rangers"`
		PendingJobs   map[string]int `json:"pending_jobs"`
	}
	if err := json.Unmarshal(ready.Data, &readyBody); err != nil {
		t.Fatalf("解析就绪响应失败: %v", err)
	}
	if readyBody.Status != "ready" || readyBody.SchemaVersion != 3 {
		t.Fatalf("就绪检查应校验 schema 版本: %+v", readyBody)
	}
	if readyBody.Trails != 2 || readyBody.Rangers != 1 {
		t.Fatalf("就绪检查应校验必要基础数据: %+v", readyBody)
	}
}

func TestHTTPPartyWorkflowEndToEnd(t *testing.T) {
	f := newHTTPFixture(t)
	leaderToken := f.token(leaderEmail, leaderPassword)
	rangerToken := f.token(rangerEmail, rangerPassword)

	created := f.mustSucceed(http.MethodPost, "/api/v1/parties", leaderToken, map[string]any{
		"trail_code":    valleyTrail,
		"hike_day":      f.hikeDay(1),
		"planned_start": f.plannedStart(1, 6).Format(time.RFC3339),
		"planned_end":   f.plannedStart(1, 15).Format(time.RFC3339),
		"size":          2,
		"contact_phone": "13800001234",
		"notes":         "溪谷穿越",
	}, http.StatusCreated)
	var party struct {
		Code  string `json:"code"`
		State string `json:"state"`
	}
	if err := json.Unmarshal(created.Data, &party); err != nil {
		t.Fatalf("解析建队响应失败: %v", err)
	}
	if party.State != string(domain.PartyDraft) {
		t.Fatalf("新建队伍状态错误: %+v", party)
	}

	batch := f.mustSucceed(http.MethodPost, fmt.Sprintf("/api/v1/parties/%s/members", party.Code), leaderToken, map[string]any{
		"members": []map[string]any{
			{"member_ref": "HK-1001", "display_name": "周岚", "phone": "13900000011", "waiver_signed": true},
		},
	}, http.StatusOK)
	var batchBody struct {
		Accepted int `json:"accepted"`
		Rejected int `json:"rejected"`
	}
	if err := json.Unmarshal(batch.Data, &batchBody); err != nil {
		t.Fatalf("解析登记响应失败: %v", err)
	}
	if batchBody.Accepted != 1 || batchBody.Rejected != 0 {
		t.Fatalf("登记结果错误: %+v", batchBody)
	}

	permitPath := fmt.Sprintf("/api/v1/parties/%s/permit-request", party.Code)
	first, firstBody := f.call(http.MethodPost, permitPath, leaderToken, nil, map[string]string{"Idempotency-Key": "http-idem-1"})
	if first.StatusCode != http.StatusOK {
		t.Fatalf("申请许可应成功，实际 %d %+v", first.StatusCode, firstBody.Error)
	}
	replay, replayBody := f.call(http.MethodPost, permitPath, leaderToken, nil, map[string]string{"Idempotency-Key": "http-idem-1"})
	if replay.StatusCode != http.StatusOK {
		t.Fatalf("重放幂等请求应成功，实际 %d", replay.StatusCode)
	}
	if !bytes.Equal(firstBody.Data, replayBody.Data) {
		t.Fatal("重放幂等请求应返回完全一致的响应体")
	}
	conflict, _ := f.call(http.MethodPost, permitPath, leaderToken, nil, nil)
	if conflict.StatusCode != http.StatusConflict {
		t.Fatalf("重复申请应返回 409，实际 %d", conflict.StatusCode)
	}

	f.mustSucceed(http.MethodPost, fmt.Sprintf("/api/v1/parties/%s/approve", party.Code), rangerToken, nil, http.StatusOK)
	f.mustSucceed(http.MethodPost, fmt.Sprintf("/api/v1/parties/%s/dispatch", party.Code), rangerToken, nil, http.StatusOK)

	f.clock.Set(f.plannedStart(1, 6).Add(30 * time.Minute))
	f.mustSucceed(http.MethodPost, fmt.Sprintf("/api/v1/parties/%s/checkpoints", party.Code), leaderToken, map[string]any{
		"seq": 1, "head_count": 2, "note": "溪口木桥",
	}, http.StatusCreated)
	f.clock.Set(f.plannedStart(1, 6).Add(280 * time.Minute))
	f.mustSucceed(http.MethodPost, fmt.Sprintf("/api/v1/parties/%s/checkpoints", party.Code), leaderToken, map[string]any{
		"seq": 3, "head_count": 2,
	}, http.StatusCreated)

	completed := f.mustSucceed(http.MethodPost, fmt.Sprintf("/api/v1/parties/%s/complete", party.Code), leaderToken, nil, http.StatusOK)
	var completedBody struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(completed.Data, &completedBody); err != nil {
		t.Fatalf("解析完成响应失败: %v", err)
	}
	if completedBody.State != string(domain.PartyCompleted) {
		t.Fatalf("完成后状态错误: %+v", completedBody)
	}

	listed := f.mustSucceed(http.MethodGet, "/api/v1/parties?state=completed&sort_by=hike_day&desc=true", leaderToken, nil, http.StatusOK)
	var page struct {
		Items []struct {
			Code string `json:"code"`
		} `json:"items"`
		Total      int `json:"total"`
		TotalPages int `json:"total_pages"`
	}
	if err := json.Unmarshal(listed.Data, &page); err != nil {
		t.Fatalf("解析列表响应失败: %v", err)
	}
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].Code != party.Code {
		t.Fatalf("列表结果错误: %+v", page)
	}

	settlement := f.mustSucceed(http.MethodGet, fmt.Sprintf("/api/v1/parties/%s/settlement", party.Code), leaderToken, nil, http.StatusOK)
	var settlementBody struct {
		State      string `json:"state"`
		TotalCents int64  `json:"total_cents"`
	}
	if err := json.Unmarshal(settlement.Data, &settlementBody); err != nil {
		t.Fatalf("解析结算响应失败: %v", err)
	}
	if settlementBody.State != string(domain.SettlementPending) || settlementBody.TotalCents <= 0 {
		t.Fatalf("结算记录错误: %+v", settlementBody)
	}
}

func TestHTTPCatalogAndPermitWindowEndpoints(t *testing.T) {
	f := newHTTPFixture(t)
	rangerToken := f.token(rangerEmail, rangerPassword)
	leaderToken := f.token(leaderEmail, leaderPassword)

	f.mustSucceed(http.MethodPost, "/api/v1/trails", rangerToken, validTrailPayload(), http.StatusCreated)
	trails := f.mustSucceed(http.MethodGet, "/api/v1/trails?sort_by=code", leaderToken, nil, http.StatusOK)
	var page struct {
		Total int `json:"total"`
	}
	if err := json.Unmarshal(trails.Data, &page); err != nil {
		t.Fatalf("解析线路列表失败: %v", err)
	}
	if page.Total != 3 {
		t.Fatalf("线路总数应为 3，实际 %d", page.Total)
	}

	detail := f.mustSucceed(http.MethodGet, "/api/v1/trails/"+valleyTrail, leaderToken, nil, http.StatusOK)
	var trail struct {
		Code        string `json:"code"`
		Checkpoints []struct {
			Seq       int  `json:"seq"`
			Mandatory bool `json:"mandatory"`
		} `json:"checkpoints"`
	}
	if err := json.Unmarshal(detail.Data, &trail); err != nil {
		t.Fatalf("解析线路详情失败: %v", err)
	}
	if trail.Code != valleyTrail || len(trail.Checkpoints) != 3 {
		t.Fatalf("线路详情错误: %+v", trail)
	}

	windowPath := "/api/v1/trails/" + valleyTrail + "/permit-windows"
	f.mustSucceed(http.MethodPost, windowPath, rangerToken, map[string]any{
		"hike_day": f.hikeDay(4), "quota_total": 20,
	}, http.StatusCreated)
	windows := f.mustSucceed(http.MethodGet, windowPath+"?from="+f.hikeDay(1)+"&to="+f.hikeDay(4), leaderToken, nil, http.StatusOK)
	var windowBody struct {
		Items []struct {
			HikeDay string `json:"hike_day"`
			Closed  bool   `json:"closed"`
		} `json:"items"`
	}
	if err := json.Unmarshal(windows.Data, &windowBody); err != nil {
		t.Fatalf("解析窗口列表失败: %v", err)
	}
	if len(windowBody.Items) != 4 {
		t.Fatalf("窗口数量应为 4，实际 %d", len(windowBody.Items))
	}

	closed := f.mustSucceed(http.MethodPost, windowPath+"/"+f.hikeDay(4)+"/close", rangerToken, nil, http.StatusOK)
	var closedBody struct {
		Closed bool `json:"closed"`
	}
	if err := json.Unmarshal(closed.Data, &closedBody); err != nil {
		t.Fatalf("解析关闭响应失败: %v", err)
	}
	if !closedBody.Closed {
		t.Fatalf("窗口应处于关闭状态: %+v", closedBody)
	}

	account := f.mustSucceed(http.MethodPost, "/api/v1/accounts", rangerToken, map[string]any{
		"email": "newleader@trailpermit.test", "display_name": "新领队",
		"password": "newleader2026", "role": "leader",
	}, http.StatusCreated)
	var accountBody struct {
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	if err := json.Unmarshal(account.Data, &accountBody); err != nil {
		t.Fatalf("解析建号响应失败: %v", err)
	}
	if accountBody.Role != string(domain.RoleLeader) {
		t.Fatalf("新账号角色错误: %+v", accountBody)
	}
	duplicate, _ := f.call(http.MethodPost, "/api/v1/accounts", rangerToken, map[string]any{
		"email": "newleader@trailpermit.test", "display_name": "重复",
		"password": "newleader2026", "role": "leader",
	}, nil)
	if duplicate.StatusCode != http.StatusConflict {
		t.Fatalf("重复邮箱应返回 409，实际 %d", duplicate.StatusCode)
	}
	weak, _ := f.call(http.MethodPost, "/api/v1/accounts", rangerToken, map[string]any{
		"email": "weak@trailpermit.test", "display_name": "弱密码",
		"password": "short", "role": "leader",
	}, nil)
	if weak.StatusCode != http.StatusBadRequest {
		t.Fatalf("弱密码应返回 400，实际 %d", weak.StatusCode)
	}
}

// validTrailPayload builds the JSON body of a valid trail registration.
func validTrailPayload() map[string]any {
	return map[string]any{
		"code":                "BEILING-TRAVERSE",
		"name":                "北岭横切线",
		"region":              "北岭保护区",
		"difficulty":          3,
		"distance_km":         21.4,
		"daily_quota":         18,
		"min_party_size":      2,
		"max_party_size":      6,
		"permit_cutoff_hours": 8,
		"checkpoints": []map[string]any{
			{"seq": 1, "name": "北岭入口", "cutoff_minutes": 80, "mandatory": true},
			{"seq": 2, "name": "云雾台", "cutoff_minutes": 220, "mandatory": true},
		},
	}
}
