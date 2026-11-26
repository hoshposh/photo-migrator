// Package photos provides a thin client for the Google Photos Library REST API.
package photos

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"golang.org/x/oauth2"
	"golang.org/x/time/rate"
)

const baseURL = "https://photoslibrary.googleapis.com/v1"

// ErrAuthFailed reports that Google rejected the saved sign-in (for example
// an expired or revoked refresh token, "invalid_grant"). Retrying cannot fix
// it, and every later request would fail the same way, so callers should stop.
var ErrAuthFailed = errors.New("google rejected the saved sign-in")

// maxRetryAfter caps how long a Retry-After header can make a request wait.
const maxRetryAfter = 2 * time.Minute

// Client wraps the Google Photos Library API. It is safe for concurrent use.
type Client struct {
	http    *http.Client
	baseURL string
	// limiter, if set, gates every HTTP request (including retries), so the
	// configured rate is requests per second, not files per second.
	limiter *rate.Limiter
	// writeMu serialises write calls (batchCreate, albums.create): Google's
	// upload guide says batchCreate for one user must be called one at a
	// time, and write calls share a per-user limit (HTTP 429 when exceeded).
	// Byte uploads stay parallel.
	writeMu sync.Mutex
}

// New returns a Client that uses the provided *http.Client (which must already
// carry OAuth2 credentials via its Transport).
func New(httpClient *http.Client, opts ...Option) *Client {
	c := &Client{http: httpClient, baseURL: baseURL}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Option configures a Client.
type Option func(*Client)

type loggerKey struct{}

// WithLogger returns a context whose API requests log their retries through
// l. Callers pass their per-file logger, so retry lines carry the same
// fields (file, album, zip) as the rest of that file's log.
func WithLogger(ctx context.Context, l *zap.Logger) context.Context {
	return context.WithValue(ctx, loggerKey{}, l)
}

func loggerFrom(ctx context.Context) *zap.Logger {
	if l, ok := ctx.Value(loggerKey{}).(*zap.Logger); ok && l != nil {
		return l
	}
	return zap.NewNop()
}

// callName names an API request for logs: upload, batchCreate, createAlbum.
func callName(req *http.Request) string {
	switch p := path.Base(req.URL.Path); {
	case p == "uploads":
		return "upload"
	case strings.HasSuffix(p, ":batchCreate"):
		return "batchCreate"
	case p == "albums":
		return "createAlbum"
	default:
		return p
	}
}

// WithRateLimit limits the client to perSecond API requests per second,
// counting every request and retry. perSecond <= 0 means no limit.
func WithRateLimit(perSecond float64) Option {
	return func(c *Client) {
		if perSecond > 0 {
			c.limiter = rate.NewLimiter(rate.Limit(perSecond), 1)
		}
	}
}

// doWithRetry executes an HTTP request, retrying with exponential backoff on
// HTTP 429 (rate limit) and 5xx (server) errors.
//
// If the request has a non-nil GetBody, the body is reset before each retry.
func (c *Client) doWithRetry(ctx context.Context, req *http.Request) (*http.Response, error) {
	const maxRetries = 6
	backoff := time.Second

	for attempt := 0; attempt <= maxRetries; attempt++ {
		// Reset body for retries.
		if attempt > 0 && req.GetBody != nil {
			body, err := req.GetBody()
			if err != nil {
				return nil, fmt.Errorf("reset request body: %w", err)
			}
			req.Body = body
		}

		if c.limiter != nil {
			if err := c.limiter.Wait(ctx); err != nil {
				return nil, err
			}
		}
		resp, err := c.http.Do(req.WithContext(ctx))
		if err != nil {
			// The token refresh was rejected: not transient, don't retry.
			var rerr *oauth2.RetrieveError
			if errors.As(err, &rerr) {
				return nil, fmt.Errorf("%w (%w); run 'photo-migrator auth' to sign in again", ErrAuthFailed, err)
			}
			// Network error — retry if we have attempts left.
			if attempt < maxRetries {
				loggerFrom(ctx).Warn("retrying after network error",
					zap.String("call", callName(req)), zap.Int("attempt", attempt+1),
					zap.String("wait", backoff.String()), zap.Error(err))
				if waitErr := sleepOrCancel(ctx, backoff); waitErr != nil {
					return nil, waitErr
				}
				backoff = minDuration(backoff*2, 60*time.Second)
				continue
			}
			return nil, err
		}

		switch {
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			_ = resp.Body.Close()
			if attempt == maxRetries {
				return nil, fmt.Errorf("max retries exceeded: HTTP %d", resp.StatusCode)
			}
			// Wait at least as long as Google asks (Retry-After), if it does.
			wait := backoff
			ra := retryAfter(resp.Header.Get("Retry-After"), time.Now())
			if ra > wait {
				wait = minDuration(ra, maxRetryAfter)
			}
			fields := []zap.Field{
				zap.String("call", callName(req)), zap.Int("status", resp.StatusCode),
				zap.Int("attempt", attempt+1), zap.String("wait", wait.String()),
			}
			if ra > 0 {
				fields = append(fields, zap.String("retry_after", ra.String()))
			}
			loggerFrom(ctx).Warn(fmt.Sprintf("retrying after HTTP %d", resp.StatusCode), fields...)
			if waitErr := sleepOrCancel(ctx, wait); waitErr != nil {
				return nil, waitErr
			}
			backoff = minDuration(backoff*2, 60*time.Second)
		default:
			return resp, nil
		}
	}
	return nil, fmt.Errorf("unreachable")
}

// retryAfter parses a Retry-After header value, either delay-seconds or an
// HTTP date, returning 0 if it is absent or invalid.
func retryAfter(v string, now time.Time) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil && t.After(now) {
		return t.Sub(now)
	}
	return 0
}

func sleepOrCancel(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
