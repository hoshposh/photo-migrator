package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"go.uber.org/zap"

	"github.com/hoshposh/photo-migrator/internal/photos"
	"github.com/hoshposh/photo-migrator/internal/store"
	"github.com/hoshposh/photo-migrator/internal/takeout"
)

// wrongDateThreshold is how far Google's date may differ from the date taken
// before a photo is reported. It absorbs time-zone differences.
const wrongDateThreshold = 24 * time.Hour

const (
	readScope     = "https://www.googleapis.com/auth/photoslibrary.readonly.appcreateddata"
	wrongDateTime = "2006-01-02 15:04 UTC"
)

type wrongDatesOpts struct {
	TakeoutDir string
	Format     string // "text" | "json"
	Recheck    bool   // ask Google again about photos currently listed as wrong
}

// dateReport is the verdict on every uploaded photo, counted per media ID.
type dateReport struct {
	Wrong                                  []wrongDate
	NoSidecarDate, NotReadable, NotChecked int
}

type wrongDate struct {
	MediaID, File, Album, SourceZip, InZipPath string // the shown row
	SourcePath                                 string // the shown row's file, for directory mode
	ShouldBe, GoogleHas                        time.Time
	OtherExpected                              []time.Time // other rows' dates > 24 h from ShouldBe
	Copies                                     []dateCopy  // every row for this media ID
}

type dateCopy struct{ SourceZip, InZipPath, Album string }

// rowEntry is the row's path inside its zip ("" in directory mode).
func rowEntry(r store.DateRow) string {
	switch {
	case r.SourceZip == "":
		return ""
	case r.ZipEntry != "":
		return r.ZipEntry
	}
	return inZipPath(r.SourcePath, r.SourceZip)
}

// rowOrder is the key that orders a media ID's rows and the report: the zip,
// then the path inside it (the file path in directory mode), so the order
// does not depend on the staging directory.
func rowOrder(r store.DateRow) (string, string) {
	if e := rowEntry(r); e != "" {
		return r.SourceZip, e
	}
	return r.SourceZip, r.SourcePath
}

func rowLess(a, b store.DateRow) bool {
	az, ap := rowOrder(a)
	bz, bp := rowOrder(b)
	if az != bz {
		return az < bz
	}
	return ap < bp
}

// shownRow picks the row a photo is reported under: preferably one with a
// sidecar date (so a copy whose sidecar was not found does not hide the
// photo), and among those one in an album. g must be sorted by rowLess.
func shownRow(g []store.DateRow) store.DateRow {
	for _, ok := range []func(store.DateRow) bool{
		func(r store.DateRow) bool { return r.AlbumName != "" && r.TakenSource == store.TakenFromSidecar },
		func(r store.DateRow) bool { return r.TakenSource == store.TakenFromSidecar },
		func(r store.DateRow) bool { return r.AlbumName != "" },
	} {
		for _, r := range g {
			if ok(r) {
				return r
			}
		}
	}
	return g[0]
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// judgeDates groups rows by media ID (Google dedupes identical uploads, so
// several files can share one item) and decides which photos are wrong.
func judgeDates(rows []store.DateRow) dateReport {
	groups := map[string][]store.DateRow{}
	var ids []string
	for _, r := range rows {
		if _, ok := groups[r.MediaID]; !ok {
			ids = append(ids, r.MediaID)
		}
		groups[r.MediaID] = append(groups[r.MediaID], r)
	}

	type sortable struct {
		w     wrongDate
		shown store.DateRow
	}
	var rep dateReport
	var wrong []sortable
	for _, id := range ids {
		g := groups[id]
		sort.SliceStable(g, func(i, j int) bool { return rowLess(g[i], g[j]) })
		shown := shownRow(g)
		missing := false
		for _, r := range g {
			missing = missing || r.GoogleMissing
		}
		switch {
		case missing:
			rep.NotReadable++
			continue
		case shown.GoogleCheckedAt.IsZero() || shown.TakenSource == "":
			rep.NotChecked++
			continue
		case shown.TakenSource == store.TakenNone:
			rep.NoSidecarDate++
			continue
		}

		w := wrongDate{
			MediaID: id, File: filepath.Base(shown.SourcePath), Album: shown.AlbumName,
			SourceZip: shown.SourceZip, InZipPath: rowEntry(shown), SourcePath: shown.SourcePath,
			ShouldBe: shown.TakenTime, GoogleHas: shown.GoogleTime,
		}
		if w.InZipPath != "" {
			w.File = filepath.Base(w.InZipPath)
		}
		for _, r := range g {
			w.Copies = append(w.Copies, dateCopy{r.SourceZip, rowEntry(r), r.AlbumName})
			if r.TakenSource == store.TakenFromSidecar && absDuration(r.TakenTime.Sub(w.ShouldBe)) > wrongDateThreshold {
				w.OtherExpected = append(w.OtherExpected, r.TakenTime)
			}
		}
		if absDuration(w.ShouldBe.Sub(w.GoogleHas)) > wrongDateThreshold || len(w.OtherExpected) > 0 {
			wrong = append(wrong, sortable{w, shown})
		}
	}
	sort.SliceStable(wrong, func(i, j int) bool { return rowLess(wrong[i].shown, wrong[j].shown) })
	for _, s := range wrong {
		rep.Wrong = append(rep.Wrong, s.w)
	}
	return rep
}

// runWrongDates fills in missing dates, then prints the report to out.
// newClient is called only if something must be fetched from Google.
func runWrongDates(ctx context.Context, st *store.Store, newClient func() (*photos.Client, error),
	opts wrongDatesOpts, out, errOut io.Writer, logger *zap.Logger) error {
	if opts.Format != "text" && opts.Format != "json" {
		return fmt.Errorf("--format must be text or json, got %q", opts.Format)
	}
	rows, err := st.GetDateRows()
	if err != nil {
		return fmt.Errorf("read uploaded files: %w", err)
	}
	if err := fillTakenDates(ctx, st, rows, opts.TakeoutDir, logger); err != nil {
		return err
	}
	// rows predates fillTakenDates; fine, as a photo whose date was found just now was never listed.
	var recheck []string
	if opts.Recheck {
		for _, w := range judgeDates(rows).Wrong {
			recheck = append(recheck, w.MediaID)
		}
	}
	if err := fillGoogleDates(ctx, st, rows, recheck, newClient, errOut, logger); err != nil {
		return err
	}
	if rows, err = st.GetDateRows(); err != nil {
		return fmt.Errorf("read uploaded files: %w", err)
	}
	rep := judgeDates(rows)
	if opts.Format == "json" {
		return printWrongDatesJSON(out, rep)
	}
	printWrongDatesText(out, rep)
	return nil
}

// fillTakenDates records the sidecar date of every row that has not been
// looked up. A row stays unset when it cannot be judged fairly: its source is
// gone, or a zip that might hold its sidecar is unavailable.
func fillTakenDates(ctx context.Context, st *store.Store, rows []store.DateRow, takeoutDir string, logger *zap.Logger) error {
	var found []store.TakenDate
	var zipRows []store.DateRow
	for _, r := range rows {
		switch {
		case r.TakenSource != "":
		case r.SourceZip != "":
			zipRows = append(zipRows, r)
		default:
			if err := ctx.Err(); err != nil {
				return err
			}
			if _, err := os.Stat(r.SourcePath); err != nil {
				continue
			}
			if _, sc, err := takeout.FindSidecar(r.SourcePath); err == nil {
				t, _ := sc.TakenTime()
				found = append(found, store.TakenDate{SourcePath: r.SourcePath, Time: t})
			}
		}
	}
	if len(zipRows) > 0 {
		d, err := zipTakenDates(ctx, rows, zipRows, takeoutDir, logger)
		if err != nil {
			return err
		}
		found = append(found, d...)
	}
	if len(found) == 0 {
		return nil
	}
	return st.SetTakenDates(found)
}

// zipTakenDates looks the zip rows up in an index of every zip in takeoutDir.
// "No sidecar" is only recorded when every zip the ledger knows is readable,
// because Takeout may put a photo's sidecar in a different zip.
func zipTakenDates(ctx context.Context, all, zipRows []store.DateRow, takeoutDir string, logger *zap.Logger) ([]store.TakenDate, error) {
	if takeoutDir == "" {
		logger.Warn("no --takeout-dir given: cannot read sidecars from the zips; those photos are left unchecked")
		return nil, nil
	}
	zips, err := takeout.ListZips(takeoutDir)
	if err != nil {
		logger.Warn("cannot read the takeout directory; photos from zips are left unchecked", zap.Error(err))
		return nil, nil
	}
	ix, failed, err := takeout.BuildSidecarIndex(ctx, zips)
	if err != nil {
		return nil, err
	}
	complete := len(failed) == 0
	for _, z := range failed {
		logger.Warn("cannot open zip: photos whose sidecar may be in it are left unchecked", zap.String("zip", filepath.Base(z)))
	}
	have := map[string]bool{}
	for _, z := range zips {
		have[filepath.Base(z)] = true
	}
	warned := map[string]bool{}
	for _, r := range all {
		if b := filepath.Base(r.SourceZip); r.SourceZip != "" && !have[b] {
			complete = false
			if !warned[b] {
				warned[b] = true
				logger.Warn("zip not found in the takeout directory: photos whose sidecar may be in it are left unchecked", zap.String("zip", b))
			}
		}
	}

	var out []store.TakenDate
	for _, r := range zipRows {
		if t, ok := ix.Lookup(rowEntry(r)).TakenTime(); ok {
			out = append(out, store.TakenDate{SourcePath: r.SourcePath, Time: t})
		} else if complete {
			out = append(out, store.TakenDate{SourcePath: r.SourcePath})
		}
	}
	return out, nil
}

// fillGoogleDates asks Google for the date of every media ID not yet checked,
// plus the IDs in recheck, saving each batch as it arrives so an interrupt
// loses nothing.
func fillGoogleDates(ctx context.Context, st *store.Store, rows []store.DateRow, recheck []string,
	newClient func() (*photos.Client, error), errOut io.Writer, logger *zap.Logger) error {
	seen := map[string]bool{}
	var ids []string
	for _, r := range rows {
		if r.GoogleCheckedAt.IsZero() && !seen[r.MediaID] {
			seen[r.MediaID] = true
			ids = append(ids, r.MediaID)
		}
	}
	for _, id := range recheck {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	sort.Strings(ids)
	client, err := newClient()
	if err != nil {
		return err
	}
	ctx = photos.WithLogger(ctx, logger)

	const maxFailedChunks = 3 // consecutive, so an outage does not retry every chunk
	checked, shown, failedRun := 0, -1, 0
	progress := func() {
		shown = checked
		_, _ = fmt.Fprintf(errOut, "checked %s / %s\n", withCommas(checked), withCommas(len(ids)))
	}
	for start := 0; start < len(ids); start += photos.MaxBatchItems {
		end := min(start+photos.MaxBatchItems, len(ids))
		res, err := client.BatchGet(ctx, ids[start:end])
		switch {
		case errors.Is(err, photos.ErrInsufficientScope):
			return fmt.Errorf("cannot read photo dates from Google: the saved sign-in lacks the "+
				"permission %s. Add it on the OAuth consent screen (Scopes), then run: photo-migrator auth: %w", readScope, err)
		case errors.Is(err, photos.ErrAuthFailed):
			return err
		case ctx.Err() != nil:
			return ctx.Err()
		case err != nil:
			logger.Warn("could not read a batch of photo dates; they stay unchecked until the next run",
				zap.Int("photos", end-start), zap.Error(err))
			if failedRun++; failedRun >= maxFailedChunks {
				return fmt.Errorf("stopping after %d failed batches in a row (%d photo dates were saved; run again to continue): %w",
					failedRun, checked, err)
			}
			continue
		}
		failedRun = 0
		var got []store.GoogleDate
		for _, d := range res {
			if !d.Missing && d.CreationTime.IsZero() {
				logger.Warn("Google returned a photo without a date; leaving it unchecked", zap.String("media_id", d.ID))
				continue
			}
			got = append(got, store.GoogleDate{MediaID: d.ID, Time: d.CreationTime, Missing: d.Missing})
		}
		if err := st.SetGoogleDates(got, time.Now()); err != nil {
			return fmt.Errorf("save Google dates: %w", err)
		}
		checked += len(got)
		if end/1000 > start/1000 {
			progress()
		}
	}
	if shown != checked {
		progress()
	}
	return nil
}

// withCommas formats n with thousands separators: 15288 -> "15,288".
func withCommas(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

func printWrongDatesText(w io.Writer, rep dateReport) {
	_, _ = fmt.Fprintln(w)
	if len(rep.Wrong) == 0 {
		_, _ = fmt.Fprintln(w, "  ✓ No uploaded photos have a wrong date.")
	} else {
		_, _ = fmt.Fprintf(w, "  ⚠️  Wrong Dates Report — %d photo(s) uploaded with a date that differs from the date taken\n\n", len(rep.Wrong))
		tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "  FILE\tALBUM\tSHOULD BE\tGOOGLE HAS\tZIP / PATH")
		for _, d := range rep.Wrong {
			album := d.Album
			if album == "" {
				album = "-"
			}
			_, _ = fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n", d.File, album,
				d.ShouldBe.UTC().Format(wrongDateTime), d.GoogleHas.UTC().Format(wrongDateTime), copyLabel(d))
		}
		_ = tw.Flush()
		var notes []string
		for _, d := range rep.Wrong {
			for _, t := range d.OtherExpected {
				notes = append(notes, fmt.Sprintf("  %s: another copy of this photo expects %s.\n", d.File, t.UTC().Format(wrongDateTime)))
			}
		}
		if len(notes) > 0 {
			_, _ = fmt.Fprint(w, "\n"+strings.Join(notes, ""))
		}
		_, _ = fmt.Fprint(w, "\n  Fix each in Google Photos: open the photo → ⋮ → Edit date & time.\n  Dates are shown in UTC.\n  After fixing some, run: photo-migrator wrong-dates --recheck\n")
	}

	var parts []string
	for _, p := range []struct {
		n    int
		what string
	}{
		{rep.NoSidecarDate, "without a sidecar date"},
		{rep.NotReadable, "already in your library before this tool, or deleted"},
		{rep.NotChecked, "not checked yet"},
	} {
		if p.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", p.n, p.what))
		}
	}
	if len(parts) > 0 {
		_, _ = fmt.Fprintf(w, "  Not judged: %s.\n", strings.Join(parts, ", "))
	}
	_, _ = fmt.Fprintln(w)
}

// copyLabel is "zip / path" for a zip row, the file's path in directory mode.
func copyLabel(d wrongDate) string {
	if d.SourceZip == "" {
		return d.SourcePath
	}
	return filepath.Base(d.SourceZip) + " / " + d.InZipPath
}

type wrongDateJSON struct {
	MediaID       string     `json:"media_id"`
	File          string     `json:"file"`
	Album         string     `json:"album"`
	SourceZip     string     `json:"source_zip"`
	InZipPath     string     `json:"in_zip_path"`
	ShouldBe      string     `json:"should_be"`
	GoogleHas     string     `json:"google_has"`
	OtherExpected []string   `json:"other_expected"`
	Copies        []copyJSON `json:"copies"`
}

type copyJSON struct {
	SourceZip string `json:"source_zip"`
	InZipPath string `json:"in_zip_path"`
	Album     string `json:"album"`
}

func printWrongDatesJSON(w io.Writer, rep dateReport) error {
	rfc := func(t time.Time) string { return t.UTC().Format(time.RFC3339) }
	doc := struct {
		Wrong     []wrongDateJSON `json:"wrong"`
		NotJudged struct {
			NoSidecarDate int `json:"no_sidecar_date"`
			NotReadable   int `json:"not_readable"`
			NotChecked    int `json:"not_checked"`
		} `json:"not_judged"`
	}{Wrong: []wrongDateJSON{}}
	doc.NotJudged.NoSidecarDate, doc.NotJudged.NotReadable, doc.NotJudged.NotChecked =
		rep.NoSidecarDate, rep.NotReadable, rep.NotChecked
	for _, d := range rep.Wrong {
		j := wrongDateJSON{
			MediaID: d.MediaID, File: d.File, Album: d.Album, SourceZip: d.SourceZip, InZipPath: d.InZipPath,
			ShouldBe: rfc(d.ShouldBe), GoogleHas: rfc(d.GoogleHas),
			OtherExpected: []string{}, Copies: []copyJSON{},
		}
		for _, t := range d.OtherExpected {
			j.OtherExpected = append(j.OtherExpected, rfc(t))
		}
		for _, c := range d.Copies {
			j.Copies = append(j.Copies, copyJSON(c))
		}
		doc.Wrong = append(doc.Wrong, j)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}

func newWrongDatesCmd() *cobra.Command {
	var format string
	var recheck bool
	cmd := &cobra.Command{
		Use:   "wrong-dates",
		Short: "List uploaded photos whose date in Google Photos is not the date taken",
		Long: `Finds photos that were uploaded but show the wrong date in the destination
account, usually the day they were uploaded instead of the day they were taken.

For every uploaded file the command reads the date taken from the file's
Takeout sidecar JSON (looking in every zip in --takeout-dir, because Takeout
can put a sidecar in a different zip than its photo) and asks Google Photos
which date it assigned. A photo is reported when the two differ by more than
24 hours (smaller differences are time zones), or when copies of the same
photo disagree about the date. Each answer is saved in the database, so a
second run needs neither the zips nor Google.

Reading dates from Google needs the photoslibrary.readonly.appcreateddata
scope. If the saved sign-in lacks it, add the scope on the OAuth consent
screen and run 'photo-migrator auth' again.

The Google Photos API cannot change a photo's date after upload, so the dates
can only be fixed by hand: open the photo in Google Photos, choose
⋮ → Edit date & time. Afterwards, --recheck asks Google again about the
photos currently listed, so the ones you fixed drop off the list.

Photos not judged are counted at the end: those whose sidecar has no date,
those Google does not let this tool read (already in your library before
this tool, or deleted), and those not checked yet (for example a zip missing
from --takeout-dir).

Flags:
  --takeout-dir <dir>  where the Takeout zips are (or set takeout-dir in the config)
  --recheck            ask Google again about the photos listed as wrong
  --format json        Emit machine-readable JSON instead of a text table`,
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			st, err := store.Open(viperPath("db-path"))
			if err != nil {
				return fmt.Errorf("open database: %w", err)
			}
			defer func() {
				if cerr := st.Close(); cerr != nil && err == nil {
					err = cerr
				}
			}()

			ctx, stop := withSignals(cmd.Context(), func() {
				fmt.Fprintln(os.Stderr, "\nStopping: dates already read are saved. Press Ctrl+C again to quit immediately.")
			})
			defer stop()

			return runWrongDates(ctx, st, func() (*photos.Client, error) {
				return buildPhotosClient(viperPath("credentials"), viper.GetFloat64("rate-limit"))
			}, wrongDatesOpts{TakeoutDir: viperPath("takeout-dir"), Format: format, Recheck: recheck},
				os.Stdout, os.Stderr, buildLogger())
		},
	}
	f := cmd.Flags()
	f.String("takeout-dir", "", "directory with the Takeout .zip files (to read sidecars)")
	f.String("credentials", defaultCredentials, "path to OAuth2 credentials.json from Google Cloud Console")
	f.Float64("rate-limit", defaultRateLimit, "max Google Photos API requests per second")
	f.StringVar(&format, "format", "text", "output format: text | json")
	f.BoolVar(&recheck, "recheck", false, "ask Google again about the photos currently listed as wrong")
	return cmd
}
