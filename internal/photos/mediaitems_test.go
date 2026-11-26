package photos

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// batchGetClient starts a server that answers every request with h and counts
// the requests.
func batchGetClient(t *testing.T, h http.HandlerFunc) (*Client, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	c := New(srv.Client())
	c.baseURL = srv.URL + "/v1"
	return c, &calls
}

func TestBatchGet_ReturnsDatesInRequestOrder(t *testing.T) {
	c, _ := batchGetClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/mediaItems:batchGet" {
			t.Errorf("request = %s %s, want GET /v1/mediaItems:batchGet", r.Method, r.URL.Path)
		}
		want := url.Values{"mediaItemIds": {"a", "b"}}
		if got := r.URL.Query(); !reflect.DeepEqual(got, want) {
			t.Errorf("query = %v, want %v", got, want)
		}
		_, _ = w.Write([]byte(`{"mediaItemResults":[{"mediaItem":{"id":"a","mediaMetadata":{"creationTime":"2019-06-14T18:02:00Z"}}},{"status":{"code":5,"message":"Not found"}}]}`))
	})

	got, err := c.BatchGet(context.Background(), []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	want := []ItemDate{
		{ID: "a", CreationTime: time.Date(2019, 6, 14, 18, 2, 0, 0, time.UTC)},
		{ID: "b", Missing: true, Status: "Not found"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d results, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].ID != want[i].ID || !got[i].CreationTime.Equal(want[i].CreationTime) ||
			got[i].Missing != want[i].Missing || got[i].Status != want[i].Status {
			t.Errorf("result %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestBatchGet_TooManyIDs(t *testing.T) {
	c, calls := batchGetClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})

	ids := make([]string, MaxBatchItems+1)
	for i := range ids {
		ids[i] = "x"
	}
	if _, err := c.BatchGet(context.Background(), ids); err == nil {
		t.Error("51 IDs: got nil error, want an error")
	}
	got, err := c.BatchGet(context.Background(), nil)
	if got != nil || err != nil {
		t.Errorf("no IDs: got (%v, %v), want (nil, nil)", got, err)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("%d requests sent, want none", n)
	}
}

func TestBatchGet_InsufficientScope(t *testing.T) {
	c, calls := batchGetClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":403,"message":"Request had insufficient authentication scopes.","status":"PERMISSION_DENIED"}}`))
	})

	_, err := c.BatchGet(context.Background(), []string{"a"})
	if !errors.Is(err, ErrInsufficientScope) {
		t.Errorf("err = %v, want ErrInsufficientScope", err)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("%d requests, want 1 (not retried)", n)
	}
}

func TestBatchGet_OtherErrorIsNotInsufficientScope(t *testing.T) {
	c, _ := batchGetClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":400,"message":"bad id"}}`))
	})

	_, err := c.BatchGet(context.Background(), []string{"a"})
	if err == nil || errors.Is(err, ErrInsufficientScope) || !strings.Contains(err.Error(), "batchGet HTTP 400") {
		t.Errorf("err = %v, want a plain 'batchGet HTTP 400' error", err)
	}
}

func TestBatchGet_RetriesOn429(t *testing.T) {
	var n atomic.Int32
	c, calls := batchGetClient(t, func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"mediaItemResults":[{"mediaItem":{"id":"a","mediaMetadata":{"creationTime":"2019-06-14T18:02:00Z"}}}]}`))
	})

	got, err := c.BatchGet(context.Background(), []string{"a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Missing {
		t.Errorf("got %+v, want one found item", got)
	}
	if calls.Load() != 2 {
		t.Errorf("%d requests, want 2", calls.Load())
	}
}

func TestBatchGet_ShortResponseIsAnError(t *testing.T) {
	c, _ := batchGetClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"mediaItemResults":[{"mediaItem":{"id":"a","mediaMetadata":{"creationTime":"2019-06-14T18:02:00Z"}}}]}`))
	})

	got, err := c.BatchGet(context.Background(), []string{"a", "b"})
	if err == nil || !strings.Contains(err.Error(), "1 results for 2 IDs") {
		t.Errorf("err = %v, want a '1 results for 2 IDs' error", err)
	}
	if got != nil {
		t.Errorf("got %+v, want no results", got)
	}
}

func TestBatchGet_ItemStatusCodes(t *testing.T) {
	cases := []struct {
		code    int
		missing bool
	}{
		{3, true},
		{5, true},
		{7, true},
		{14, false},
	}
	for _, tc := range cases {
		c, _ := batchGetClient(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = fmt.Fprintf(w, `{"mediaItemResults":[{"status":{"code":%d,"message":"msg"}}]}`, tc.code)
		})
		got, err := c.BatchGet(context.Background(), []string{"a"})
		if tc.missing {
			if err != nil || len(got) != 1 || !got[0].Missing || got[0].Status != "msg" {
				t.Errorf("code %d: got (%+v, %v), want one Missing item", tc.code, got, err)
			}
			continue
		}
		if err == nil || got != nil {
			t.Fatalf("code %d: got (%+v, %v), want an error and no results", tc.code, got, err)
		}
		for _, want := range []string{"a", "14", "msg"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("code %d: error %q does not mention %q", tc.code, err, want)
			}
		}
	}
}

// Break caught: a request whose answer never comes hanging the run forever
// (a real run hung for 8 hours) instead of being retried.
func TestBatchGet_RetriesAfterTimeout(t *testing.T) {
	release := make(chan struct{})
	var n atomic.Int32
	c, calls := batchGetClient(t, func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			<-release // never answers while the test runs
			return
		}
		_, _ = w.Write([]byte(`{"mediaItemResults":[{"mediaItem":{"id":"a","mediaMetadata":{"creationTime":"2019-06-14T18:02:00Z"}}}]}`))
	})
	t.Cleanup(func() { close(release) }) // runs before the server is closed
	c.callTimeout = 100 * time.Millisecond

	got, err := c.BatchGet(context.Background(), []string{"a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Missing {
		t.Errorf("got %+v, want one found item", got)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("%d requests, want 2", n)
	}
}

// Break caught: the photo's link being dropped from the batchGet answer, so
// the report cannot link to it.
func TestBatchGet_ProductURL(t *testing.T) {
	c, _ := batchGetClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"mediaItemResults":[` +
			`{"mediaItem":{"id":"a","productUrl":"https://photos.google.com/lr/photo/AAA","mediaMetadata":{"creationTime":"2019-06-14T18:02:00Z"}}},` +
			`{"mediaItem":{"id":"b","mediaMetadata":{"creationTime":"2019-06-14T18:02:00Z"}}},` +
			`{"status":{"code":5,"message":"Not found"}}]}`))
	})
	got, err := c.BatchGet(context.Background(), []string{"a", "b", "c"})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].ProductURL != "https://photos.google.com/lr/photo/AAA" || got[1].ProductURL != "" || got[2].ProductURL != "" {
		t.Errorf("ProductURLs = %q, %q, %q", got[0].ProductURL, got[1].ProductURL, got[2].ProductURL)
	}
}
