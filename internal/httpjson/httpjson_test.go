package httpjson

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vance1852/trail-permit-dispatch/internal/apperr"
	"github.com/vance1852/trail-permit-dispatch/internal/domain"
	"github.com/vance1852/trail-permit-dispatch/internal/logging"
)

type sampleBody struct {
	Name string `json:"name"`
	Size int    `json:"size"`
}

func newRequest(method, target, body string) *http.Request {
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequest(method, target, reader)
	return request.WithContext(logging.WithRequestID(request.Context(), "req-unit"))
}

func TestDecodeRejectsUnknownAndTrailingContent(t *testing.T) {
	var target sampleBody
	if err := Decode(newRequest(http.MethodPost, "/", `{"name":"清溪","size":3}`), &target); err != nil {
		t.Fatalf("合法请求体被拒绝: %v", err)
	}
	if target.Name != "清溪" || target.Size != 3 {
		t.Fatalf("解析结果错误: %+v", target)
	}
	if err := Decode(newRequest(http.MethodPost, "/", `{"name":"清溪","extra":1}`), &sampleBody{}); !apperr.Is(err, apperr.CodeInvalidArgument) {
		t.Fatalf("未知字段应被拒绝，实际 %v", err)
	}
	if err := Decode(newRequest(http.MethodPost, "/", `{"name":"清溪"}{"name":"再来"}`), &sampleBody{}); !apperr.Is(err, apperr.CodeInvalidArgument) {
		t.Fatalf("多个 JSON 对象应被拒绝，实际 %v", err)
	}
	if err := Decode(newRequest(http.MethodPost, "/", ""), &sampleBody{}); !apperr.Is(err, apperr.CodeInvalidArgument) {
		t.Fatalf("空请求体应被拒绝，实际 %v", err)
	}
	if err := Decode(newRequest(http.MethodPost, "/", "not json"), &sampleBody{}); !apperr.Is(err, apperr.CodeInvalidArgument) {
		t.Fatalf("非法 JSON 应被拒绝，实际 %v", err)
	}
}

func TestWriteDataAndWriteError(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := newRequest(http.MethodGet, "/", "")
	WriteData(recorder, request, http.StatusCreated, sampleBody{Name: "清溪", Size: 2})
	if recorder.Code != http.StatusCreated {
		t.Fatalf("状态码错误: %d", recorder.Code)
	}
	if got := recorder.Header().Get("X-Request-ID"); got != "req-unit" {
		t.Fatalf("响应应回显请求标识，实际 %q", got)
	}
	var success struct {
		Data sampleBody `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &success); err != nil {
		t.Fatalf("解析成功响应失败: %v", err)
	}
	if success.Data.Name != "清溪" {
		t.Fatalf("成功响应内容错误: %+v", success)
	}

	recorder = httptest.NewRecorder()
	WriteError(recorder, request, apperr.New(apperr.CodeQuotaExhausted, "名额不足").WithField("size"))
	if recorder.Code != http.StatusConflict {
		t.Fatalf("名额不足应映射为 409，实际 %d", recorder.Code)
	}
	var failure struct {
		Error ErrorBody `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &failure); err != nil {
		t.Fatalf("解析错误响应失败: %v", err)
	}
	if failure.Error.Code != string(apperr.CodeQuotaExhausted) || failure.Error.Field != "size" {
		t.Fatalf("错误响应内容错误: %+v", failure.Error)
	}
	if failure.Error.RequestID != "req-unit" {
		t.Fatalf("错误响应应包含请求标识: %+v", failure.Error)
	}
}

func TestBearerTokenExtraction(t *testing.T) {
	request := newRequest(http.MethodGet, "/", "")
	if BearerToken(request) != "" {
		t.Fatal("缺少认证头时应返回空令牌")
	}
	request.Header.Set("Authorization", "Bearer abc.def")
	if BearerToken(request) != "abc.def" {
		t.Fatalf("令牌解析错误: %q", BearerToken(request))
	}
	request.Header.Set("Authorization", "bearer   spaced  ")
	if BearerToken(request) != "spaced" {
		t.Fatalf("应忽略大小写与空白: %q", BearerToken(request))
	}
	request.Header.Set("Authorization", "Basic abc")
	if BearerToken(request) != "" {
		t.Fatal("非 Bearer 认证不应被接受")
	}
	request.Header.Set("Authorization", "Bearer")
	if BearerToken(request) != "" {
		t.Fatal("缺少令牌值时应返回空")
	}
}

func TestActorContext(t *testing.T) {
	request := newRequest(http.MethodGet, "/", "")
	if _, ok := ActorFrom(request.Context()); ok {
		t.Fatal("未认证的上下文不应包含操作者")
	}
	if _, err := RequireActor(request.Context()); !apperr.Is(err, apperr.CodeUnauthenticated) {
		t.Fatalf("匿名上下文应返回 unauthenticated，实际 %v", err)
	}
	ctx := WithActor(request.Context(), domain.Actor{UserID: 5, Role: domain.RoleRanger})
	actor, ok := ActorFrom(ctx)
	if !ok || actor.UserID != 5 || actor.Role != domain.RoleRanger {
		t.Fatalf("操作者上下文错误: %+v", actor)
	}
	if _, err := RequireActor(ctx); err != nil {
		t.Fatalf("已认证上下文不应报错: %v", err)
	}
	anonymous := WithActor(request.Context(), domain.Actor{})
	if _, ok := ActorFrom(anonymous); ok {
		t.Fatal("零值操作者不应视为已认证")
	}
}

func TestQueryHelpers(t *testing.T) {
	request := newRequest(http.MethodGet, "/api/v1/parties?page=3&size=10&desc=true&state=draft,%20on_trail&empty=", "")
	page, err := QueryInt(request, "page", 1)
	if err != nil || page != 3 {
		t.Fatalf("整数参数解析错误: %d (%v)", page, err)
	}
	fallback, err := QueryInt(request, "missing", 7)
	if err != nil || fallback != 7 {
		t.Fatalf("缺省值未生效: %d (%v)", fallback, err)
	}
	if _, err := QueryInt(request, "state", 0); !apperr.Is(err, apperr.CodeInvalidArgument) {
		t.Fatalf("非整数参数应被拒绝，实际 %v", err)
	}
	desc, err := QueryBool(request, "desc", false)
	if err != nil || !desc {
		t.Fatalf("布尔参数解析错误: %v (%v)", desc, err)
	}
	if _, err := QueryBool(request, "state", false); !apperr.Is(err, apperr.CodeInvalidArgument) {
		t.Fatalf("非布尔参数应被拒绝，实际 %v", err)
	}
	states := QueryCSV(request, "state")
	if len(states) != 2 || states[0] != "draft" || states[1] != "on_trail" {
		t.Fatalf("列表参数解析错误: %+v", states)
	}
	if QueryCSV(request, "empty") != nil {
		t.Fatal("空列表参数应返回 nil")
	}
	if _, err := QueryInt64("0", "id"); !apperr.Is(err, apperr.CodeInvalidArgument) {
		t.Fatalf("非正整数标识应被拒绝，实际 %v", err)
	}
	id, err := QueryInt64(" 42 ", "id")
	if err != nil || id != 42 {
		t.Fatalf("标识解析错误: %d (%v)", id, err)
	}
}

func TestPageRequestFromQuery(t *testing.T) {
	allowed := []string{"hike_day", "created_at"}
	request := newRequest(http.MethodGet, "/?page=2&size=5&sort_by=created_at&desc=true", "")
	page, err := PageRequest(request, allowed)
	if err != nil {
		t.Fatalf("分页解析失败: %v", err)
	}
	if page.Page != 2 || page.Size != 5 || page.SortBy != "created_at" || !page.SortDesc {
		t.Fatalf("分页解析结果错误: %+v", page)
	}
	if page.Offset() != 5 {
		t.Fatalf("偏移量错误: %d", page.Offset())
	}
	if _, err := PageRequest(newRequest(http.MethodGet, "/?sort_by=leader_id", ""), allowed); !apperr.Is(err, apperr.CodeInvalidArgument) {
		t.Fatalf("白名单外排序字段应被拒绝，实际 %v", err)
	}
	if _, err := PageRequest(newRequest(http.MethodGet, "/?page=abc", ""), allowed); !apperr.Is(err, apperr.CodeInvalidArgument) {
		t.Fatalf("非法页码应被拒绝，实际 %v", err)
	}
	if _, err := PageRequest(newRequest(http.MethodGet, "/?desc=maybe", ""), allowed); !apperr.Is(err, apperr.CodeInvalidArgument) {
		t.Fatalf("非法排序方向应被拒绝，实际 %v", err)
	}
	defaults, err := PageRequest(newRequest(http.MethodGet, "/", ""), allowed)
	if err != nil {
		t.Fatalf("默认分页失败: %v", err)
	}
	if defaults.Page != 1 || defaults.SortBy != "hike_day" || defaults.SortDesc {
		t.Fatalf("默认分页结果错误: %+v", defaults)
	}
}
