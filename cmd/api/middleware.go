package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/ezechidc/greenlight/internal/data"
	"github.com/ezechidc/greenlight/internal/validator"
	"golang.org/x/time/rate"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/tomasen/realip"
)

type statusRecorder struct {
	http.ResponseWriter
	status  int
	errBody []byte
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	// Try to extract error message from response for 4xx
	if r.status >= 400 && r.status < 500 {
		r.errBody = append([]byte{}, b...)
	}
	return r.ResponseWriter.Write(b)
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (app *application) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				w.Header().Set("Connection", "close")
				app.serverErrorResponse(w, r, fmt.Errorf("%s", err))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (app *application) logRequestDuration(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ip := realip.FromRequest(r)
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(rec, r)
		duration := time.Since(start)

		msg := "request complete"
		fields := []any{
			"method", r.Method,
			"url", r.URL.String(),
			"status", rec.status,
			"source_ip", ip,
			"duration", fmt.Sprintf("%d ms", duration.Milliseconds()),
		}

		if rec.status >= 500 {
			msg = "internal server error"
			fields = append(fields, "stack", string(debug.Stack()))
			app.logger.Error(msg, fields...)
		} else if rec.status >= 400 {
			msg = "client error"
			// Try to extract "error" key from JSON response body
			var resp map[string]interface{}
			var errValue string
			if len(rec.errBody) > 0 {
				if err := json.Unmarshal(rec.errBody, &resp); err == nil {
					if val, ok := resp["error"].(string); ok {
						errValue = val
					}
				}
			}
			if errValue != "" {
				fields = append(fields, "error", errValue)
			} else if len(rec.errBody) > 0 {
				fields = append(fields, "error_message", string(rec.errBody))
			}
			app.logger.Warn(msg, fields...)
		} else {
			app.logger.Info(msg, fields...)
		}
	})
}

func (app *application) rateLimit(next http.Handler) http.Handler {
	type client struct {
		limiter  *rate.Limiter
		lastSeen time.Time
	}

	var (
		mu      sync.Mutex
		clients = make(map[string]*client)
	)
	go func() {
		for {
			time.Sleep(time.Minute)
			mu.Lock()
			for ip, client := range clients {
				if time.Since(client.lastSeen) > 3*time.Minute {
					delete(clients, ip)
				}
			}
			mu.Unlock()
		}
	}()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if app.config.limiter.enabled {
			ip := realip.FromRequest(r)
			mu.Lock()
			defer mu.Unlock()
			if _, ok := clients[ip]; !ok {
				clients[ip] = &client{
					limiter: rate.NewLimiter(rate.Limit(app.config.limiter.rps), app.config.limiter.burst),
				}
			}
			clients[ip].lastSeen = time.Now()
			if !clients[ip].limiter.Allow() {

				app.rateLimitExceededResponse(w, r)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (app *application) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Authorization")
		authorizationHeader := r.Header.Get("Authorization")
		if authorizationHeader == "" {
			r = app.contextSetUser(r, data.AnonymousUser)
			next.ServeHTTP(w, r)
			return
		}

		headerParts := strings.Split(authorizationHeader, " ")
		if len(headerParts) != 2 || headerParts[0] != "Bearer" {
			app.invalidAuthenticationTokenResponse(w, r)
			return
		}

		token := headerParts[1]
		v := validator.New()

		if data.ValidateTokenPlaintext(v, token); !v.Valid() {
			app.invalidAuthenticationTokenResponse(w, r)
			return
		}

		user, err := app.models.Users.GetForToken(data.ScopeAuthentication, token)
		if err != nil {
			switch {
			case errors.Is(err, data.ErrRecordNotFound):
				app.invalidAuthenticationTokenResponse(w, r)
			default:
				app.serverErrorResponse(w, r, err)
			}
			return
		}
		r = app.contextSetUser(r, user)
		next.ServeHTTP(w, r)
	})
}

func (app *application) requireAuthenticatedUser(next http.HandlerFunc) http.HandlerFunc {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := app.contextGetUser(r)
		if user.IsAnonymous() {
			app.authenticationRequiredResponse(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (app *application) requireActivatedUser(next http.HandlerFunc) http.HandlerFunc {
	// Rather than returning this http.HandlerFunc we assign it to the variable fn.
	fn := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := app.contextGetUser(r)
		// Check that a user is activated.
		if !user.Activated {
			app.inactiveAccountResponse(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
	// Wrap fn with the requireAuthenticatedUser() middleware before returning it.
	return app.requireAuthenticatedUser(fn)
}
