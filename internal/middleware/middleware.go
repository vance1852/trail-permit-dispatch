// Package middleware provides the request pipeline: correlation identifiers,
// structured access logs, panic recovery, bearer authentication and role gates.
package middleware

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"time"

	"github.com/vance1852/trail-permit-dispatch/internal/apperr"
	"github.com/vance1852/trail-permit-dispatch/internal/domain"
	"github.com/vance1852/trail-permit-dispatch/internal/httpjson"
	"github.com/vance1852/trail-permit-dispatch/internal/logging"
)

// Middleware decorates an http.Handler.
type Middleware func(http.Handler) http.Handler

// Chain applies middlewares so the first entry is the outermost layer.
func Chain(handler http.Handler, middlewares ...Middleware) http.Handler {
	for i := len(middlewares) - 1; i >= 0; i-- {
		handler = middlewares[i](handler)
	}
	return handler
}

// Authenticator resolves a bearer token into an actor.
type Authenticator interface {
	Authenticate(ctx context.Context, token string) (domain.Actor, error)
}

// RequestID attaches an incoming or freshly minted correlation identifier.
func RequestID() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestID := r.Header.Get("X-Request-ID")
			if requestID == "" || len(requestID) > 64 {
				requestID = newRequestID()
			}
			ctx := logging.WithRequestID(r.Context(), requestID)
			w.Header().Set("X-Request-ID", requestID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// statusRecorder captures the response status for the access log.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (s *statusRecorder) WriteHeader(status int) {
	s.status = status
	s.ResponseWriter.WriteHeader(status)
}

func (s *statusRecorder) Write(payload []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	written, err := s.ResponseWriter.Write(payload)
	s.bytes += written
	return written, err
}

// AccessLog writes one structured line per request and puts a request scoped
// logger into the context.
func AccessLog(logger *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			started := time.Now()
			requestID := logging.RequestID(r.Context())
			scoped := logger.With(slog.String("request_id", requestID))
			ctx := logging.WithLogger(r.Context(), scoped)
			recorder := &statusRecorder{ResponseWriter: w}
			next.ServeHTTP(recorder, r.WithContext(ctx))
			if recorder.status == 0 {
				recorder.status = http.StatusOK
			}
			scoped.Info("http 请求",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", recorder.status),
				slog.Int("bytes", recorder.bytes),
				slog.Duration("elapsed", time.Since(started)),
			)
		})
	}
}

// Recovery converts a panic into a stable internal error response.
func Recovery(logger *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if recovered := recover(); recovered != nil {
					logger.Error("请求处理发生 panic",
						slog.String("request_id", logging.RequestID(r.Context())),
						slog.String("path", r.URL.Path),
						slog.Any("panic", recovered),
					)
					httpjson.WriteError(w, r, apperr.New(apperr.CodeInternal, "服务内部错误"))
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// Authenticate resolves the bearer token. When optional is true an anonymous
// request continues without an actor; otherwise it is rejected.
func Authenticate(authenticator Authenticator, optional bool) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := httpjson.BearerToken(r)
			if token == "" {
				if optional {
					next.ServeHTTP(w, r)
					return
				}
				httpjson.WriteError(w, r, apperr.New(apperr.CodeUnauthenticated, "缺少访问令牌"))
				return
			}
			actor, err := authenticator.Authenticate(r.Context(), token)
			if err != nil {
				httpjson.WriteError(w, r, err)
				return
			}
			next.ServeHTTP(w, r.WithContext(httpjson.WithActor(r.Context(), actor)))
		})
	}
}

// RequireRole rejects an actor without the expected business role.
func RequireRole(role domain.Role) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			actor, err := httpjson.RequireActor(r.Context())
			if err != nil {
				httpjson.WriteError(w, r, err)
				return
			}
			if err := actor.RequireRole(role); err != nil {
				httpjson.WriteError(w, r, err)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireAuthenticated rejects anonymous requests.
func RequireAuthenticated() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, err := httpjson.RequireActor(r.Context()); err != nil {
				httpjson.WriteError(w, r, err)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Timeout bounds the lifetime of a request context so a slow dependency cannot
// hold a connection forever.
func Timeout(limit time.Duration) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), limit)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// newRequestID mints a random correlation identifier.
func newRequestID() string {
	raw := make([]byte, 12)
	if _, err := rand.Read(raw); err != nil {
		return "req-fallback"
	}
	return "req-" + hex.EncodeToString(raw)
}
