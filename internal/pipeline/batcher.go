package pipeline

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"go.uber.org/zap"

	"github.com/hoshposh/photo-migrator/internal/photos"
	"github.com/hoshposh/photo-migrator/internal/store"
)

// Defaults for Config.BatchSize and Config.BatchDelay.
const (
	defaultBatchSize  = photos.MaxBatchItems
	defaultBatchDelay = 2 * time.Second
	// batchQueue bounds how many uploaded-but-not-yet-created files can wait
	// for the batcher. When Google makes the batcher wait (HTTP 429), byte
	// uploads keep going until this fills, then workers wait too.
	batchQueue = 200
)

// uploaded is one file whose bytes are uploaded, waiting for batchCreate.
type uploaded struct {
	job     Job
	log     *zap.Logger // the file's logger (file, album, zip)
	item    photos.NewItem
	albumID string // "" = library root
	added   time.Time
}

// batcher adds uploaded files to the library with as few batchCreate calls
// as possible. batchCreate calls are write calls that Google rate-limits per
// user; one call can create up to 50 items, but only in a single album, so
// files are grouped by album. A group is sent when it is full, or once its
// oldest file has waited BatchDelay. It runs in a single goroutine, so its
// writes are sequential.
type batcher struct {
	p     *Pool
	stop  func(error) // stops the whole run (rejected sign-in)
	in    chan uploaded
	done  chan struct{}
	size  int
	delay time.Duration

	authFailed bool // owned by the loop goroutine
}

func (p *Pool) newBatcher(stop func(error)) *batcher {
	size, delay := p.cfg.BatchSize, p.cfg.BatchDelay
	if size <= 0 || size > photos.MaxBatchItems {
		size = defaultBatchSize
	}
	if delay <= 0 {
		delay = defaultBatchDelay
	}
	return &batcher{p: p, stop: stop, in: make(chan uploaded, batchQueue), done: make(chan struct{}), size: size, delay: delay}
}

// add queues an uploaded file. It blocks while the queue is full.
func (b *batcher) add(u uploaded) { b.in <- u }

// close sends everything still queued, then returns once the loop exits.
// Call it after every worker has stopped adding files.
func (b *batcher) close() {
	close(b.in)
	<-b.done
}

func (b *batcher) loop() {
	defer close(b.done)
	groups := map[string][]uploaded{} // album ID -> files, oldest first
	tick := time.NewTicker(b.delay / 4)
	defer tick.Stop()

	flush := func(due func([]uploaded) bool) {
		for album, g := range groups {
			if due(g) {
				b.send(album, g)
				delete(groups, album)
			}
		}
	}
	for {
		select {
		case u, ok := <-b.in:
			if !ok {
				flush(func([]uploaded) bool { return true }) // end of run or Ctrl+C
				return
			}
			groups[u.albumID] = append(groups[u.albumID], u)
			if g := groups[u.albumID]; len(g) >= b.size {
				b.send(u.albumID, g)
				delete(groups, u.albumID)
			}
		case <-tick.C:
			flush(func(g []uploaded) bool { return time.Since(g[0].added) >= b.delay })
		}
	}
}

// send creates one group of files with a single batchCreate call and
// records each file's own result.
func (b *batcher) send(albumID string, group []uploaded) {
	if b.authFailed {
		return // sign-in rejected: leave the files pending for the next run
	}
	log := b.p.cfg.Logger.With(zap.Int("batch", len(group)))
	if z := group[0].job.SourceZip; z != "" {
		log = log.With(zap.String("zip", filepath.Base(z)))
	}
	items := make([]photos.NewItem, len(group))
	for i, u := range group {
		items[i] = u.item
	}

	// Not cancellable: these bytes are already uploaded, so finish adding
	// them even after Ctrl+C (a second Ctrl+C exits the process).
	results, err := b.p.client.BatchCreate(photos.WithLogger(context.Background(), log), albumID, items)
	if err != nil {
		if errors.Is(err, photos.ErrAuthFailed) {
			b.authFailed = true
			b.stop(err) // not these files' fault: leave them pending
			return
		}
		for _, u := range group {
			u.log.Error("upload failed", zap.Error(fmt.Errorf("batchCreate: %w", err)))
			_ = b.p.store.SetMediaError(u.job.MediaPath, "batchCreate: "+err.Error())
		}
		return
	}

	byToken := make(map[string]photos.BatchCreateResult, len(results))
	for _, r := range results {
		byToken[r.UploadToken] = r
	}
	for _, u := range group {
		r, ok := byToken[u.item.UploadToken]
		switch {
		case !ok:
			msg := "batchCreate returned no result for this file"
			u.log.Error("upload failed", zap.String("error", msg))
			_ = b.p.store.SetMediaError(u.job.MediaPath, msg)
		case r.StatusCode != 0:
			msg := "batchCreate item error: " + r.StatusMsg
			u.log.Error("upload failed", zap.String("error", msg))
			_ = b.p.store.SetMediaError(u.job.MediaPath, msg)
		default:
			if err := b.p.store.SetMediaComplete(u.job.MediaPath, r.MediaItemID); err != nil {
				u.log.Error("mark complete", zap.Error(err))
				continue
			}
			if !r.CreationTime.IsZero() {
				// Best effort: the date never changes what happens to the upload.
				g := []store.GoogleDate{{MediaID: r.MediaItemID, Time: r.CreationTime}}
				if err := b.p.store.SetGoogleDates(g, time.Now()); err != nil {
					u.log.Debug("record Google date", zap.Error(err))
				}
			}
			u.log.Info("✓ uploaded", zap.String("mediaID", r.MediaItemID))
		}
	}
}
