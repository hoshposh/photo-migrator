package photos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrInsufficientScope reports that the saved sign-in lacks a permission the
// call needs (HTTP 403, insufficient authentication scopes). Not retried.
var ErrInsufficientScope = errors.New("the saved sign-in is missing a permission")

// ItemDate is Google's date for one media item, in request order.
type ItemDate struct {
	ID           string    // the requested ID
	CreationTime time.Time // mediaMetadata.creationTime; zero when Missing
	Missing      bool      // Google returned a status instead of the item
	Status       string    // that status message, for logging
}

// google.rpc.Code values that mean a media item is gone or unreadable.
const (
	statusNotFound         = 5
	statusPermissionDenied = 7
)

type mediaMetadata struct {
	CreationTime string `json:"creationTime"`
}

type batchGetResp struct {
	MediaItemResults []struct {
		MediaItem *struct {
			MediaMetadata mediaMetadata `json:"mediaMetadata"`
		} `json:"mediaItem"`
		Status *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"status"`
	} `json:"mediaItemResults"`
}

// parseCreationTime parses Google's RFC 3339 creationTime. An absent or
// unparsable value is the zero time, not an error.
func parseCreationTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// BatchGet fetches up to MaxBatchItems media items and returns Google's date
// for each, in the order of ids. It is a read, so it is not serialised with
// writeMu. A missing permission is reported as ErrInsufficientScope.
func (c *Client) BatchGet(ctx context.Context, ids []string) ([]ItemDate, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if len(ids) > MaxBatchItems {
		return nil, fmt.Errorf("batchGet: %d IDs, at most %d allowed", len(ids), MaxBatchItems)
	}

	q := url.Values{"mediaItemIds": ids}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL+"/mediaItems:batchGet?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.doWithRetry(ctx, req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusForbidden && isInsufficientScope(body) {
		return nil, fmt.Errorf("%w: %s", ErrInsufficientScope, body)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("batchGet HTTP %d: %s", resp.StatusCode, body)
	}

	var parsed batchGetResp
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("decode batchGet response: %w", err)
	}

	if len(parsed.MediaItemResults) != len(ids) {
		return nil, fmt.Errorf("batchGet: %d results for %d IDs", len(parsed.MediaItemResults), len(ids))
	}
	dates := make([]ItemDate, len(ids))
	for i, id := range ids {
		d := ItemDate{ID: id}
		r := parsed.MediaItemResults[i]
		if r.MediaItem != nil {
			d.CreationTime = parseCreationTime(r.MediaItem.MediaMetadata.CreationTime)
			dates[i] = d
			continue
		}
		// Only NOT_FOUND and PERMISSION_DENIED mean the item is gone or not
		// readable by this app. Anything else may be transient, so fail the
		// batch rather than let the caller record the item as missing.
		if r.Status == nil {
			return nil, fmt.Errorf("batchGet: item %s: result has neither a media item nor a status", id)
		}
		if r.Status.Code != statusNotFound && r.Status.Code != statusPermissionDenied {
			return nil, fmt.Errorf("batchGet: item %s: status code %d: %s", id, r.Status.Code, r.Status.Message)
		}
		d.Missing, d.Status = true, r.Status.Message
		dates[i] = d
	}
	return dates, nil
}

// isInsufficientScope reports whether a 403 body says the token's scopes are
// too narrow for the call.
func isInsufficientScope(body []byte) bool {
	s := string(body)
	return strings.Contains(strings.ToLower(s), "insufficient") ||
		strings.Contains(s, "ACCESS_TOKEN_SCOPE_INSUFFICIENT")
}
