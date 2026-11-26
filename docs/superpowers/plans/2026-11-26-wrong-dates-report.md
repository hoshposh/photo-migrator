# `wrong-dates` Report Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A `photo-migrator wrong-dates` command that lists every uploaded photo whose date in Google Photos differs from the date it was taken, so the user can fix each one by hand.

**Architecture:** First fix the SQLite connection settings, which the driver currently ignores (separate commit). Then add five date columns to `media_items`. The command fills in the expected date from Takeout sidecars (read from all zips through a cross-zip index) and Google's date (`mediaItems.batchGet`), stores both, and reports from the ledger, one line per Google photo. Future uploads record both dates as they go.

**Tech Stack:** Go, Cobra/Viper, `modernc.org/sqlite`, goose migrations, zap, `net/http/httptest` for fakes.

**Spec:** `docs/superpowers/specs/2026-11-26-wrong-dates-report-design.md`

## Global Constraints

- Builds stay `CGO_ENABLED=0`; add no new module dependencies.
- `task ci` (lint → test → build) passes after every task.
- Never edit existing migration files; the new one is `internal/store/migrations/00006_date_check.sql`.
- The date check never changes `status`, `photos_media_id`, or any zip status, and never causes a file to be uploaded again.
- Wrong = the two dates differ by **more than 24 hours** (`wrongDateThreshold = 24 * time.Hour`, a constant, not a setting).
- New scope: `https://www.googleapis.com/auth/photoslibrary.readonly.appcreateddata`.
- `mediaItems.batchGet`: at most 50 IDs per call (`photos.MaxBatchItems`); every request goes through `doWithRetry`, sets `GetBody` if it has a body, and carries a context logger (`photos.WithLogger`).
- Flags are bound to viper only in `initConfig` (CLAUDE.md); read paths with `viperPath()`.
- Commit messages: plain imperative subject like the existing history (e.g. "Add raw-pairs setting: …"); no `Co-Authored-By` or `Claude-Session` lines (user's global CLAUDE.md).

## Review Focus

1. **A file re-uploaded under a new media ID keeps the old item's Google date.** Expected: `SetMediaComplete` with a different ID clears `google_time`, `google_checked_at`, `google_missing`. Test in Task 2.
2. **A zip is missing from `takeout-dir` (moved, renamed, not yet downloaded).** Its sidecars may belong to files in other zips. Expected: no row is marked "no sidecar date" in that run; unfound rows stay unchecked and the warning names the zip. Test in Task 6.
3. **The cached token lacks the new scope.** Expected: the command stops at the first request with a message telling the user to add the scope and run `photo-migrator auth`, and stores nothing from Google. Test in Task 6.
4. **Ctrl+C halfway through the Google check.** Expected: the batches already fetched stay stored, exit code 130, and the next run fetches only the rest. Test in Task 6.
5. **Running the report a second time with nothing new to check.** Expected: no zip is opened, no credentials are loaded, no request is sent. Test in Task 6.

---

### Task 1: Make the SQLite connection settings take effect

**Files:**
- Modify: `internal/store/store.go:54-68` (`Open`)
- Test: `internal/store/store_test.go`
- Modify: `README.md` (database section near line 412), `CLAUDE.md` (Schema bullet)

**Interfaces:**
- Consumes: nothing.
- Produces: `store.Open` connections use WAL and a 5000 ms busy timeout. Later tasks rely on the report being able to write while a `run` is writing.

- [ ] **Step 1: Write the failing tests**

```go
func TestOpen_UsesWALAndBusyTimeout(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "m.db"))
	// require no error; defer Close
	var mode string
	_ = st.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode)
	var timeout int
	_ = st.db.QueryRow(`PRAGMA busy_timeout`).Scan(&timeout)
	// want mode == "wal", timeout == 5000
}

func TestOpen_SecondConnectionWaitsForLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.db")
	a, _ := Open(path)
	b, _ := Open(path)
	// a: BEGIN IMMEDIATE via a.db.Begin + an UPDATE on zip_archives; hold it.
	// release a's transaction (Commit) after 300 ms in a goroutine.
	// b.RegisterZip("/z.zip") must return nil (it waited), not "database is locked".
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -run 'TestOpen_' ./internal/store/`
Expected: FAIL — journal mode `delete`, timeout `0`, and the second test gets `database is locked (5) (SQLITE_BUSY)`.

- [ ] **Step 3: Change the DSN in `Open`**

```go
sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
```

Keep `SetMaxOpenConns(1)`. Fix the comment above it if it mentions the old parameters.

- [ ] **Step 4: Run the store tests**

Run: `go test -count=1 ./internal/store/`
Expected: PASS, including the existing migration tests.

- [ ] **Step 5: Document it**

README database section: one short paragraph saying the ledger uses SQLite WAL mode, so `migrator.db-wal` and `migrator.db-shm` appear next to it while it is open and belong with it; copy the database with `sqlite3 migrator.db ".backup copy.db"`, not `cp`, while a run is going. CLAUDE.md Schema bullet: add that the DB is opened in WAL mode with a 5 s busy timeout via `_pragma=` DSN parameters (the driver ignores `_journal_mode` / `_busy_timeout`), so a second process (a report) can write while `run` does.

- [ ] **Step 6: Run `task ci` and commit**

```bash
git add internal/store/store.go internal/store/store_test.go README.md CLAUDE.md
git commit -m "Fix SQLite settings: use WAL and a busy timeout as intended"
```

---

### Task 2: Date columns in the ledger

**Files:**
- Create: `internal/store/migrations/00006_date_check.sql`
- Create: `internal/store/dates.go`
- Modify: `internal/store/store.go` (`SetMediaComplete`)
- Test: `internal/store/dates_test.go`

**Interfaces:**
- Consumes: nothing new.
- Produces:

```go
const (
	TakenFromSidecar = "sidecar" // taken_time_source values
	TakenNone        = "none"
)

// DateRow is one uploaded file for the date check. Zero times mean NULL.
type DateRow struct {
	SourcePath, SourceZip, ZipEntry, AlbumName, MediaID string
	TakenTime       time.Time
	TakenSource     string // "", TakenFromSidecar, TakenNone
	GoogleTime      time.Time
	GoogleCheckedAt time.Time
	GoogleMissing   bool
}

// TakenDate is a looked-up expected date; zero Time records TakenNone.
type TakenDate struct {
	SourcePath string
	Time       time.Time
}

// GoogleDate is Google's answer for one media ID.
type GoogleDate struct {
	MediaID string
	Time    time.Time // zero when Missing
	Missing bool
}

func (s *Store) GetDateRows() ([]DateRow, error)
func (s *Store) SetTakenDates(ds []TakenDate) error                       // one transaction
func (s *Store) SetGoogleDates(ds []GoogleDate, checkedAt time.Time) error // one transaction; updates every row with that media ID
```

- [ ] **Step 1: Write the failing tests** in `internal/store/dates_test.go`

- `TestMigration00006_AddsDateColumns`: open a DB at version 5 with `goose.UpTo(db, "migrations", 5)` (same setup as the existing migration tests in `store_test.go`), insert a complete row, then `Open` it. `GetDateRows` returns that row with zero `TakenTime`, `TakenSource == ""`, zero `GoogleTime`, `GoogleMissing == false`; its `status` is still `complete`.
- `TestGetDateRows_OnlyRealUploads`: rows `complete`/`m-1`, `complete`/`dry-run`, `complete`/`""`, `pending`/NULL, `too_large`. Only the `m-1` row is returned. Order is by `source_zip`, then `source_path`.
- `TestSetTakenDates`: `{a, 2019-06-14T18:02:00Z}` and `{b, zero}` → a has `TakenSource == "sidecar"` and that time; b has `TakenSource == "none"` and zero time.
- `TestSetGoogleDates_AllRowsWithThatID`: two rows share `m-1`. `SetGoogleDates([{m-1, t}], now)` sets `GoogleTime == t` and `GoogleCheckedAt == now` (to the second) on both. `SetGoogleDates([{m-1, Missing: true}], now)` sets `GoogleMissing == true` and zero `GoogleTime`.
- `TestSetMediaComplete_NewIDClearsGoogleDate` (Review Focus 1): row complete as `m-1` with a Google date. `SetMediaComplete(path, "m-2")` → `GoogleTime`, `GoogleCheckedAt` zero, `GoogleMissing` false. `SetMediaComplete(path, "m-2")` again after setting a date for `m-2` keeps it.

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -count=1 ./internal/store/`
Expected: FAIL to compile (`GetDateRows` undefined).

- [ ] **Step 3: Write the migration**

Up: `ALTER TABLE media_items ADD COLUMN` × 5: `taken_time INTEGER`, `taken_time_source TEXT`, `google_time INTEGER`, `google_checked_at INTEGER`, `google_missing INTEGER NOT NULL DEFAULT 0` (times are Unix seconds, UTC). Down: `DROP COLUMN` each. Open with a two-line comment saying what the columns are for, like `00002`.

- [ ] **Step 4: Implement `dates.go` and change `SetMediaComplete`**

`GetDateRows` filters `status = 'complete' AND photos_media_id IS NOT NULL AND photos_media_id NOT IN ('', 'dry-run')`. `SetMediaComplete` clears the three `google_*` columns only when `photos_media_id` differs from the new ID (a `CASE WHEN photos_media_id IS ? …` per column).

- [ ] **Step 5: Run the store tests**

Run: `go test -count=1 ./internal/store/`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/store/
git commit -m "Store expected and Google dates for uploaded files"
```

---

### Task 3: Sidecar dates and a cross-zip sidecar index

**Files:**
- Modify: `internal/takeout/sidecar.go` (`FindSidecar` → shared `findSidecar`; new `Sidecar.TakenTime`)
- Create: `internal/takeout/sidecarindex.go`
- Modify: `internal/metadata/restore.go` (use `TakenTime`)
- Test: `internal/takeout/sidecar_test.go` (or the existing sidecar test file), `internal/takeout/sidecarindex_test.go`

**Interfaces:**
- Consumes: nothing new.
- Produces:

```go
// TakenTime is the date the photo was taken: photoTakenTime, else creationTime,
// as UTC. ok is false when s is nil or neither holds a positive Unix time.
func (s *Sidecar) TakenTime() (t time.Time, ok bool)

// findSidecar applies FindSidecar's four strategies to one folder. parse opens
// a JSON file in that folder by name; list returns the folder's file names and
// is only called if strategies 1–2 miss.
func findSidecar(base string, parse func(name string) (*Sidecar, error), list func() []string) (name string, s *Sidecar)

// SidecarIndex holds every per-file sidecar JSON found in a set of zips,
// by folder path inside the zip ("Takeout/Google Photos/Trip 2019").
type SidecarIndex struct{ /* folder -> json file name -> parsed sidecar */ }

// BuildSidecarIndex reads the JSON entries of every zip without extracting
// them. A zip it cannot open is skipped and returned in failed, not an error.
// It stops with ctx.Err() when ctx is cancelled.
func BuildSidecarIndex(ctx context.Context, zipPaths []string) (ix *SidecarIndex, failed []string, err error)

// Lookup finds the sidecar for a file by its path inside a zip, using the same
// strategies as FindSidecar. nil when none matches.
func (ix *SidecarIndex) Lookup(zipEntry string) *Sidecar
```

- [ ] **Step 1: Write the failing tests**

- `TestSidecarTakenTime`: table — photoTakenTime `"1560535320"` → 2019-06-14 18:02:00 UTC, ok; photoTakenTime `"0"` with creationTime `"1560535320"` → that time; both empty → not ok; `"abc"` → not ok; nil `*Sidecar` → not ok.
- `TestSidecarIndex_SidecarInOtherZip`: zip 1 has `Takeout/Google Photos/Trip/a.jpg`; zip 2 has `Takeout/Google Photos/Trip/a.jpg.supplemental-metadata.json` (photoTakenTime 1560535320). `Lookup("Takeout/Google Photos/Trip/a.jpg")` returns a sidecar whose `TakenTime` is that time.
- `TestSidecarIndex_Strategies`: one zip covering a truncated name (`long-name.jpg.supplemental-me.json`), legacy `b.jpg.json`, and the title fallback (`x.json` with `"title": "c.jpg"`), plus `metadata.json` in the same folder that must not be matched. Same folder name in a second folder does not match (lookups are per folder).
- `TestSidecarIndex_UnreadableZipReported`: one valid zip and one path that is not a zip → `failed` contains the bad path, `err == nil`, lookups in the valid zip still work.
- The existing `FindSidecar` tests must still pass unchanged.

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -count=1 ./internal/takeout/`
Expected: FAIL to compile.

- [ ] **Step 3: Implement**

- `TakenTime`: same rule as `RestoreMetadata` today (photoTakenTime unless empty or `"0"`, then creationTime; positive epoch only). Change `RestoreMetadata` to call it; its formatting of `exifDate` stays.
- `findSidecar`: move the four strategies out of `FindSidecar` unchanged in order and in `nonSidecarJSON` handling; `FindSidecar` becomes a wrapper passing `parseSidecar(filepath.Join(dir, name))` and a lazy `os.ReadDir`. Return values of `FindSidecar` stay the same.
- `BuildSidecarIndex`: for each zip, for each entry ending `.json` whose base name is not in `nonSidecarJSON`, parse it (skip entries that fail to parse) and store it under `path.Dir(entry)`. The first zip in `zipPaths` order wins when two zips hold the same JSON path. Check `ctx.Err()` between entries.
- `Lookup`: `findSidecar(path.Base(entry), …)` over the folder `path.Dir(entry)`.

- [ ] **Step 4: Run the takeout and metadata tests**

Run: `go test -count=1 ./internal/takeout/ ./internal/metadata/`
Expected: PASS (metadata tests need `exiftool` installed).

- [ ] **Step 5: Commit**

```bash
git add internal/takeout/ internal/metadata/restore.go
git commit -m "Find sidecars across zips, and a shared rule for a photo's date"
```

---

### Task 4: Read Google's dates (`batchGet`) and the new sign-in scope

**Files:**
- Create: `internal/photos/mediaitems.go`
- Modify: `internal/photos/upload.go` (`batchCreateResp`, `BatchCreateResult`)
- Modify: `internal/auth/oauth.go:28-31` (`Scopes`)
- Test: `internal/photos/mediaitems_test.go`, `internal/photos/client_test.go`
- Modify: `README.md:134-135` (consent-screen scope list, plus a note that existing users must add it and run `photo-migrator auth` again), `CLAUDE.md` (OAuth bullet: three scopes)

**Interfaces:**
- Consumes: `doWithRetry`, `loggerFrom`, `MaxBatchItems`.
- Produces:

```go
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

// BatchGet fetches up to MaxBatchItems media items. Not serialised with writeMu.
func (c *Client) BatchGet(ctx context.Context, ids []string) ([]ItemDate, error)

// BatchCreateResult gains:
//	CreationTime time.Time // mediaItem.mediaMetadata.creationTime; zero if absent
```

- [ ] **Step 1: Write the failing tests** (fake server via `httptest`, as in `client_test.go`)

- `TestBatchGet_ReturnsDatesInRequestOrder`: request `["a","b"]`; the server checks the request is `GET …/mediaItems:batchGet?mediaItemIds=a&mediaItemIds=b` and answers `{"mediaItemResults":[{"mediaItem":{"id":"a","mediaMetadata":{"creationTime":"2019-06-14T18:02:00Z"}}},{"status":{"code":5,"message":"Not found"}}]}`. Result: `[{a, 2019-06-14T18:02:00Z, false}, {b, zero, Missing: true, Status: "Not found"}]`.
- `TestBatchGet_TooManyIDs`: 51 IDs → error, no request sent. Zero IDs → `nil, nil`, no request.
- `TestBatchGet_InsufficientScope`: server answers 403 with body `{"error":{"code":403,"message":"Request had insufficient authentication scopes.","status":"PERMISSION_DENIED"}}` → `errors.Is(err, ErrInsufficientScope)`, exactly one request (not retried).
- `TestBatchGet_RetriesOn429`: first answer 429, then 200 → succeeds after two requests.
- `TestBatchCreate_ReturnsCreationTime`: response item with `mediaMetadata.creationTime` → `BatchCreateResult.CreationTime` set; without it → zero.

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -count=1 ./internal/photos/`
Expected: FAIL to compile.

- [ ] **Step 3: Implement**

Detect missing scope as HTTP 403 whose body contains `insufficient` (case-insensitive) or `ACCESS_TOKEN_SCOPE_INSUFFICIENT`; wrap as `fmt.Errorf("%w: %s", ErrInsufficientScope, body)`. Other non-200 answers: `fmt.Errorf("batchGet HTTP %d: %s", …)`. Parse times with `time.RFC3339Nano`; an unparsable time counts as zero, not an error. Add the scope string from Global Constraints to `auth.Scopes`.

- [ ] **Step 4: Run the photos and auth tests**

Run: `go test -count=1 ./internal/photos/ ./internal/auth/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/photos/ internal/auth/oauth.go README.md CLAUDE.md
git commit -m "Read media items' dates from Google (new read-only scope)"
```

---

### Task 5: Record both dates during uploads

**Files:**
- Modify: `cmd/photo-migrator/main.go` (`processDirectory`, after `RegisterMedia`)
- Modify: `internal/pipeline/batcher.go` (`send`, after `SetMediaComplete` succeeds)
- Test: `cmd/photo-migrator/main_test.go` (extend `echoBatchCreate`; new test)

**Interfaces:**
- Consumes: `Sidecar.TakenTime` (Task 3), `store.SetTakenDates`, `store.SetGoogleDates`, `store.GetDateRows` (Task 2), `BatchCreateResult.CreationTime` (Task 4).
- Produces: nothing new for later tasks.

- [ ] **Step 1: Write the failing test**

Extend `echoBatchCreate` so each created item carries `"mediaMetadata":{"creationTime":"2026-07-03T09:41:00Z"}` (harmless to existing tests).

`TestProcessZips_RecordsDates`: zip with `Takeout/Google Photos/Trip/a.jpg` + `a.jpg.supplemental-metadata.json` (photoTakenTime `1560535320`) and `Takeout/Google Photos/Trip/b.jpg` with no sidecar; run through `batchFixture` (restore-metadata off). `GetDateRows`: a has `TakenSource == "sidecar"`, `TakenTime` 2019-06-14 18:02 UTC, `GoogleTime` 2026-07-03 09:41 UTC, non-zero `GoogleCheckedAt`; b has `TakenSource == ""` (left for `wrong-dates`, whose cross-zip lookup may still find it) and the Google date.

- [ ] **Step 2: Run it to verify it fails**

Run: `go test -count=1 -run TestProcessZips_RecordsDates ./cmd/photo-migrator/`
Expected: FAIL — `TakenSource == ""` for a, zero `GoogleTime`.

- [ ] **Step 3: Implement**

`processDirectory`: when not a dry run and `j.Sidecar.TakenTime()` is ok, call `SetTakenDates` for that path; on error `logger.Debug` and carry on. Never record `TakenNone` here. `batcher.send`: after `SetMediaComplete` succeeds and `r.CreationTime` is non-zero, call `SetGoogleDates` with `time.Now()`; on error `u.log.Debug`. Neither may change what happens to the upload.

- [ ] **Step 4: Run the command tests**

Run: `go test -count=1 ./cmd/photo-migrator/`
Expected: PASS, including `TestDryRun_DoesNotChangeLedger`.

- [ ] **Step 5: Commit**

```bash
git add cmd/photo-migrator/main.go cmd/photo-migrator/main_test.go internal/pipeline/batcher.go
git commit -m "Record the expected and Google dates as files are uploaded"
```

---

### Task 6: The `wrong-dates` command (without `--recheck`)

**Files:**
- Create: `cmd/photo-migrator/wrongdates.go`
- Create: `cmd/photo-migrator/wrongdates_test.go`
- Modify: `cmd/photo-migrator/main.go` (register `newWrongDatesCmd()` in `newRootCmd`)
- Modify: `README.md` (a `wrong-dates` section next to `large-files`), `CLAUDE.md` (one bullet in Architecture), `ROADMAP.md` (item 7: a `**Status:**` line like item 6's — "the `wrong-dates` report is built; finding and fixing the causes is not")

**Interfaces:**
- Consumes: everything from Tasks 2–4; `takeout.ListZips`, `takeout.FindSidecar`, `withSignals`, `buildPhotosClient`, `zipEntryOf`-style fallback (`inZipPath`) for rows with empty `zip_entry`.
- Produces (used by Task 8):

```go
const wrongDateThreshold = 24 * time.Hour

type wrongDatesOpts struct {
	TakeoutDir string
	Format     string // "text" | "json"
	Recheck    bool   // Task 8
}

// runWrongDates fills in missing dates, then prints the report to out.
// newClient is called only if something must be fetched from Google.
func runWrongDates(ctx context.Context, st *store.Store, newClient func() (*photos.Client, error),
	opts wrongDatesOpts, out, errOut io.Writer, logger *zap.Logger) error

// judgeDates groups rows by media ID and decides which photos are wrong.
func judgeDates(rows []store.DateRow) dateReport

type dateReport struct {
	Wrong                                   []wrongDate
	NoSidecarDate, NotReadable, NotChecked int // per media ID
}

type wrongDate struct {
	MediaID, File, Album, SourceZip, InZipPath string // the shown row
	ShouldBe, GoogleHas                        time.Time
	OtherExpected                              []time.Time // other rows' dates > 24 h from ShouldBe
	Copies                                     []dateCopy  // every row for this media ID
}

type dateCopy struct{ SourceZip, InZipPath, Album string }
```

**Rules `judgeDates` must follow** (from the spec):

- Shown row per media ID: the one with a non-empty album; if several or none, the first by (zip, path).
- Per media ID, in this order: `GoogleMissing` on any row → `NotReadable`; Google not checked, or the shown row's `TakenSource == ""` → `NotChecked`; shown row `TakenNone` → `NoSidecarDate`; otherwise wrong when `|ShouldBe − GoogleHas| > 24h` **or** `OtherExpected` is non-empty.
- `Wrong` sorted by zip, then path.

**How `runWrongDates` fills in dates:**

1. `GetDateRows`. Rows with `TakenSource == ""`:
   - zip rows: only if any exist, build the index from `takeout.ListZips(opts.TakeoutDir)`. Found → `TakenDate{path, t}`. Not found → `TakenDate{path, zero}` (none) **only if** no zip failed to open and every `SourceZip` referenced by the rows is in the listing (compare base names); otherwise leave it unset, and warn once per missing or failed zip, naming it. If `TakeoutDir` is empty, warn once and leave all zip rows unset.
   - dir rows (`SourceZip == ""`): if `source_path` no longer exists, leave unset; else `FindSidecar` → found / none.
   - Store with one `SetTakenDates` call.
2. Media IDs with no `GoogleCheckedAt` (distinct, sorted). If none, skip step 3 and never call `newClient`.
3. `newClient()`, then `BatchGet` in chunks of 50 with `photos.WithLogger(ctx, logger)`; after each chunk, `SetGoogleDates` with `time.Now()`. Progress to `errOut`: `checked 1,000 / 15,288` every 1,000 IDs and once at the end. `ErrInsufficientScope` → return an error whose text says to add the scope (give it) on the OAuth consent screen and run `photo-migrator auth`. `ctx` cancelled → return `ctx.Err()` (main maps it to 130).
4. `GetDateRows` again, `judgeDates`, print.

**Output.** Text, using the spec's sketch (header with count; columns FILE, ALBUM, SHOULD BE, GOOGLE HAS, ZIP / PATH; times as `2006-01-02 15:04 UTC`; then):

```text
  Fix each in Google Photos: open the photo → ⋮ → Edit date & time.
  Dates are shown in UTC.
  Not judged: 41 without a sidecar date, 3 deleted or not created by this app, 0 not checked yet.
```

Print the "Not judged" line only when some count is non-zero, and leave out zero parts. With nothing wrong: `  ✓ No uploaded photos have a wrong date.` plus the "Not judged" line. JSON (`--format json`): `{"wrong":[{"media_id","file","album","source_zip","in_zip_path","should_be","google_has","other_expected":[…],"copies":[{"source_zip","in_zip_path","album"}]}],"not_judged":{"no_sidecar_date":N,"not_readable":N,"not_checked":N}}` with RFC 3339 UTC times; progress never goes to stdout.

**Command.** `Use: "wrong-dates"`, a `Long` help in the style of `large-files` that explains the check, the 24-hour rule, the scope requirement, and that dates can only be fixed by hand. Flags: `--takeout-dir`, `--credentials` (default `defaultCredentials`), `--rate-limit` (default `defaultRateLimit`), `--format` (default `text`). `RunE` opens the store, wraps `cmd.Context()` with `withSignals(…, nil)`, and passes `func() (*photos.Client, error) { return buildPhotosClient(viperPath("credentials"), viper.GetFloat64("rate-limit")) }`.

- [ ] **Step 1: Write the failing `judgeDates` tests** (table in `wrongdates_test.go`)

Taken 2019-06-14 18:02 UTC against Google: +3 h → not wrong; +25 h → wrong; −25 h → wrong. `TakenNone` → `NoSidecarDate == 1`. `GoogleMissing` → `NotReadable == 1`. `TakenSource == ""` → `NotChecked == 1`. Two rows sharing `m-1`, one in `Photos from 2019` (album "") and one in album `Trip`, Google +25 h → one `wrongDate`, `Album == "Trip"`, `len(Copies) == 2`. Two rows sharing an ID whose expected dates are 3 days apart, Google equal to the shown one → listed, `OtherExpected` has the other date.

- [ ] **Step 2: Run them to verify they fail, implement `judgeDates`, run them to pass**

Run: `go test -count=1 -run TestJudgeDates ./cmd/photo-migrator/`
Expected: FAIL to compile, then PASS.

- [ ] **Step 3: Write the failing `runWrongDates` tests**

Shared setup: a temp `takeout-dir` with zips written by `writeZipEntries`, a store with rows registered (`RegisterZip`, `RegisterMedia` with `zip_entry`, `SetMediaComplete`), and a fake `batchGet` server (`httptest` + `rewriteTransport`) returning dates from a map, counting requests.

- `TestWrongDates_ListsWrongDate`: a.jpg taken 2019-06-14, Google 2026-07-03 → stdout lists `a.jpg`, `2019-06-14 18:02 UTC`, `2026-07-03 09:41 UTC`; b.jpg Google +3 h → not listed.
- `TestWrongDates_SidecarInOtherZip`: a.jpg in `takeout-001.zip`, its sidecar in `takeout-002.zip` → listed with the right SHOULD BE.
- `TestWrongDates_MissingZipLeavesRowsUnchecked` (Review Focus 2): rows from `takeout-001.zip` and `takeout-002.zip`, but only 001 is in `takeout-dir`; a 001 row whose sidecar is absent → after the run its `TakenSource == ""` (not `none`), errOut/log names `takeout-002.zip`, the "not checked yet" count is shown.
- `TestWrongDates_InsufficientScope` (Review Focus 3): fake answers 403 with the insufficient-scope body → error mentions `photo-migrator auth` and `photoslibrary.readonly.appcreateddata`; no row has `GoogleCheckedAt`.
- `TestWrongDates_InterruptKeepsFetchedBatches` (Review Focus 4): 60 media IDs; the fake cancels the context while answering the second request → `errors.Is(err, context.Canceled)`; exactly 50 IDs have `GoogleCheckedAt`. A second run fetches only the remaining 10.
- `TestWrongDates_RerunMakesNoRequests` (Review Focus 5): after a complete run, a second run with `newClient` that fails the test if called, and a `TakeoutDir` that does not exist, prints the same report.
- `TestWrongDates_DoesNotChangeUploadState`: snapshot `status`, `photos_media_id` of all rows and all zip statuses before and after → equal.
- `TestWrongDates_JSON`: `--format json` output decodes into the shape above; errOut holds the progress line; stdout has no progress text.
- `TestWrongDatesCommand_EmptyLedger` (Cobra level, `execute`): `wrong-dates --db-path <tmp>/m.db` → prints the ✓ line, needs no credentials.

- [ ] **Step 4: Run them to verify they fail**

Run: `go test -count=1 -run 'TestWrongDates' ./cmd/photo-migrator/`
Expected: FAIL to compile (`runWrongDates` undefined).

- [ ] **Step 5: Implement `runWrongDates`, the printers and `newWrongDatesCmd`; register it**

- [ ] **Step 6: Run all tests and lint**

Run: `task ci`
Expected: PASS.

- [ ] **Step 7: Update README, CLAUDE.md and ROADMAP.md as listed under Files, then commit**

```bash
git add cmd/photo-migrator/ README.md CLAUDE.md ROADMAP.md
git commit -m "Add wrong-dates report: uploaded photos whose date differs from the date taken"
```

(`ROADMAP.md` already carries the uncommitted item 7 text written earlier in this session; this commit includes it.)

---

### Task 7: Manual check — does a hand-fixed date show up through the API? (human step)

Spec, Prerequisite 2. Needs the user's Google account; an agent cannot do it alone.

- [ ] **Step 1:** User adds the scope `photoslibrary.readonly.appcreateddata` on the OAuth consent screen and runs `photo-migrator auth`.
- [ ] **Step 2:** With no `run` in progress, make a copy of the real ledger: `sqlite3 ~/data/migrator.db ".backup <scratchpad>/check.db"`.
- [ ] **Step 3:** `photo-migrator wrong-dates --db-path <scratchpad>/check.db --format json`. Pick one listed photo; note its `media_id`.
- [ ] **Step 4:** User changes that photo's date in Google Photos (⋮ → Edit date & time) to its SHOULD BE date.
- [ ] **Step 5:** `sqlite3 <scratchpad>/check.db "UPDATE media_items SET google_checked_at = NULL WHERE photos_media_id = '<id>'"`, then run step 3 again.
- [ ] **Step 6:** Record the result. Photo no longer listed → do Task 8. Still listed with the old Google date → skip Task 8 (the report works without it), and add one line to the README's `wrong-dates` section saying a date fixed by hand is not visible through the API, so the report cannot tell which photos have been fixed.

---

### Task 8: `--recheck` (only if Task 7 showed the fixed date)

**Files:**
- Modify: `cmd/photo-migrator/wrongdates.go`, `cmd/photo-migrator/wrongdates_test.go`, `README.md`

**Interfaces:**
- Consumes: `runWrongDates`, `judgeDates`, `wrongDatesOpts.Recheck` (Task 6).
- Produces: nothing.

- [ ] **Step 1: Write the failing test**

`TestWrongDates_RecheckSeesFixedDate`: first run lists a.jpg (Google 2026-07-03). The fake now returns a.jpg's taken date. A run without `Recheck` still lists it and sends no request. A run with `Recheck: true` sends exactly one request, for a.jpg's ID only (not b.jpg's, which was not wrong), and lists nothing.

- [ ] **Step 2: Run it to verify it fails**

Run: `go test -count=1 -run TestWrongDates_Recheck ./cmd/photo-migrator/`
Expected: FAIL — a.jpg still listed.

- [ ] **Step 3: Implement**

With `Recheck`, before step 2 of the fill-in, run `judgeDates` on the current rows and add the `MediaID`s of `Wrong` to the IDs to fetch. Add the `--recheck` flag, its line in `Long`, and the footer `  After fixing some, run: photo-migrator wrong-dates --recheck` (text output only, only when something is listed).

- [ ] **Step 4: Run `task ci`**

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/photo-migrator/wrongdates.go cmd/photo-migrator/wrongdates_test.go README.md
git commit -m "wrong-dates: --recheck asks Google again for photos listed as wrong"
```

---

### Task 9: Links and an HTML checklist (`--format html`)

Spec: the "Addition: links and an HTML checklist" section of the design doc.

**Files:**
- Create: `internal/store/migrations/00008_google_url.sql`
- Modify: `internal/store/dates.go` (`DateRow.GoogleURL`, `GoogleDate.URL`, `GetDateRows`, `SetGoogleDates`), `internal/store/dates_test.go`
- Modify: `internal/photos/mediaitems.go` (`ItemDate.ProductURL`), `internal/photos/upload.go` (`BatchCreateResult.ProductURL`), their tests
- Modify: `internal/pipeline/batcher.go` (pass the URL to `SetGoogleDates`)
- Modify: `cmd/photo-migrator/wrongdates.go` (URL in `wrongDate`, fetch missing URLs, JSON `url`, `--format html` accepted)
- Create: `cmd/photo-migrator/wrongdates_html.go` (the printer and its embedded template), tests in `cmd/photo-migrator/wrongdates_test.go` or a new `wrongdates_html_test.go`
- Modify: `README.md` (wrong-dates section), `CLAUDE.md` (wrong-dates bullet: one clause)

**Interfaces:**
- Consumes: everything already in `wrongdates.go`, `dates.go`, `mediaitems.go`.
- Produces:

```go
// store
type DateRow struct { /* existing fields */ GoogleURL string } // "" = unknown
type GoogleDate struct { /* existing fields */ URL string }    // stored only when non-empty

// photos
type ItemDate struct { /* existing fields */ ProductURL string }          // mediaItem.productUrl
type BatchCreateResult struct { /* existing fields */ ProductURL string } // mediaItem.productUrl

// cmd/photo-migrator
type wrongDate struct { /* existing fields */ URL string }
func printWrongDatesHTML(w io.Writer, rep dateReport, loc *time.Location, generated time.Time) error
```

**Rules:**
- Migration 00008: `ALTER TABLE media_items ADD COLUMN google_url TEXT;` Down drops it. Never edit 00006 or 00007.
- `SetGoogleDates` writes `google_url` only when `URL != ""` (a Missing answer or an answer without a URL leaves an existing URL alone).
- `wrongDate.URL` comes from the shown row, falling back to any row of that media ID with a URL.
- In `runWrongDates`, the media IDs of currently-wrong photos with no URL on any of their rows are added to the IDs to fetch (same union and dedupe as `--recheck`), so one run fills them. All existing chunk error handling applies unchanged.
- `--format` accepts `text`, `json`, `html`; the error message lists all three.
- HTML page: built with `html/template` from one embedded template string; inline CSS and JS only; no external URLs except the per-photo `productUrl` links (which open in a new tab, `rel="noopener"`). Each row has `<input type="checkbox" data-id="<media ID>">`; JS restores and saves ticks under one localStorage key (`photo-migrator-wrong-dates`), and updates a "N / M done" counter. Date to set: `ShouldBe.In(loc)` formatted `2 Jan 2006, 3:04 PM (GMT-07:00)`; also show UTC (`2006-01-02 15:04 UTC`) and Google's current date. A row without a URL shows "no link" instead of the link. Show the "Not judged" counts and the generated time. Nothing listed → a short "No uploaded photos have a wrong date." page.

- [ ] **Step 1: Write the failing tests**
  - store: `TestSetGoogleDates_URL` — a URL is stored on every row with the media ID; a later `SetGoogleDates` with `URL == ""` (date update, and Missing) keeps it. `TestMigration00008_GoogleURL` — opens a DB at version 7 with a row, migrates, `GetDateRows` returns `GoogleURL == ""`.
  - photos: `TestBatchGet_ProductURL` (field parsed); `TestBatchCreate_ReturnsProductURL`.
  - cmd: `TestWrongDates_FetchesMissingURLs` — a wrong photo whose Google date is stored but has no URL → one run sends one batchGet for exactly that ID, and the JSON output carries its `url`. `TestWrongDates_HTML` — run with `Format: "html"` over two wrong photos (one with a URL, one without) and a file named `<b>&"x".jpg`: output contains the link with `target="_blank"` and `rel="noopener"`, "no link" for the other, the escaped name (`&lt;b&gt;&amp;`), the local date `29 Jul 2018, 2:17 PM (GMT-04:00)` for taken `2018-07-29T18:17:00Z` with `loc = time.FixedZone("", -4*3600)`, `data-id="<media ID>"`, and no `http` URL other than the `productUrl` links (no `<script src`, no `<link href`). `TestWrongDates_FormatRejected` — `--format xml` error lists text, json and html.
- [ ] **Step 2: Run them to verify they fail**
- [ ] **Step 3: Implement** (store, photos, batcher, wrongdates.go, wrongdates_html.go; `newWrongDatesCmd` passes `time.Local` and `time.Now()` to the HTML printer)
- [ ] **Step 4: `task ci`**, then README (a `--format html` example and one paragraph on the checklist page and localStorage) and the CLAUDE.md clause
- [ ] **Step 5: Commit** — `wrong-dates: links to each photo and an HTML checklist`
