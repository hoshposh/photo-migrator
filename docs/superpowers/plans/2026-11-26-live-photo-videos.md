# Live Photo Videos — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Hold back Live Photo / motion video halves (`IMG_x.MP4` next to `IMG_x.HEIC`) during `run` and in `scan --skipped`, and add `motion-videos --uploaded` with an HTML cleanup page for the ones already uploaded.

**Architecture:** A small index in `internal/takeout` (`LivePairs`) records stills by folder and stem from every zip listing (or the dir walk) plus the ledger's registered rows, and answers "is this entry a Live Photo video half?". `processZips`/`processDirectory` and `reportSkipped` consult it after `Classify`. The cleanup page reuses the `wrong-dates` Google-fetch path (`fillGoogleDates`) and a checklist page shell factored out of the `wrong-dates` HTML.

**Tech Stack:** Go, Cobra/Viper, `html/template`, `archive/zip`, SQLite via `internal/store`.

**Spec:** `docs/superpowers/specs/2026-11-26-live-photo-videos-design.md`

## Global Constraints

- A video is a Live Photo half **only** when the same folder holds a still with the same stem; video extensions `.mp4`, `.mov`; still extensions `.heic`, `.jpg`, `.jpeg`; extensions compared case-insensitively, stems exactly. Name patterns alone never decide.
- Held-back halves use the existing `store.StatusMotionVideo` through `RegisterHeld`; the Locked Folder hold takes precedence; an existing `pending` row is not re-held.
- The cleanup page never changes `status` or `photos_media_id`.
- `BatchGet` error handling is unchanged (rejected sign-in / missing scope fatal; other failed batch skipped; stop after 3 in a row; codes 3/5/7 = gone).
- HTML pages: `html/template`, no network loads, links `target="_blank" rel="noopener"`; cleanup ticks under localStorage key `photo-migrator-live-videos`, keyed by media ID.
- Builds stay `CGO_ENABLED=0`; no new dependencies; `task ci` passes after every task. Commits: plain imperative subjects, no `Co-Authored-By` / `Claude-Session` lines.

## Review Focus

1. **A video and its still in different zips, the still's zip processed in an earlier run and since deleted from `takeout-dir`.** Expected: still held back (the still comes from the ledger). Test in Task 2.
2. **A Live Photo video previously released by setting it to `pending`.** Expected: uploaded, not re-held. Test in Task 2.
3. **A standalone video sharing a stem with a non-still (`clip.mov` next to `clip.png`) or with a still in another folder.** Expected: uploaded. Tests in Task 1.
4. **`motion-videos --uploaded` after the user deleted some videos in Google Photos.** Expected: those are counted "already removed" and not listed; the others still are. Test in Task 4.
5. **`motion-videos --uploaded` with no `takeout-dir` set (zips deleted).** Expected: works from the ledger alone. Test in Task 4.

---

### Task 1: The `LivePairs` index

**Files:**
- Create: `internal/takeout/livepairs.go`, `internal/takeout/livepairs_test.go`

**Interfaces:**
- Consumes: `takeout.ListZips` (for callers), `archive/zip`.
- Produces:

```go
// LivePairs records stills (.heic/.jpg/.jpeg) by folder and stem, to tell
// which .mp4/.mov files are Live Photo / motion video halves. Entries are
// paths inside a zip ("Takeout/Google Photos/Trip/IMG_1.HEIC") or, in dir
// mode, slash-separated paths relative to the takeout directory. A nil
// *LivePairs knows no stills.
type LivePairs struct{ /* unexported */ }

func NewLivePairs() *LivePairs
func (p *LivePairs) Add(entry string)              // records entry if it is a still; ignores anything else
func (p *LivePairs) IsVideoHalf(entry string) bool // .mp4/.mov with a recorded still of the same folder and stem; false on nil
func (p *LivePairs) AddZips(ctx context.Context, zips []string) (failed []string, err error) // reads central directories only; unreadable zips are returned in failed, not an error; ctx.Err() on cancel
func (p *LivePairs) AddDir(dir string) error        // walks dir, adding relative slash paths
```

- [ ] **Step 1: Write the failing tests** — `TestLivePairs` (table over `Add` then `IsVideoHalf`): `IMG_7297.MP4` with `IMG_7297.HEIC` in the same folder → true; `MVIMG_1.mp4` + `MVIMG_1.jpg` → true; `IMG_2.MOV` + `IMG_2.JPEG` → true; `IMG_3.mov` with `IMG_3.heic` in a **different** folder → false; `clip.mov` + `clip.png` → false (Review Focus 3); `IMG_4.MP4` alone → false; `IMG_6.jpg` + `IMG_6.MP4.json` → false (JSON isn't a still, and `.json` isn't a video); `img_7.MP4` + `IMG_7.HEIC` → false (stems compared exactly); nil receiver → false. `TestLivePairs_AddZips`: still in zip 1 and video in zip 2 → true; a path that is not a zip is returned in `failed` with `err == nil`; a cancelled ctx returns `context.Canceled`. `TestLivePairs_AddDir`: a temp dir with `Takeout/Google Photos/Trip/IMG_1.HEIC` and `…/IMG_1.MP4` → `IsVideoHalf("Takeout/Google Photos/Trip/IMG_1.MP4")` true.
- [ ] **Step 2: Run them to verify they fail** — `go test -count=1 -run LivePairs ./internal/takeout/` → FAIL to compile.
- [ ] **Step 3: Implement** in `livepairs.go` (a set keyed by `path.Dir(entry) + "/" + stem`; reuse `zip.OpenReader` like `ScanZip`).
- [ ] **Step 4: `go test -count=1 ./internal/takeout/` → PASS; `task ci`**
- [ ] **Step 5: Commit** — `Recognise Live Photo video halves by their still across zips`

---

### Task 2: Hold them back in `run`, and count them in `scan --skipped`

**Files:**
- Modify: `cmd/photo-migrator/main.go` (`runZipMode`, `runDirMode`, `processZips`, `processDirectory`, `reportSkipped`), `internal/store/store.go` (new `ListRegisteredEntries`)
- Test: `cmd/photo-migrator/main_test.go`, `internal/store/store_test.go`
- Modify: `CLAUDE.md` (the "One rule decides what is migrated" and "Media recognised by content; motion-photo halves held back" bullets)

**Interfaces:**
- Consumes: Task 1's `LivePairs`.
- Produces:

```go
// store
type RegisteredEntry struct{ SourcePath, SourceZip, ZipEntry string }
func (s *Store) ListRegisteredEntries() ([]RegisteredEntry, error) // every media_items row, any status

// cmd/photo-migrator — new trailing parameter; nil means no pairing (existing tests pass nil)
func processZips(ctx context.Context, zips []string, stagingDir string, st *store.Store, pool *pipeline.Pool, logger *zap.Logger, pairs *takeout.LivePairs) error
func processDirectory(ctx context.Context, dir, sourceZip string, st *store.Store, pool *pipeline.Pool, logger *zap.Logger, pairs *takeout.LivePairs) error

// cmd/photo-migrator — builds the index from all zips (or the dir) plus the ledger
func buildLivePairs(ctx context.Context, takeoutDir string, zipMode bool, st *store.Store, logger *zap.Logger) (*takeout.LivePairs, error)
```

**Rules:**
- `buildLivePairs`: `AddZips(ListZips(takeoutDir))` (zip mode) or `AddDir(takeoutDir)` (dir mode); warn once per failed zip, naming it, and continue; then `Add` each ledger entry: `ZipEntry` when set, else (dir mode) `SourcePath` made relative to `takeoutDir` with forward slashes; rows whose path can't be made relative are skipped. `st == nil` (scan) skips the ledger.
- `runZipMode` builds the index once before `processZips` (after registering zips, only if there are pending zips); `runDirMode` before `processDirectory`.
- In `processDirectory`'s emit callback, a job with `!j.MotionPart && pairs.IsVideoHalf(entry)` takes the existing `j.MotionPart` branch (dry-run count, `RegisterHeld(store.StatusMotionVideo, …)`, `heldMotion++`, debug line). `entry` is `j.ZipEntry`, or in dir mode `j.MediaPath` relative to `dir` with forward slashes. The Locked Folder branch stays first.
- `reportSkipped`: build the index from the same zips/dir (no ledger) before scanning; in `visit`, a recognised file that `pairs.IsVideoHalf(entry)` counts as `motion` (the `motion_video_parts` summary).

- [ ] **Step 1: Write the failing tests**
  - store: `TestListRegisteredEntries` — rows of several statuses (pending, complete, motion_video, locked) all come back with their `SourceZip`/`ZipEntry`/`SourcePath`.
  - cmd (through `processZips`, fake API as `batchFixture`/`echoBatchCreate` do):
    - `TestProcessZips_HoldsBackLivePhotoVideo` — one zip with `Photos from 2023/IMG_7297.HEIC` and `IMG_7297.MP4` plus a standalone `VID_1.mp4`; pairs built with `buildLivePairs` → the `.MP4` row is `motion_video`, never sent to `/uploads`; `VID_1.mp4` and the `.HEIC` are uploaded.
    - `TestProcessZips_LivePhotoStillInDeletedZip` (Review Focus 1) — ledger already has a `complete` row for zip 1's `…/IMG_8.HEIC`; only zip 2 (with `…/IMG_8.MOV`) is in `takeout-dir` → the `.MOV` is held back.
    - `TestProcessZips_ReleasedLivePhotoVideoUploads` (Review Focus 2) — ledger has the `.MP4` row as `pending` (released) → it is uploaded.
    - `TestProcessZips_LockedLivePhotoVideo` — the pair under `Locked Folder/` → `locked`, not `motion_video`.
    - `TestDryRun_LivePhotoVideoCountedNotWritten` — dry run: no row written for the `.MP4`; the per-zip summary counts it as held back.
  - `TestScanSkipped_CountsLivePhotoVideos` — `scan --skipped --format json` on a takeout with one pair → `motion_video_parts` is 1.
- [ ] **Step 2: Run them to verify they fail** — `go test -count=1 -run 'LivePhoto|ListRegisteredEntries' ./cmd/photo-migrator/ ./internal/store/` → FAIL (compile, then assertions).
- [ ] **Step 3: Implement** (update every existing `processZips`/`processDirectory` call site in tests to pass `nil`).
- [ ] **Step 4: `go test -count=1 ./...` → PASS; `task ci`**
- [ ] **Step 5: Update the two CLAUDE.md bullets and commit** — `Hold back Live Photo video halves during run and in scan`

---

### Task 3: Share the checklist page shell

**Files:**
- Create: `cmd/photo-migrator/checklist_html.go`
- Modify: `cmd/photo-migrator/wrongdates_html.go` (use the shell; output for the wrong-dates page unchanged in substance)
- Test: existing `TestWrongDates_HTML*` / `TestPrintWrongDatesHTML_*` must pass unchanged; add `TestChecklistTemplate_Shell`

**Interfaces:**
- Produces:

```go
// checklistPage is what the shared shell renders around a page's own table.
type checklistPage struct {
	Title, Heading, Intro string // Intro: one paragraph of instructions
	StorageKey            string // localStorage key for the ticks
	Generated             string
	Notes                 []string // lines under the table (counts, hints)
	Rows                  any      // passed to the page's "table" template
}

// newChecklistTemplate returns the shared shell (CSS, counter, tick JS) with
// the page's table definition parsed in as {{define "table"}}…{{end}}. The
// table's checkboxes must be <input type="checkbox" data-id="KEY">.
func newChecklistTemplate(name, table string) *template.Template
```

- [ ] **Step 1: Write the failing test** — `TestChecklistTemplate_Shell`: a template with a one-row table and `StorageKey: "k-test"` renders the title, the intro, the "0 / 1 done" counter markup, the storage key inside the script, and the row; no `http` in the output.
- [ ] **Step 2: Run it to verify it fails** — compile failure.
- [ ] **Step 3: Implement** `checklist_html.go` by moving the CSS and JS from `wrongdates_html.go`; `wrongdates_html.go` keeps only its table definition and fills `checklistPage` (storage key `photo-migrator-wrong-dates`, unchanged, so the user's existing ticks survive).
- [ ] **Step 4: `go test -count=1 ./cmd/photo-migrator/` → PASS; `task ci`**
- [ ] **Step 5: Commit** — `Share one checklist page shell between report pages`

---

### Task 4: `motion-videos --uploaded` and its cleanup page

**Files:**
- Create: `cmd/photo-migrator/livevideos.go`, `cmd/photo-migrator/livevideos_test.go`
- Modify: `cmd/photo-migrator/main.go` (`newMotionVideosCmd`: add `--uploaded`, `--takeout-dir`, `--credentials`, `--rate-limit`; `--format` gains `html` with `--uploaded` only)
- Modify: `README.md` (`motion-videos` section: Live Photo videos, `--uploaded --format html`), `ROADMAP.md` (item 9 **Status:** line)

**Interfaces:**
- Consumes: Task 1/2's `LivePairs`, `buildLivePairs`; `store.GetDateRows`; `fillGoogleDates(ctx, st, rows, also, newClient, errOut, logger)`; `rowEntry`; Task 3's `newChecklistTemplate`.
- Produces:

```go
type uploadedVideosOpts struct{ TakeoutDir, Format string }

// runUploadedVideos lists Live Photo video halves already in the library.
func runUploadedVideos(ctx context.Context, st *store.Store, newClient func() (*photos.Client, error),
	opts uploadedVideosOpts, out, errOut io.Writer, logger *zap.Logger, loc *time.Location, generated time.Time) error
```

**Rules:**
- Index: `buildLivePairs(ctx, opts.TakeoutDir, zipMode, st, logger)`; when `TakeoutDir` is empty, ledger entries only (Review Focus 5). Dir mode is detected with `takeout.IsZipMode` when `TakeoutDir` is set.
- Candidates: `GetDateRows()` rows whose entry (`rowEntry(r)`, or in dir mode `SourcePath` relative to `TakeoutDir`) `IsVideoHalf`. Group by media ID (one listed item per ID; shown row = first by entry).
- Every run: `fillGoogleDates(ctx, st, candidates, <all candidate media IDs>, …)`, then re-read rows. A media ID with `GoogleMissing` → counted "already removed", not listed. A media ID never checked (its batch failed) → listed from stored state and counted "not checked".
- Text: header `🎞  Live Photo videos already uploaded — N video(s) to remove by hand`, one line per video (file, album or `-`, zip / path, link or `no link`), then `Already removed: X. Not checked: Y.` (zero parts omitted). JSON: `{"videos":[{"media_id","file","album","source_zip","in_zip_path","url"}],"already_removed":X,"not_checked":Y}`. HTML: `newChecklistTemplate` with title "Live Photo videos", intro "Open each video and delete it (🗑). The still photo is a separate item and stays.", storage key `photo-migrator-live-videos`, rows keyed by media ID, columns: checkbox, file, album, Google's date (local time, `htmlDateLayout`), **Open in Google Photos**.
- Without `--uploaded`, `motion-videos` is unchanged; `--format html` without `--uploaded` is an error naming the flag.

- [ ] **Step 1: Write the failing tests** (fake batchGet as in `wrongdates_test.go`):
  - `TestUploadedVideos_ListsOnlyUploadedLivePhotoVideos` — complete rows for `IMG_1.MP4` (+ its `IMG_1.HEIC` row), standalone `VID_1.mp4`, and a held `IMG_2.MOV` (`motion_video`) → only `IMG_1.MP4` listed, with its link.
  - `TestUploadedVideos_DeletedVideosDropOff` (Review Focus 4) — the fake answers code 5 for one of two videos → one listed, "Already removed: 1"; a second run with both answered normally lists both again (Google is asked every run).
  - `TestUploadedVideos_NoTakeoutDir` (Review Focus 5) — `TakeoutDir: ""`, stills only in the ledger → the video is listed.
  - `TestUploadedVideos_DoesNotChangeUploadState` — status and media IDs unchanged.
  - `TestUploadedVideos_HTML` — escaped name `<b>&.MP4` pairs with `<b>&.HEIC`, `data-id="<media ID>"`, `photo-migrator-live-videos` in the script, link with `rel="noopener"`, no other `http`.
  - `TestMotionVideosCmd_HTMLNeedsUploaded` (Cobra, `execute`) — `motion-videos --format html` errors mentioning `--uploaded`.
- [ ] **Step 2: Run them to verify they fail** — `go test -count=1 -run 'UploadedVideos|MotionVideosCmd' ./cmd/photo-migrator/` → FAIL.
- [ ] **Step 3: Implement** `livevideos.go` and the flags in `newMotionVideosCmd` (bind flags only through `initConfig`; read paths with `viperPath`).
- [ ] **Step 4: `go test -count=1 ./...` → PASS; `task ci`**
- [ ] **Step 5: README + ROADMAP item 9 Status line ("built: Live Photo video halves held back as `motion_video`; `motion-videos --uploaded --format html` cleanup page"), then commit** — `motion-videos --uploaded: cleanup page for Live Photo videos already uploaded`

---

### Task 5: Check on real data (controller)

- [ ] **Step 1:** With no upload running, `sqlite3 -readonly ~/data/migrator.db ".backup <scratchpad>/live.db"`.
- [ ] **Step 2:** `scan --skipped --format json --takeout-dir ~/data/photos-takeout` → `motion_video_parts` rises by about 818 over today's count.
- [ ] **Step 3:** `motion-videos --uploaded --format html --db-path <scratchpad>/live.db > <scratchpad>/live-videos.html` → about 640 videos listed, each with a link; spot-check two rows open the right video's stem.
