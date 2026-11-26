# `wrong-dates` report: design

Part of ROADMAP item 7 ("Uploaded files show the upload date instead of the date taken").

## Why

Some photos arrive in the destination account dated the day photo-migrator uploaded them, not the day they were taken. The Library API cannot change a media item's date after upload, so these can only be fixed by hand in Google Photos ("Edit date & time"). To do that, the user needs a list of which files are affected and the date each should have.

The user's own migration is almost finished: 16,182 files `complete`, 55 `pending`. So the report must cover files **already uploaded**, not just future uploads. That was the user's explicit choice.

## Goals

- `photo-migrator wrong-dates` lists every uploaded file whose date in Google Photos is wrong, showing the date it should have, the date Google gave it, its album, and its zip and path inside the zip.
- It covers files already uploaded, by asking Google for each item's date using the `photos_media_id` already in the ledger.
- Results are stored in the ledger, so rerunning the report is fast and only checks files not checked yet.
- `--recheck` asks Google again for files already checked, so the user can see which ones are still wrong after fixing some by hand.
- Future uploads record both dates as they go, so they need no separate check.

## Non-goals

- Fixing dates. The API cannot do it, and re-uploading would leave the wrongly dated copy in place, because the API cannot delete.
- Finding out *why* each date is wrong (exiftool failure, unsupported format, …). That is the rest of item 7 and can build on the columns added here.
- Comparing GPS location or other metadata.

## Anti-goals

- Changing upload progress. The check must never change `status`, `photos_media_id`, or zip status, and must not make a file be uploaded again.
- Listing files as wrong when the difference is only a time zone. A report full of false alarms would be useless for fixing by hand.
- Extracting media files from the zips. Only the small sidecar JSON files are read.

## Constraints

- **Sign-in permission.** Reading media items needs the `https://www.googleapis.com/auth/photoslibrary.readonly.appcreateddata` scope (confirm that `mediaItems.batchGet` works with it for app-created items). Add it to `auth.Scopes`. Users must add it on the OAuth consent screen and run `photo-migrator auth` again: update the README setup steps and the scope note in CLAUDE.md. Item 1 needs the same scope.
- **Old tokens.** `auth.GetClient` reuses the cached token, which may not have the new scope. When Google answers 403 for missing scopes, the report must stop with a clear message: add the scope to the consent screen and run `photo-migrator auth`. Do not retry it.
- **Sidecars live in the zips, possibly in a different zip than their media file.** Takeout splits folders across zips. In zip mode the extracted files are gone, so expected dates must be read from the sidecar JSON inside the zips in `takeout-dir`.
- **Requests.** `mediaItems.batchGet` takes up to 50 IDs per call: ~310 calls for the user's 15,288 distinct items. It must go through `doWithRetry`, so `rate-limit`, retries and `Retry-After` apply. Pass a context logger (`photos.WithLogger`).
- **Ctrl+C.** Follow the existing interrupt rules: stop between batches, keep what was already stored, exit code 130.
- **Concurrent use.** The report must be safe to run while a `run` is in progress. Today it is not: see Prerequisite 1. Once that is fixed, the DB waits up to 5 s for a lock, and the report's writes are small and only touch the new columns.
- **Schema.** New goose migration `00006_…`. Never edit earlier files.

## Approaches considered

- **A. Work everything out on every run, store nothing.** Least code, but every rerun reads all sidecars from 27 zips and makes ~330 requests to Google, including the reruns the user makes while fixing dates by hand.
- **B. Store both dates in the ledger (chosen).** New columns on `media_items`. `wrong-dates` fills in what is missing, then reports from the DB. `--recheck` refreshes Google's dates. Future uploads record both dates as they go. More code (migration, store methods), but reruns are fast and it gives the rest of item 7 its per-file record.
- **C. Like B, but split into `check-dates` (ask Google, store) and `wrong-dates` (read DB only).** More like the other reports, which only read the DB, but an extra step to remember.

The user picked **B**.

## Prerequisites

### 1. Fix the SQLite connection settings (separate change, first)

`store.Open` passes `?_journal_mode=WAL&_busy_timeout=5000`, but `modernc.org/sqlite` only reads settings given as `_pragma=…` and silently ignores these. The user's live ledger reports `journal_mode = delete` and has no busy timeout, so when two processes write at once, one fails immediately with SQLITE_BUSY. The batcher only logs a failed `SetMediaComplete`, so an uploaded file can stay `pending` and be uploaded again. This already affects `run` and `upload` used together.

- Change the DSN to `?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)`.
- Test that a store opened by `store.Open` reports `journal_mode = wal` and `busy_timeout = 5000`, and that a second `store.Open` on the same file can write while the first holds a write transaction for less than 5 s.
- This switches existing ledgers to WAL permanently: `migrator.db-wal` and `migrator.db-shm` appear next to the DB. Mention this in the README where the DB is described, and in CLAUDE.md. The `db:*` tasks and a plain file copy of `migrator.db` while a run is going are affected (a copy without the `-wal` file may miss recent writes); roadmap item 3's `db backup` uses `VACUUM INTO` for this reason.
- The user chose this over making `wrong-dates` refuse to run alongside `run`.

### 2. Confirm that a hand-fixed date is visible through the API (manual test, first step of the plan)

`--recheck` assumes that "Edit date & time" in Google Photos changes the `mediaMetadata.creationTime` that `mediaItems.batchGet` returns. Before building `--recheck`, check it on one app-uploaded item: fetch its date, change it in Google Photos, fetch again. If the API still returns the old date, drop `--recheck` and the "after fixing, run --recheck" line from the report (my call); the report then lists files, and the user keeps track of what they have fixed.

## Design

### Ledger

Migration `00006` adds to `media_items`:

| column | meaning |
|---|---|
| `taken_time` | the date the file should have: the sidecar's `photoTakenTime`, else `creationTime` (same rule as `metadata.RestoreMetadata`). Unix seconds, UTC. NULL = not looked up yet. |
| `taken_time_source` | `sidecar` or `none` (looked up, no sidecar or no usable date). Lets the report skip files it cannot judge without looking them up again. |
| `google_time` | Google's `mediaMetadata.creationTime` for the item, Unix seconds, UTC. NULL = not fetched yet. |
| `google_checked_at` | when `google_time` was last fetched. NULL = never. Also set when Google says the item no longer exists. |
| `google_missing` | 1 when Google answered NOT_FOUND for the media ID (for example the user deleted it). |

These columns are never read by the upload path to decide anything, so they cannot affect upload progress.

### What counts as wrong

A file is listed when both dates are known and they differ by **more than 24 hours** (my call: a smaller gap is usually a time-zone offset, from a file whose own EXIF date has no offset). The threshold is a constant, not a setting, for now.

**One line per Google photo, not per ledger row** (my call). In the user's ledger, 2,029 complete rows share 1,014 media IDs (15,288 distinct items in all): usually an album copy and the same file in `Photos from YYYY`, which Google stored once. The user fixes photos in Google Photos, so each one is listed once:

- Group rows by `photos_media_id`. Show the row that has an album (the album name helps the user find the photo); if several or none do, the first by zip and path.
- The JSON output includes every row for that item (`copies`: zip, path, album) so nothing is hidden.
- The expected date is taken from the shown row. If the rows for one item have expected dates more than 24 hours apart, list the item anyway and include both dates in the JSON.
- Counts, the `--recheck` set and Google requests are all per media ID: ~310 requests for the user's library.
- `google_time` is fetched once per media ID and written to every row with that ID.

Not listed, but counted in a summary line under the report:

- photos with no sidecar date (`taken_time_source = 'none'`): cannot be judged;
- photos Google would not return (`google_missing = 1`). Labelled "already in your library before this tool, or deleted": an upload that Google deduplicated against a photo already in the account gets that photo's ID, which this app did not create and can't read with the `appcreateddata` scope;
- uploaded photos not checked yet (only if the check was interrupted).

### Finding expected dates (`taken_time`)

- **Zip mode:** build an index of all sidecar JSON across all zips in `takeout-dir`, keyed by folder path inside the zip (for example `Takeout/Google Photos/Trip 2019/`). Read each JSON entry from the zip without extracting. Then, for each row missing `taken_time`, look up its sidecar using its `zip_entry` folder and file name, with the same four strategies as `takeout.FindSidecar`, against the index instead of the filesystem.
- **Dir mode:** call `takeout.FindSidecar(source_path)` as today. The original files are still there.
- Refactor `FindSidecar` so the strategies run over a small "folder listing + open file" abstraction, shared by the filesystem and the zip index, so the two cannot drift apart. How to shape that is the builder's call.
- Only rows with `status = 'complete'` and a real `photos_media_id` (not empty, not `dry-run`) are looked up.
- If `takeout-dir` is missing or a zip is gone, the rows from those zips stay unlooked-up and are counted as "not checked", with a warning naming the zip.

### Fetching Google's dates (`google_time`)

- New client method `photos.Client.BatchGet(ctx, ids []string)`, up to 50 IDs per call, returning each item's ID, `creationTime`, or a not-found result. It goes through `doWithRetry` (and sets `GetBody`, as every request must).
- `wrong-dates` fetches for every complete row with a real media ID and `google_checked_at IS NULL`. With `--recheck`, it also fetches again for the rows currently listed as wrong (my call: that is what a user fixing dates by hand needs, and it costs far fewer requests than rechecking all 16k; a recheck of every row is not needed for now).
- Store results after each batch, so Ctrl+C keeps what was fetched.
- Show progress on stderr (`checked 1,250 / 16,182`), so stdout stays clean for `--format json`.

### Future uploads

- When the worker has the sidecar (`job.Sidecar`), store `taken_time` with the row.
- Parse `mediaItem.mediaMetadata.creationTime` from the `batchCreate` response (`batchCreateResp`) and store it as `google_time` with `google_checked_at = now` when the file is marked complete (confirm the response includes it; if it does not, those rows are simply fetched by the next `wrong-dates`).
- None of this may block or fail an upload: errors here are logged at debug level and ignored.
- Dry run: nothing is written, as for everything else.

### The command

```text
photo-migrator wrong-dates [--recheck] [--format text|json]
```

- Opens the DB, fills in missing `taken_time` (needs `takeout-dir`), fills in missing `google_time` (needs credentials and the new scope), then prints the report.
- Reads `takeout-dir`, `credentials`, `rate-limit` and `db-path` the same way as `run`, through `initConfig` (never bind flags while building the command).
- Uses the existing `newManualFilesCmd` printing where it fits. It needs extra fields (`expected`, `google`), and `--extract` makes no sense here, so extend `manualReport` / `manualFileReport` or write a sibling printer: the builder's call.

Text output sketch:

```text
📅  Wrong dates — 312 file(s) whose date in Google Photos differs from the date taken

  FILE                ALBUM          SHOULD BE            GOOGLE HAS           ZIP / PATH
  IMG_0412.PNG        Trip 2019      2019-06-14 18:02 UTC  2026-07-03 09:41 UTC  takeout-…-004.zip  Takeout/Google Photos/Trip 2019/IMG_0412.PNG
  …

  Fix each in Google Photos: open the photo → ⋮ → Edit date & time.
  Not judged: 41 without a sidecar date, 3 already in your library before this tool, or deleted.
  After fixing some, run: photo-migrator wrong-dates --recheck
```

Dates are shown in UTC (the sidecar's own time zone), with a note saying so.

## Testing

- Prerequisite 1: the DSN test described there.
- Store: migration from version 5 adds the columns and leaves existing rows unchanged (use `goose.UpTo`, as existing store tests do). Queries for "needs lookup", "needs fetch" and "wrong".
- Sidecar index: a test zip where the sidecar sits in a different zip than its media file; truncated names; title fallback. The same cases pass through the filesystem `FindSidecar`.
- Client: `BatchGet` against a fake server, including NOT_FOUND items, more than 50 IDs, and a 403 missing-scope answer.
- Command: full Cobra tree through the `execute` helper with a fake Photos server. Covers listing a wrong file, ignoring a 3-hour difference, two rows sharing a media ID listed once, `--recheck` after the fake server's date changes, Ctrl+C keeping stored batches, and that no `status` or zip status changes.

## Left to the builder

- Shape of the `FindSidecar` refactor and the zip index.
- Whether to extend `manualReport` or write a sibling printer.
- Exact names of store methods and constants.
- Column order and widths in the text output.

## What the first real run changed

Running the report on the user's library changed several decisions above.

**Code 3 means not readable.** `batchGet` answered status code 3 (INVALID_ARGUMENT, "The provided ID does not match any media items") for 72% of the user's media IDs. A `mediaItems.list` call showed only about 4,300 items created by this app, and a sampled photo that Google would not return had its correct date in Google Photos. So these are uploads Google deduplicated against photos already in the account: it handed back the existing items' IDs, which the app did not create and cannot read. Codes 3, 5 and 7 are all counted as not readable, under the label "already in your library before this tool, or deleted". Any other code fails the batch, since it may be transient.

**Failures.** A rejected sign-in stops the command, as it does for uploads. A batch that fails for any other reason is skipped with a warning, and its photos stay unchecked until the next run; after 3 failed batches in a row the run stops, so an outage does not retry every batch. A photo Google returns without a date also stays unchecked rather than being judged.

**Deadlines.** One run hung for 8 hours on a request that never got an answer. Every API call except byte uploads now has a 2-minute deadline per attempt, including reading the response, and a timed-out attempt is retried like a network error. The OAuth HTTP client also drops dead connections (no response headers within 2 minutes, or an HTTP/2 connection that stops answering pings). There is no total timeout, so large, slow uploads still work.

**Which copy is shown.** When rows share a media ID, the report now prefers a copy that has a sidecar date, and among those one in an album; only if no copy has a sidecar date does it fall back to the album copy, then the first. Otherwise an album copy whose sidecar was not found hid a photo whose other copy had its date. "First" is by the path inside the zip, not the staging path. The text output no longer lists every copy; the JSON still does.

**`--recheck`.** The manual check in prerequisite 2 passed: a date fixed by hand in Google Photos is visible through `batchGet`. So `--recheck` was built, and asks Google again only about the photos currently listed as wrong.

**Follow-ups.** About 1,600 of the user's photos have no sidecar date because `FindSidecar` misses some Takeout names: `-edited` copies, `(1)` duplicates, and motion-photo MP4/MP files. When that is fixed, rows recorded with `taken_time_source = 'none'` must be reset so they are looked up again.

## Addition: links and an HTML checklist

Found while trying the report on real data: Google Photos' search box does not find a photo by its exact file name. It offers many near matches that must be checked one by one, so with ~316 photos to fix, searching is not a workable way to reach each one.

The Library API returns a `productUrl` for every media item the app can read: a `https://photos.google.com/lr/…` link that opens that exact photo for the signed-in user. Every photo the report lists is readable by the app, so every listed photo can have a link.

- **Ledger:** a new column `media_items.google_url` (migration `00008`), stored with Google's date by `SetGoogleDates` and by the upload batcher from the `batchCreate` response. NULL = not known.
- **Fetching:** before printing, any listed (wrong) photo without a stored URL is fetched from Google along with the other IDs to fetch (≈7 requests for 316 photos). A photo that still has no URL is listed without a link.
- **`--format html`** writes one self-contained page to stdout (`wrong-dates --format html > wrong-dates.html`): a counter, short instructions, and one row per wrong photo with a checkbox, file name, album, the date to set in the local time zone of the computer that made the page with its offset (for example `29 Jul 2018, 2:17 PM (GMT-04:00)`), the same instant in UTC, Google's current date, and an **Open in Google Photos** link. Ticked boxes are remembered in the browser's localStorage, keyed by media ID, so the list can be worked through over several sittings; a counter shows how many are done. The page loads nothing from the network and escapes every name (Go's `html/template`). No thumbnails: Google's image URLs expire after about an hour.
- **`--format json`** gains a `url` field. The text output is unchanged.
- After fixing photos, `wrong-dates --recheck --format html > wrong-dates.html` regenerates the page without the fixed photos.
