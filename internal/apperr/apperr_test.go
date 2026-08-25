package apperr

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
)

func TestCodeOfUnwrapsNestedErrors(t *testing.T) {
	cause := errors.New("底层驱动错误")
	wrapped := Wrap(CodeUnavailable, "数据库不可用", cause)
	if CodeOf(wrapped) != CodeUnavailable {
		t.Fatalf("包装后的错误码不正确: %s", CodeOf(wrapped))
	}
	if !errors.Is(wrapped, cause) {
		t.Fatal("包装错误必须保留原始错误链")
	}
	deeper := fmt.Errorf("服务层: %w", wrapped)
	if CodeOf(deeper) != CodeUnavailable {
		t.Fatalf("跨层包装后仍应可识别错误码: %s", CodeOf(deeper))
	}
	if CodeOf(nil) != "" {
		t.Fatal("空错误不应有错误码")
	}
	if CodeOf(errors.New("未分类")) != CodeInternal {
		t.Fatal("未分类错误应视为内部错误")
	}
	if CodeOf(context.Canceled) != CodeDeadlineExceeded {
		t.Fatal("上下文取消应映射为 deadline_exceeded")
	}
	if CodeOf(fmt.Errorf("查询: %w", context.DeadlineExceeded)) != CodeDeadlineExceeded {
		t.Fatal("包装后的超时应映射为 deadline_exceeded")
	}
}

func TestFieldAndMessageHelpers(t *testing.T) {
	err := New(CodeInvalidArgument, "出行日期不合法").WithField("hike_day")
	if FieldOf(err) != "hike_day" {
		t.Fatalf("字段定位错误: %q", FieldOf(err))
	}
	if Message(err) != "出行日期不合法" {
		t.Fatalf("消息不正确: %q", Message(err))
	}
	if FieldOf(errors.New("普通错误")) != "" {
		t.Fatal("普通错误不应携带字段")
	}
	if Message(errors.New("驱动细节")) != "服务内部错误" {
		t.Fatal("未分类错误不应向调用方泄漏细节")
	}
	if Message(context.Canceled) != "请求已取消或超时" {
		t.Fatalf("取消错误的提示不正确: %q", Message(context.Canceled))
	}
	original := New(CodeConflict, "冲突")
	clone := original.WithField("code")
	if original.Field != "" {
		t.Fatal("WithField 不应修改原始错误")
	}
	if clone.Field != "code" || clone.Code != CodeConflict {
		t.Fatalf("克隆结果错误: %+v", clone)
	}
}

func TestErrorStringIncludesCause(t *testing.T) {
	plain := New(CodeNotFound, "队伍不存在")
	if plain.Error() != "not_found: 队伍不存在" {
		t.Fatalf("错误文本不正确: %q", plain.Error())
	}
	wrapped := Wrapf(CodeUnavailable, errors.New("io timeout"), "读取 %s 失败", "队伍")
	if wrapped.Error() != "unavailable: 读取 队伍 失败: io timeout" {
		t.Fatalf("包装错误文本不正确: %q", wrapped.Error())
	}
	var empty *Error
	if empty.Error() != "" || empty.Unwrap() != nil || empty.WithField("x") != nil {
		t.Fatal("空错误的方法必须安全返回")
	}
}

func TestHTTPStatusMapping(t *testing.T) {
	cases := map[Code]int{
		CodeInvalidArgument:  http.StatusBadRequest,
		CodeUnauthenticated:  http.StatusUnauthorized,
		CodePermissionDenied: http.StatusForbidden,
		CodeNotFound:         http.StatusNotFound,
		CodeConflict:         http.StatusConflict,
		CodeStateInvalid:     http.StatusConflict,
		CodeVersionConflict:  http.StatusConflict,
		CodeQuotaExhausted:   http.StatusConflict,
		CodeDeadlineExceeded: http.StatusRequestTimeout,
		CodeUnavailable:      http.StatusServiceUnavailable,
		CodeInternal:         http.StatusInternalServerError,
		Code("unknown"):      http.StatusInternalServerError,
	}
	for code, expected := range cases {
		if got := HTTPStatus(code); got != expected {
			t.Fatalf("%s 应映射为 %d，实际 %d", code, expected, got)
		}
	}
}

func TestRetryableClassification(t *testing.T) {
	retryable := []Code{CodeVersionConflict, CodeUnavailable, CodeDeadlineExceeded}
	for _, code := range retryable {
		if !Retryable(New(code, "x")) {
			t.Fatalf("%s 应允许重试", code)
		}
	}
	permanent := []Code{CodeInvalidArgument, CodeNotFound, CodeConflict, CodeStateInvalid, CodePermissionDenied, CodeInternal}
	for _, code := range permanent {
		if Retryable(New(code, "x")) {
			t.Fatalf("%s 不应重试", code)
		}
	}
}

func TestIsHelper(t *testing.T) {
	err := Newf(CodeQuotaExhausted, "%s 名额不足", "2026-09-12")
	if !Is(err, CodeQuotaExhausted) {
		t.Fatal("Is 应识别自身错误码")
	}
	if Is(err, CodeConflict) {
		t.Fatal("Is 不应混淆不同错误码")
	}
	if Is(nil, CodeInternal) {
		t.Fatal("空错误不应匹配任何错误码")
	}
}
