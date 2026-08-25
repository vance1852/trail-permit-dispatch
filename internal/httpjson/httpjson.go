// Package httpjson holds the JSON envelope, the request decoding rules and the
// actor context shared by the middleware chain and the API handlers.
package httpjson

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/vance1852/trail-permit-dispatch/internal/apperr"
	"github.com/vance1852/trail-permit-dispatch/internal/domain"
	"github.com/vance1852/trail-permit-dispatch/internal/logging"
)

// maxBodyBytes bounds the accepted request body size.
const maxBodyBytes = 1 << 20

// ErrorBody is the stable error payload of the API.
type ErrorBody struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Field     string `json:"field,omitempty"`
	RequestID string `json:"request_id,omitempty"`
}

// Envelope wraps every response so clients can branch on data or error.
type Envelope struct {
	Data  any        `json:"data,omitempty"`
	Error *ErrorBody `json:"error,omitempty"`
}

// WriteData renders a successful response.
func WriteData(w http.ResponseWriter, r *http.Request, status int, payload any) {
	write(w, status, Envelope{Data: payload}, r)
}

// WriteError renders a failure response derived from the stable error code.
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	code := apperr.CodeOf(err)
	body := &ErrorBody{
		Code:    string(code),
		Message: apperr.Message(err),
		Field:   apperr.FieldOf(err),
	}
	if r != nil {
		body.RequestID = logging.RequestID(r.Context())
	}
	write(w, apperr.HTTPStatus(code), Envelope{Error: body}, r)
}

func write(w http.ResponseWriter, status int, envelope Envelope, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if r != nil {
		if requestID := logging.RequestID(r.Context()); requestID != "" {
			w.Header().Set("X-Request-ID", requestID)
		}
	}
	w.WriteHeader(status)
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(envelope); err != nil && r != nil {
		logging.FromContext(r.Context()).Error("写出响应失败", "error", err.Error())
	}
}

// Decode parses a JSON request body, rejecting unknown fields and trailing data.
func Decode(r *http.Request, target any) error {
	if r.Body == nil {
		return apperr.New(apperr.CodeInvalidArgument, "请求体不能为空")
	}
	decoder := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxBodyBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		if errors.Is(err, io.EOF) {
			return apperr.New(apperr.CodeInvalidArgument, "请求体不能为空")
		}
		return apperr.Wrap(apperr.CodeInvalidArgument, "请求体不是合法的 JSON 或包含未知字段", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return apperr.New(apperr.CodeInvalidArgument, "请求体只能包含一个 JSON 对象")
	}
	return nil
}

// actorKey carries the authenticated actor through the request context.
type actorKey struct{}

// WithActor stores the authenticated actor in the context.
func WithActor(ctx context.Context, actor domain.Actor) context.Context {
	return context.WithValue(ctx, actorKey{}, actor)
}

// ActorFrom reads the authenticated actor of the context.
func ActorFrom(ctx context.Context) (domain.Actor, bool) {
	actor, ok := ctx.Value(actorKey{}).(domain.Actor)
	return actor, ok && actor.UserID != 0
}

// RequireActor returns the authenticated actor or an unauthenticated error.
func RequireActor(ctx context.Context) (domain.Actor, error) {
	actor, ok := ActorFrom(ctx)
	if !ok {
		return domain.Actor{}, apperr.New(apperr.CodeUnauthenticated, "请先登录")
	}
	return actor, nil
}

// BearerToken extracts the bearer token of a request.
func BearerToken(r *http.Request) string {
	header := r.Header.Get("Authorization")
	if header == "" {
		return ""
	}
	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return strings.TrimSpace(parts[1])
}

// QueryInt reads an optional integer query parameter.
func QueryInt(r *http.Request, name string, fallback int) (int, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil {
		return 0, apperr.Wrapf(apperr.CodeInvalidArgument, err, "查询参数 %s 必须是整数", name).WithField(name)
	}
	return parsed, nil
}

// QueryInt64 reads a required positive int64 path or query value.
func QueryInt64(raw, name string) (int64, error) {
	parsed, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || parsed <= 0 {
		return 0, apperr.Newf(apperr.CodeInvalidArgument, "%s 必须是正整数", name).WithField(name)
	}
	return parsed, nil
}

// QueryBool reads an optional boolean query parameter.
func QueryBool(r *http.Request, name string, fallback bool) (bool, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseBool(raw)
	if err != nil {
		return false, apperr.Wrapf(apperr.CodeInvalidArgument, err, "查询参数 %s 必须是布尔值", name).WithField(name)
	}
	return parsed, nil
}

// QueryCSV reads a comma separated list query parameter.
func QueryCSV(r *http.Request, name string) []string {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	values := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			values = append(values, trimmed)
		}
	}
	return values
}

// PageRequest builds a validated pagination instruction from the query string.
func PageRequest(r *http.Request, allowed []string) (domain.PageRequest, error) {
	page, err := QueryInt(r, "page", 1)
	if err != nil {
		return domain.PageRequest{}, err
	}
	size, err := QueryInt(r, "size", 0)
	if err != nil {
		return domain.PageRequest{}, err
	}
	desc, err := QueryBool(r, "desc", false)
	if err != nil {
		return domain.PageRequest{}, err
	}
	return domain.NewPageRequest(page, size, r.URL.Query().Get("sort_by"), desc, allowed)
}
