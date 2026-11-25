// Package pipeline implements the concurrent producer-consumer upload pool.
package pipeline

import (
	"context"
	"path/filepath"
	"sync"

	"go.uber.org/zap"
	"golang.org/x/sync/singleflight"
	"golang.org/x/time/rate"

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
	// RateLimit is the maximum number of Google Photos API requests per second.
	RateLimit float64
	// DryRun logs planned uploads without performing them.
	DryRun bool
	// RestoreMetadata enables writing original date/time and GPS from
	// Takeout sidecar JSON into file metadata before uploading.
	RestoreMetadata bool
	Logger          *zap.Logger
}

// Pool is a concurrent worker pool that uploads MigrationJobs to Google Photos.
type Pool struct {
	cfg        Config
	client     *photos.Client
	store      *store.Store
	limiter    *rate.Limiter
	albumGroup singleflight.Group // deduplicates concurrent album creation
}

// NewPool creates a Pool with the given configuration.
func NewPool(cfg Config, client *photos.Client, st *store.Store) *Pool {
	return &Pool{
		cfg:     cfg,
		client:  client,
		store:   st,
		limiter: rate.NewLimiter(rate.Limit(cfg.RateLimit), 1),
	}
}

// DryRun reports whether the pool only logs planned uploads.
func (p *Pool) DryRun() bool { return p.cfg.DryRun }

// Run starts cfg.Workers goroutines that consume from jobs until the channel
// is closed or ctx is cancelled. It blocks until all workers have exited.
func (p *Pool) Run(ctx context.Context, jobs <-chan Job) {
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
					p.process(ctx, job)
				}
			}
		}()
	}
	wg.Wait()
}

func (p *Pool) process(ctx context.Context, job Job) {
	log := p.cfg.Logger.With(
		zap.String("file", filepath.Base(job.MediaPath)),
		zap.String("album", job.AlbumName),
	)

	// Dry run leaves the item pending so a later real run still uploads it.
	if p.cfg.DryRun {
		log.Info("dry-run: would upload")
		return
	}

	// Resolve (or create) the destination album.
	albumID, err := p.resolveAlbum(ctx, job.AlbumName)
	if err != nil {
		log.Error("resolve album", zap.Error(err))
		_ = p.store.SetMediaError(job.MediaPath, "album: "+err.Error())
		return
	}

	// Rate-limit gate before each API interaction.
	if err := p.limiter.Wait(ctx); err != nil {
		return // context cancelled
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

	// Two-step upload: byte stream → batchCreate.
	mediaID, err := p.client.UploadMedia(ctx, job.MediaPath, description, albumID)
	if err != nil {
		log.Error("upload failed", zap.Error(err))
		_ = p.store.SetMediaError(job.MediaPath, err.Error())
		return
	}

	if err := p.store.SetMediaComplete(job.MediaPath, mediaID); err != nil {
		log.Error("mark complete", zap.Error(err))
		return
	}
	log.Info("✓ uploaded", zap.String("mediaID", mediaID))
}

// resolveAlbum returns the Google Photos album ID for albumName, creating the
// album via the API if it has not been created yet. Returns "" for library root.
//
// singleflight ensures that concurrent workers requesting the same album name
// coalesce into a single CreateAlbum call, preventing duplicate albums.
func (p *Pool) resolveAlbum(ctx context.Context, albumName string) (string, error) {
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

		if err := p.limiter.Wait(ctx); err != nil {
			return "", err
		}

		album, err := p.client.CreateAlbum(ctx, albumName)
		if err != nil {
			return "", err
		}

		if err := p.store.UpsertAlbum(albumName); err != nil {
			return "", err
		}
		if err := p.store.SetAlbumComplete(albumName, album.ID); err != nil {
			return "", err
		}

		p.cfg.Logger.Info("created album", zap.String("name", albumName), zap.String("id", album.ID))
		return album.ID, nil
	})
	if err != nil {
		return "", err
	}
	return v.(string), nil
}
