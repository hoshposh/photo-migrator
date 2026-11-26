package photos

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// fakeAPI serves /uploads and /mediaItems:batchCreate. Hooks let a test
// delay, fail or observe requests.
type fakeAPI struct {
	mu           sync.Mutex
	times        []time.Time // arrival time of every request
	batchActive  atomic.Int32
	batchMaxPar  atomic.Int32
	uploadActive atomic.Int32
	uploadMaxPar atomic.Int32
	onBatch      func(w http.ResponseWriter) bool // return true if it wrote the response
	delay        time.Duration
}

func maxInto(dst *atomic.Int32, v int32) {
	for {
		cur := dst.Load()
		if v <= cur || dst.CompareAndSwap(cur, v) {
			return
		}
	}
}

// client starts the fake API and returns a Client pointed at it.
func (f *fakeAPI) client(t *testing.T, opts ...Option) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.times = append(f.times, time.Now())
		f.mu.Unlock()
		_, _ = io.Copy(io.Discard, r.Body)
		switch {
		case strings.HasSuffix(r.URL.Path, "/uploads"):
			maxInto(&f.uploadMaxPar, f.uploadActive.Add(1))
			time.Sleep(f.delay)
			f.uploadActive.Add(-1)
			_, _ = io.WriteString(w, "tok")
		case strings.HasSuffix(r.URL.Path, "/albums"):
			maxInto(&f.batchMaxPar, f.batchActive.Add(1)) // albums.create is a write too
			time.Sleep(f.delay)
			f.batchActive.Add(-1)
			_, _ = io.WriteString(w, `{"id":"album-1","title":"x"}`)
		case strings.HasSuffix(r.URL.Path, "/mediaItems:batchCreate"):
			maxInto(&f.batchMaxPar, f.batchActive.Add(1))
			time.Sleep(f.delay)
			f.batchActive.Add(-1)
			if f.onBatch != nil && f.onBatch(w) {
				return
			}
			_, _ = io.WriteString(w, `{"newMediaItemResults":[{"uploadToken":"tok","status":{},"mediaItem":{"id":"m"}}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	c := New(srv.Client(), opts...)
	c.baseURL = srv.URL + "/v1"
	return c
}

func mediaFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "a.jpg")
	if err := os.WriteFile(p, []byte{0xFF, 0xD8, 0xFF, 0xE0}, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// Break caught: the rate limit being applied once per file while each file
// makes two API calls (byte upload + batchCreate), so "rate-limit = 4"
// allowed ~8 requests/s and Google answered with HTTP 429.
func TestRateLimitAppliesToEveryRequest(t *testing.T) {
	f := &fakeAPI{}
	c := f.client(t, WithRateLimit(10)) // one request per 100 ms
	for i := 0; i < 2; i++ {
		if _, err := c.UploadMedia(context.Background(), mediaFile(t), "", "", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.times) != 4 {
		t.Fatalf("requests = %d, want 4", len(f.times))
	}
	if span := f.times[3].Sub(f.times[0]); span < 250*time.Millisecond {
		t.Errorf("4 requests took %v at 10 req/s, want >= ~300ms (limit must apply per request)", span)
	}
}

// Break caught: concurrent batchCreate calls for one user, which Google's
// upload guide says must be made one at a time, causing HTTP 429.
func TestBatchCreateIsSerialised(t *testing.T) {
	f := &fakeAPI{delay: 50 * time.Millisecond}
	c := f.client(t)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.UploadMedia(context.Background(), mediaFile(t), "", "", "", ""); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := f.batchMaxPar.Load(); n != 1 {
		t.Errorf("up to %d batchCreate calls ran at once, want 1", n)
	}
	if n := f.uploadMaxPar.Load(); n < 2 {
		t.Errorf("byte uploads never overlapped (max %d at once); they should stay parallel", n)
	}
}

// Break caught: ignoring Google's Retry-After on HTTP 429 and retrying too
// early (the fixed backoff starts at 1s), which burns retries and fails files.
func TestRetryAfterIsHonoured(t *testing.T) {
	var calls atomic.Int32
	f := &fakeAPI{onBatch: func(w http.ResponseWriter) bool {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusTooManyRequests)
			return true
		}
		return false
	}}
	c := f.client(t)
	start := time.Now()
	if _, err := c.UploadMedia(context.Background(), mediaFile(t), "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 1900*time.Millisecond {
		t.Errorf("retried after %v, want >= 2s (Retry-After: 2)", elapsed)
	}
}

// Break caught: retries happening silently, so a run stalls for a minute
// with nothing in the log to say why (e.g. HTTP 429 with Retry-After: 60).
func TestRetryIsLogged(t *testing.T) {
	var calls atomic.Int32
	f := &fakeAPI{onBatch: func(w http.ResponseWriter) bool {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return true
		}
		return false
	}}
	c := f.client(t)
	core, logs := observer.New(zap.DebugLevel)
	ctx := WithLogger(context.Background(), zap.New(core).With(zap.String("file", "a.jpg")))

	if _, err := c.UploadMedia(ctx, mediaFile(t), "", "", "", ""); err != nil {
		t.Fatal(err)
	}

	got := logs.FilterMessageSnippet("retrying after HTTP 429").All()
	if len(got) != 1 {
		t.Fatalf("want 1 retry warning, got %d: %v", len(got), logs.All())
	}
	if got[0].Level != zap.WarnLevel {
		t.Errorf("retry logged at %v, want warn", got[0].Level)
	}
	want := map[string]any{"call": "batchCreate", "status": int64(429), "attempt": int64(1), "retry_after": "1s", "wait": "1s", "file": "a.jpg"}
	m := got[0].ContextMap()
	for k, v := range want {
		if m[k] != v {
			t.Errorf("field %s = %v (%T), want %v", k, m[k], m[k], v)
		}
	}
}

// Break caught: album creation running alongside batchCreate; both are
// write calls that share Google's per-user write limit.
func TestWritesNeverOverlap(t *testing.T) {
	f := &fakeAPI{delay: 50 * time.Millisecond}
	c := f.client(t)
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if _, err := c.CreateAlbum(context.Background(), "Trip"); err != nil {
				t.Error(err)
			}
		}()
		go func() {
			defer wg.Done()
			if _, err := c.UploadMedia(context.Background(), mediaFile(t), "", "", "", ""); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := f.batchMaxPar.Load(); n != 1 {
		t.Errorf("up to %d write calls (createAlbum/batchCreate) ran at once, want 1", n)
	}
}

// Break caught: no way to tell how far a large upload has got (needed to
// show what Ctrl+C is waiting for), or a retry double-counting bytes.
func TestUploadBytesReportsProgress(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable) // first attempt fails after sending everything
			return
		}
		_, _ = io.WriteString(w, "tok")
	}))
	defer srv.Close()
	c := New(srv.Client())
	c.baseURL = srv.URL + "/v1"

	p := filepath.Join(t.TempDir(), "clip.mp4")
	if err := os.WriteFile(p, make([]byte, 300_000), 0o644); err != nil {
		t.Fatal(err)
	}
	var sent atomic.Int64
	if _, err := c.UploadBytes(WithProgress(context.Background(), &sent), p, ""); err != nil {
		t.Fatal(err)
	}
	if got := sent.Load(); got != 300_000 {
		t.Errorf("progress = %d bytes, want 300000 (reset on retry, not doubled)", got)
	}
}
