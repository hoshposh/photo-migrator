# Live Photo videos: hold back, and a cleanup page — design

ROADMAP item 9.

## Why

Takeout exports an iPhone Live Photo as a still (`IMG_7297.HEIC`) plus a short video with the same name (`IMG_7297.MP4`); older Pixel motion photos come out the same way (`MVIMG_x.jpg` + `MVIMG_x.MP4`). The tool already holds back Pixel motion-photo video halves (`….MP`, `….MP.COVER`), but not these: each video is uploaded as a separate 1–3 second clip next to its still, cluttering the library. The API cannot rebuild a Live Photo, and cannot delete what is already uploaded.

The user's state (2026-11-26):

- 818 such videos in the Takeout. 639 are already uploaded as separate videos; the tool created all of them, so it can read them and get each one's link. 3 more are registered but not uploaded (2 `pending`, 1 `error`).
- 10 zips (018–027) are not processed yet; they hold about 176 more.
- 113 of the 818 videos sit in a different zip than their still.

The user wants future ones held back **and** a cleanup page to remove the 639 already uploaded by hand, the way the `wrong-dates` checklist works.

## Goals

- During `run`, a Live Photo video half is held back (status `motion_video`), like a Pixel motion half: not uploaded, doesn't block its zip, listed by `motion-videos`, released by setting it back to `pending`. This works when the still is in another zip.
- `scan --skipped` counts the same videos as held back, so a run and the report agree.
- `motion-videos --uploaded --format html` writes a cleanup page: one row per Live Photo video already in the library, with an **Open in Google Photos** link and a checkbox; regenerating it drops the videos the user has deleted.

## Non-goals

- Deleting anything (the API can't).
- Edited copies (ROADMAP item 8).
- Giving the still back its motion.

## Anti-goals

- Holding back a real video: a standalone video must still be uploaded. A video counts as a Live Photo half **only** when a still with the same stem exists in the same folder; name patterns alone (`IMG_1234.MOV` is also a normal iPhone video) never decide.
- Changing upload progress from the cleanup page: it never changes `status` or `photos_media_id`.

## Approaches considered

- **A. Extend `motion-videos` (chosen).** Held-back Live Photo halves get `motion_video`, so `motion-videos` already lists them; a new `--uploaded` mode adds the cleanup page. One command for everything about motion halves.
- **B. A new `live-videos` command.** Clearer alone, but duplicates `motion-videos`.
- **C. Hold back only, plus a one-off list generated now.** Least code, but can't be regenerated or notice deletions.

The user picked **A**.

## Design

### Recognising a Live Photo video half

- **Rule:** a file whose extension is `.mp4` or `.mov` (any case) is a Live Photo video half when the same folder (the path inside the zip, e.g. `Takeout/Google Photos/Photos from 2023/`) holds an image with the same stem and extension `.heic`, `.jpg` or `.jpeg` (any case), in **any** zip of `takeout-dir` (or, in dir mode, on disk under `takeout-dir`).
- **Where it lives:** in `internal/takeout`, next to `Classify`, as a small index built from file listings: a set of `folder/stem` keys for stills. Built from every zip's central directory (no extraction; the same cost as `scan`) or, in dir mode, from a walk of the directory. One helper answers "is this entry a Live Photo video half?" for an entry path (path inside the zip, or path relative to `takeout-dir` in dir mode).
- **Where the stills come from (my call):** the index is the union of (a) every zip listing in `takeout-dir` (or the dir walk) and (b) the ledger's own rows: any registered image row (any status) contributes its `zip_entry` (or dir-relative path). So a zip the user has deleted after processing still counts, because its stills are in the ledger. A zip in `takeout-dir` that can't be opened is logged as a warning naming it and skipped; `run` continues (its own processing would fail on that zip anyway). Cost: a Live Photo video whose still exists only in an unreadable, never-processed zip is uploaded as a clip — rare, and it then shows up on the cleanup page.
- `Classify` stays a per-file function. Its callers that need the answer (the `run` walk and `scan --skipped`) consult the index after `Classify`, through the same helper, so they agree. CLAUDE.md's "one rule decides what is migrated" bullet is updated to name both.

### Holding back during `run`

- At the start of `run` (zip mode: before the first zip; dir mode: before the walk), build the index once from **all** zips in `takeout-dir` (including completed ones), so a video in zip 019 whose still is in zip 004 is recognised.
- A file that is a Live Photo half is treated exactly like today's `MotionPart` files: `RegisterHeld(store.StatusMotionVideo, …)`, counted in the per-zip held-back summary line, never sent to the workers. The Locked Folder hold still takes precedence.
- Dry run: counted as held back, nothing written (as for other held files). The dry-run count can be a little higher than a real run's, because the existing `pending` rows below are uploaded by a real run (my call: not worth a ledger check in dry run).
- **Existing rows (my call):** `RegisterHeld` only holds back *new* rows and treats an existing `pending` row as released, by design. The 3 Live Photo videos already registered as `pending`/`error` (from before this change) are therefore uploaded as today; they then appear on the cleanup page. Re-holding them would also re-hold any video a user deliberately released, which is worse.

### `scan --skipped`

- Uses the same index (built from all zips in the scanned `takeout-dir`) and reports Live Photo halves in the same "held back" group as Pixel motion halves.

### The cleanup page: `motion-videos --uploaded`

- **Which videos:** ledger rows with `status = 'complete'` and a real `photos_media_id` whose entry is a Live Photo half per the index (zip listings from `takeout-dir` when available, plus the ledger's own still rows — so `--uploaded` works even without the zips). Pixel `.MP` halves are never uploaded, so they don't appear. Rows that share a media ID (Google deduplicated them) are listed once per media ID, matching the per-media-ID ticks.
- **Checking Google:** every run asks Google (`BatchGet`, chunks of 50; ~13 requests for 639) whether each video still exists, and for its link. Results go through the existing `SetGoogleDates` path, so the date, `google_url` and `google_missing` are stored (a deleted video then also counts as not readable in `wrong-dates`, which is right). Videos Google answers with codes 3/5/7 (gone) are not listed; they are counted as "already removed". Error handling is the same as `wrong-dates`: a rejected sign-in or missing scope stops the run with its message; another failed batch is skipped (those videos are listed from their last stored state, and counted as "not checked"), stopping after 3 failed batches in a row.
- **Formats:** `--format text` (default), `json`, `html`. Text: file, album, zip/path, link. JSON: the same plus `media_id`, `url`. HTML: a self-contained page like the `wrong-dates` checklist — counter "N / M done", short instructions ("Open each video and delete it with 🗑; the still photo stays"), one row per video with a checkbox, file name, album, the date Google has, and **Open in Google Photos** (`target="_blank" rel="noopener"`), plus the "already removed" and "not checked" counts. Ticks are kept in localStorage under a key distinct from the wrong-dates page (`photo-migrator-live-videos`), keyed by media ID. Built with `html/template`; loads nothing from the network; escapes every name.
- `--uploaded` uses `takeout-dir` if set (for the index; optional thanks to the ledger) and needs credentials (for `BatchGet`, scope `photoslibrary.readonly.appcreateddata`, already required by `wrong-dates`). Without `--uploaded`, `motion-videos` behaves exactly as today (held-back list, `--extract`).
- Reuse, don't copy: the `wrong-dates` HTML page and this one share their CSS/JS skeleton and escaping; the builder should factor the common parts rather than duplicate the template.

### Docs

- README: `motion-videos` section gains Live Photo videos and `--uploaded --format html`; `scan --skipped` wording.
- CLAUDE.md: the "Media recognised by content; motion-photo halves held back" bullet and the "one rule" bullet.
- ROADMAP item 9: Status line.

## Testing

- Index: a Live Photo video with its still in the same zip, with its still in another zip, with no still (uploaded), a `.MOV` next to a `.jpg`, a `.mp4` next to a `.png` (not a pair), case differences, and dir mode.
- `run` (through `processZips` with a fake API, as the existing tests do): the paired video is held back as `motion_video` even when its still is in a zip processed earlier or later; a standalone video is uploaded; a Locked Folder video is held as `locked`; dry run writes nothing.
- `scan --skipped`: reports the paired video as held back.
- `motion-videos --uploaded`: lists only uploaded paired videos; a video Google answers as gone is counted "already removed" and not listed; rerunning after a deletion drops it; HTML escaping and no external URLs; status/media ID unchanged.

## Left to the builder

- The index's type and names, and how dir-mode entry paths are computed.
- How `motion-videos` is split between `newManualFilesCmd` and the new `--uploaded` path.
- How the shared HTML skeleton is factored.
