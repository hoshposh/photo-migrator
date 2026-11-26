package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"go.uber.org/zap"

	"github.com/hoshposh/photo-migrator/internal/photos"
	"github.com/hoshposh/photo-migrator/internal/store"
	"github.com/hoshposh/photo-migrator/internal/takeout"
)

type uploadedVideosOpts struct {
	TakeoutDir string // "" = recognise videos from the ledger alone
	Format     string // "text" | "json" | "html"
}

// uploadedVideo is a Live Photo video in the library, one per media ID.
type uploadedVideo struct {
	MediaID, File, Album, SourceZip, InZipPath, SourcePath, URL string
	GoogleTime                                                  time.Time
}

type uploadedVideosReport struct {
	Videos                     []uploadedVideo
	AlreadyRemoved, NotChecked int
}

// videoRows returns the uploaded rows whose file is a Live Photo video half.
// A row's entry is its path inside the zip, or in directory mode its path
// relative to takeoutDir (rows outside it, or any when it is "", have none).
func videoRows(rows []store.DateRow, pairs *takeout.LivePairs, takeoutDir string) []store.DateRow {
	var out []store.DateRow
	for _, r := range rows {
		entry := rowEntry(r)
		if r.SourceZip == "" && takeoutDir != "" {
			rel, err := filepath.Rel(takeoutDir, r.SourcePath)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				continue
			}
			entry = filepath.ToSlash(rel)
		}
		if entry != "" && pairs.IsVideoHalf(entry) {
			out = append(out, r)
		}
	}
	return out
}

// judgeVideos groups rows (all video rows) by media ID. A video Google no
// longer has is counted as removed; one never checked is listed from stored
// state and counted as not checked.
func judgeVideos(rows []store.DateRow) uploadedVideosReport {
	groups := map[string][]store.DateRow{}
	var ids []string
	for _, r := range rows {
		if _, ok := groups[r.MediaID]; !ok {
			ids = append(ids, r.MediaID)
		}
		groups[r.MediaID] = append(groups[r.MediaID], r)
	}
	type listed struct {
		v     uploadedVideo
		shown store.DateRow
	}
	var rep uploadedVideosReport
	var list []listed
	for _, id := range ids {
		g := groups[id]
		sort.SliceStable(g, func(i, j int) bool { return rowLess(g[i], g[j]) })
		shown := g[0]
		removed, checked := false, false
		v := uploadedVideo{
			MediaID: id, File: filepath.Base(shown.SourcePath), Album: shown.AlbumName,
			SourceZip: shown.SourceZip, InZipPath: rowEntry(shown), SourcePath: shown.SourcePath,
			GoogleTime: shown.GoogleTime,
		}
		if v.InZipPath != "" {
			v.File = filepath.Base(v.InZipPath)
		}
		for _, r := range g {
			removed = removed || r.GoogleMissing
			checked = checked || !r.GoogleCheckedAt.IsZero()
			if v.URL == "" {
				v.URL = r.GoogleURL
			}
			if v.GoogleTime.IsZero() {
				v.GoogleTime = r.GoogleTime
			}
		}
		switch {
		case removed:
			rep.AlreadyRemoved++
			continue
		case !checked:
			rep.NotChecked++
		}
		list = append(list, listed{v, shown})
	}
	sort.SliceStable(list, func(i, j int) bool { return rowLess(list[i].shown, list[j].shown) })
	for _, l := range list {
		rep.Videos = append(rep.Videos, l.v)
	}
	return rep
}

// runUploadedVideos lists Live Photo video halves already in the library.
// Google is asked about every candidate on every run, so videos deleted since
// the last run drop off. newClient is called only if there are candidates.
func runUploadedVideos(ctx context.Context, st *store.Store, newClient func() (*photos.Client, error),
	opts uploadedVideosOpts, out, errOut io.Writer, logger *zap.Logger, loc *time.Location, generated time.Time) error {
	if opts.Format != "text" && opts.Format != "json" && opts.Format != "html" {
		return fmt.Errorf("--format must be text, json or html, got %q", opts.Format)
	}
	zipMode := false
	if opts.TakeoutDir != "" {
		// The zips are often deleted while the checklist is still being worked
		// through; the ledger alone is enough then.
		if _, err := os.Stat(opts.TakeoutDir); errors.Is(err, fs.ErrNotExist) {
			logger.Warn("takeout-dir not found; using the database only", zap.String("path", opts.TakeoutDir))
			opts.TakeoutDir = ""
		} else if err != nil {
			return fmt.Errorf("takeout-dir: %w", err)
		}
	}
	if opts.TakeoutDir != "" {
		var err error
		if zipMode, err = takeout.IsZipMode(opts.TakeoutDir); err != nil {
			return err
		}
	}
	pairs, err := buildLivePairs(ctx, opts.TakeoutDir, zipMode, st, logger)
	if err != nil {
		return err
	}
	rows, err := st.GetDateRows()
	if err != nil {
		return fmt.Errorf("read uploaded files: %w", err)
	}
	cands := videoRows(rows, pairs, opts.TakeoutDir)
	seen := map[string]bool{}
	var ids []string
	for _, r := range cands {
		if !seen[r.MediaID] {
			seen[r.MediaID] = true
			ids = append(ids, r.MediaID)
		}
	}
	// Only the candidates are passed as rows, so no other photo is looked up.
	if err := fillGoogleDates(ctx, st, cands, ids, newClient, errOut, logger); err != nil {
		return err
	}
	if rows, err = st.GetDateRows(); err != nil {
		return fmt.Errorf("read uploaded files: %w", err)
	}
	rep := judgeVideos(videoRows(rows, pairs, opts.TakeoutDir))

	switch opts.Format {
	case "json":
		return printUploadedVideosJSON(out, rep)
	case "html":
		if loc == nil {
			loc = time.Local
		}
		if generated.IsZero() {
			generated = time.Now()
		}
		return printUploadedVideosHTML(out, rep, loc, generated)
	}
	printUploadedVideosText(out, rep)
	return nil
}

// countsText is "Already removed: 1. Not checked: 2."; zero parts are left
// out, and "" means both are zero.
func (r uploadedVideosReport) countsText() string {
	var parts []string
	if r.AlreadyRemoved > 0 {
		parts = append(parts, fmt.Sprintf("Already removed: %d.", r.AlreadyRemoved))
	}
	if r.NotChecked > 0 {
		parts = append(parts, fmt.Sprintf("Not checked: %d.", r.NotChecked))
	}
	return strings.Join(parts, " ")
}

func (v uploadedVideo) location() string {
	if v.SourceZip == "" {
		return v.SourcePath
	}
	return filepath.Base(v.SourceZip) + " / " + v.InZipPath
}

func printUploadedVideosText(w io.Writer, rep uploadedVideosReport) {
	_, _ = fmt.Fprintln(w)
	if len(rep.Videos) == 0 {
		_, _ = fmt.Fprintln(w, "  ✓ No uploaded Live Photo videos to remove.")
	} else {
		_, _ = fmt.Fprintf(w, "  🎞  Live Photo videos already uploaded — %d video(s) to remove by hand\n\n", len(rep.Videos))
		tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		for _, v := range rep.Videos {
			album, link := v.Album, v.URL
			if album == "" {
				album = "-"
			}
			if link == "" {
				link = "no link"
			}
			_, _ = fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", v.File, album, v.location(), link)
		}
		_ = tw.Flush()
		_, _ = fmt.Fprintln(w)
	}
	if c := rep.countsText(); c != "" {
		_, _ = fmt.Fprintf(w, "  %s\n", c)
	}
	_, _ = fmt.Fprintln(w)
}

func printUploadedVideosJSON(w io.Writer, rep uploadedVideosReport) error {
	type video struct {
		MediaID   string `json:"media_id"`
		File      string `json:"file"`
		Album     string `json:"album"`
		SourceZip string `json:"source_zip"`
		InZipPath string `json:"in_zip_path"`
		URL       string `json:"url"`
	}
	doc := struct {
		Videos         []video `json:"videos"`
		AlreadyRemoved int     `json:"already_removed"`
		NotChecked     int     `json:"not_checked"`
	}{Videos: []video{}, AlreadyRemoved: rep.AlreadyRemoved, NotChecked: rep.NotChecked}
	for _, v := range rep.Videos {
		doc.Videos = append(doc.Videos, video{v.MediaID, v.File, v.Album, v.SourceZip, v.InZipPath, v.URL})
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}
