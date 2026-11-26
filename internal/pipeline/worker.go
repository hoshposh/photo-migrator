// Package pipeline implements the concurrent producer-consumer upload pool.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"go.uber.org/zap"
	"golang.org/x/sync/singleflight"

	"github.com/hoshposh/photo-migrator/internal/metadata"
	"github.com/hoshposh/photo-migrator/internal/photos"
	"github.com/hoshposh/photo-migrator/internal/store"
	"github.com/hoshposh/photo-migrator/internal/takeout"
)

// Job wraps a takeout.MigrationJob for the upload pipeline.
type Job struct {
	takeout.MigrationJob
}

// Config holds worker pool parameters.
type Config struct {
	// Workers is the number of concurrent upload goroutines.
	Workers int
	// DryRun logs planned uploads without performing them.
	DryRun bool
	// RestoreMetadata enables writing original date/time and GPS from
	// Takeout sidecar JSON into file metadata before uploading.
	RestoreMetadata bool
	Logger          *zap.Logger
	// BatchSize is the most files added per batchCreate call (default and
	// maximum 50); BatchDelay is how long a file may wait for its batch to
	// fill (default 2s). See batcher.
	BatchSize  int
	BatchDelay time.Duration
}

// Pool is a concurrent worker pool that uploads MigrationJobs to Google Photos.
type Pool struct {
	cfg        Config
	client     *photos.Client
	store      *store.Store
	albumGroup singleflight.Group // deduplicates concurrent album creation
}

// NewPool creates a Pool with the given configuration.
func NewPool(cfg Config, client *photos.Client, st *store.Store) *Pool {
	return &Pool{
		cfg:    cfg,
		client: client,
		store:  st,
	}
}

// DryRun reports whether the pool only logs planned uploads.
func (p *Pool) DryRun() bool { return p.cfg.DryRun }

// Run starts cfg.Workers goroutines that consume from jobs until the channel
// is closed or ctx is cancelled. It blocks until all workers have exited.
// Cancelling ctx stops new uploads from starting; uploads already in flight
// run to completion.
//
// If Google rejects the sign-in (photos.ErrAuthFailed), every worker stops,
// the affected items stay pending, and Run returns that error.
//
// Workers upload bytes in parallel and hand each file to a batcher, which
// adds files to the library in batches (see batcher). Run returns only once
// every uploaded file has been sent, including after an interrupt.
func (p *Pool) Run(ctx context.Context, jobs <-chan Job) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		fatalOnce sync.Once
		fatal     error
	)
	stop := func(err error) {
		fatalOnce.Do(func() {
			fatal = err
			cancel()
		})
	}

	var b *batcher
	if !p.cfg.DryRun {
		b = p.newBatcher(stop)
		go b.loop()
	}

	var wg sync.WaitGroup
	for i := 0; i < p.cfg.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case job, ok := <-jobs:
					if !ok {
						return
					}
					p.process(ctx, job, stop, b)
				}
			}
		}()
	}
	wg.Wait()
	if b != nil {
		b.close() // send every file already uploaded
	}
	return fatal
}

func (p *Pool) process(ctx context.Context, job Job, stop func(error), b *batcher) {
	fields := []zap.Field{
		zap.String("file", filepath.Base(job.MediaPath)),
		zap.String("album", job.AlbumName),
	}
	if job.SourceZip != "" {
		fields = append(fields, zap.String("zip", filepath.Base(job.SourceZip)))
	}
	log := p.cfg.Logger.With(fields...)

	// Interrupted before this job started: leave it pending for the next run.
	if ctx.Err() != nil {
		return
	}

	// Dry run leaves the item pending so a later real run still uploads it.
	if p.cfg.DryRun {
		log.Info("dry-run: would upload")
		return
	}

	// Resolve (or create) the destination album.
	albumID, err := p.resolveAlbum(ctx, job.AlbumName, log)
	if err != nil {
		if ctx.Err() != nil {
			return // interrupted, not a failure: leave pending
		}
		if errors.Is(err, photos.ErrAuthFailed) {
			stop(err) // not this file's fault: leave pending, stop the run
			return
		}
		log.Error("resolve album", zap.Error(err))
		_ = p.store.SetMediaError(job.MediaPath, "album: "+err.Error())
		return
	}

	// Last chance to stop before the upload starts (the API client applies
	// the rate limit to each request it makes).
	if ctx.Err() != nil {
		return // interrupted: leave pending
	}

	// Build optional description from sidecar.
	description := ""
	if job.Sidecar != nil {
		description = job.Sidecar.Description
	}

	// Restore original date/time and GPS from sidecar into file metadata.
	if p.cfg.RestoreMetadata && job.Sidecar != nil {
		if err := metadata.RestoreMetadata(job.MediaPath, job.Sidecar); err != nil {
			log.Warn("restore metadata (continuing with upload)", zap.Error(err))
		}
	}

	// Step 1 of 2: upload the bytes. From here the upload is in flight, so an
	// interrupt (Ctrl+C) lets it finish rather than aborting it and recording
	// a spurious failure. A second Ctrl+C exits the process.
	token, err := p.client.UploadBytes(photos.WithLogger(context.WithoutCancel(ctx), log), job.MediaPath, job.MimeType)
	if err != nil {
		err = fmt.Errorf("byte upload: %w", err)
		if errors.Is(err, photos.ErrAuthFailed) {
			stop(err) // not this file's fault: leave pending, stop the run
			return
		}
		if errors.Is(err, photos.ErrTooLarge) {
			log.Warn("too large for the Google Photos API; will not retry (see `photo-migrator large-files`)")
			_ = p.store.SetMediaTooLarge(job.MediaPath, err.Error())
			return
		}
		log.Error("upload failed", zap.Error(err))
		_ = p.store.SetMediaError(job.MediaPath, err.Error())
		return
	}

	// Step 2 of 2: the batcher adds the file to the library, together with
	// others for the same album, and records the result.
	fileName := job.UploadName
	if fileName == "" {
		fileName = filepath.Base(job.MediaPath)
	}
	b.add(uploaded{
		job:     job,
		log:     log,
		item:    photos.NewItem{UploadToken: token, FileName: fileName, Description: description},
		albumID: albumID,
		added:   time.Now(),
	})
}

// resolveAlbum returns the Google Photos album ID for albumName, creating the
// album via the API if it has not been created yet. Returns "" for library root.
//
// singleflight ensures that concurrent workers requesting the same album name
// coalesce into a single CreateAlbum call, preventing duplicate albums.
func (p *Pool) resolveAlbum(ctx context.Context, albumName string, log *zap.Logger) (string, error) {
	if albumName == "" {
		return "", nil // library root — no album needed
	}

	// Fast path: album already persisted.
	if id, found, err := p.store.GetAlbumID(albumName); err != nil {
		return "", err
	} else if found {
		return id, nil
	}

	// Slow path: create the album exactly once across all concurrent workers.
	v, err, _ := p.albumGroup.Do(albumName, func() (any, error) {
		// Re-check inside the singleflight in case another goroutine persisted
		// it while we were waiting to enter.
		if id, found, err := p.store.GetAlbumID(albumName); err != nil {
			return "", err
		} else if found {
			return id, nil
		}

		album, err := p.client.CreateAlbum(photos.WithLogger(ctx, log), albumName)
		if err != nil {
			return "", err
		}

		if err := p.store.UpsertAlbum(albumName); err != nil {
			return "", err
		}
		if err := p.store.SetAlbumComplete(albumName, album.ID); err != nil {
			return "", err
		}

		log.Info("created album", zap.String("name", albumName), zap.String("id", album.ID))
		return album.ID, nil
	})
	if err != nil {
		return "", err
	}
	return v.(string), nil
}
