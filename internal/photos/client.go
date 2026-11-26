// Package photos provides a thin client for the Google Photos Library REST API.
package photos

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"golang.org/x/oauth2"
)

const baseURL = "https://photoslibrary.googleapis.com/v1"

// ErrAuthFailed reports that Google rejected the saved sign-in (for example
// an expired or revoked refresh token, "invalid_grant"). Retrying cannot fix
// it, and every later request would fail the same way, so callers should stop.
var ErrAuthFailed = errors.New("google rejected the saved sign-in")

// Client wraps the Google Photos Library API.
type Client struct {
	http    *http.Client
	baseURL string
}

// New returns a Client that uses the provided *http.Client (which must already
// carry OAuth2 credentials via its Transport).
func New(httpClient *http.Client) *Client {
	return &Client{http: httpClient, baseURL: baseURL}
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

		resp, err := c.http.Do(req.WithContext(ctx))
		if err != nil {
			// The token refresh was rejected: not transient, don't retry.
			var rerr *oauth2.RetrieveError
			if errors.As(err, &rerr) {
				return nil, fmt.Errorf("%w (%w); run 'photo-migrator auth' to sign in again", ErrAuthFailed, err)
			}
			// Network error — retry if we have attempts left.
			if attempt < maxRetries {
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
			if waitErr := sleepOrCancel(ctx, backoff); waitErr != nil {
				return nil, waitErr
			}
			backoff = minDuration(backoff*2, 60*time.Second)
		default:
			return resp, nil
		}
	}
	return nil, fmt.Errorf("unreachable")
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
