# Roadmap

Planned improvements to photo-migrator, highest priority first.

## Backlog

### 1. Reuse existing albums instead of always creating new ones

**Problem:** the local `migrator.db` is the tool's only record of which albums it has created. If that database is lost or deleted and the migration is run again, every album is created a second time, because Google Photos allows several albums with the same name. The re-uploaded photos are then filed into the new copies.

**Approach:**
- Add the `photoslibrary.readonly.appcreateddata` scope in `internal/auth`. Users must also add it on their OAuth consent screen (update the README setup steps) and run `photo-migrator auth` again.
- Before creating an album, list the albums this app has already created (`albums.list`), and reuse the one whose title matches. Record its ID in the `albums` table as if the tool had just created it.
- Load that list once per run and keep it in memory, rather than making a request for every album.

**Done when:** running a migration with an empty `migrator.db` against an account the tool has already migrated to creates no new albums, and uploads go into the existing albums.

**Limits:** the API only shows albums created by this app. A user-made album with the same name is not reused.

### 2. Skip photos this app already uploaded (best-effort)

**Problem:** with an empty `migrator.db`, every file is uploaded again. The Google Photos API does not document detecting duplicates, so the result can be duplicate photos.

**Approach:**
- Using the same scope as item 1, list the media items this app has uploaded (`mediaItems.list`). Build an index keyed on `filename` + `mediaMetadata.creationTime`.
- Before uploading a file, look it up in that index. On a match, mark it `complete` in the ledger with the existing media item ID, and skip the upload.
- Make this opt-in or easy to turn off (for example a `--reconcile` flag), and log every match so the user can review what was skipped.

**Done when:** after losing `migrator.db`, a new run skips photos already in the account and uploads only the missing ones, and the log lists each match.

**Limits:** this is a best guess, not an exact match.
- The API returns no content hash, so files are matched only by filename and creation time. Common names like `IMG_0001.jpg` can match by accident.
- A photo whose date was edited, in Takeout or by `restore-metadata`, may not match.
- Only items uploaded by this app are visible. Files added through the web interface (such as oversized files uploaded by hand) or from other devices cannot be checked.
- What counts as "created by this app" appears to be tied to the OAuth client / Google Cloud project. Items uploaded with credentials from a different project may not be visible. Confirm this before relying on it.

Backing up `migrator.db` remains the only exact protection against re-uploading.

### 3. Add database cleanup sub-commands to the utility

**Problem:** keeping `migrator.db` healthy has so far meant running `sqlite3` by hand: backing it up, finding out why a zip never completes, deleting rows that will never match anything, and getting a zip processed again. Real cases:

- Changing `staging-dir` mid-migration left stale rows that kept zips from completing.
- The old `--dry-run` bug marked files complete that were never uploaded.
- macOS `._` files were recorded and could never be retried.

Database migrations fixed those known cases automatically, but the next problem will again need SQL. That is error-prone, and easy to get wrong while a run is using the database.

**Approach:** add a `photo-migrator db` command group:

- `db backup [--to <path>]`: take a consistent copy of the database (SQLite `VACUUM INTO`), safe even while a run is in progress.
- `db check`: read-only report, per zip, of complete / pending / error / too-large counts, plus anything inconsistent:
  - rows whose zip entry no longer exists in the zip (read from the zip's file listing, so nothing is extracted);
  - media files in a zip that the ledger has never seen;
  - zips marked complete that still have unfinished rows;
  - rows completed without a real media ID.
- `db fix`: apply the fixes `db check` suggests, such as deleting orphaned rows or resetting false completions.
- `db reset-zip <zip>`: put a zip back to pending so the next run checks it again. Files already uploaded are still skipped.
- `db mark-uploaded <file>`: record that a file `large-files` reports was uploaded by hand through the web interface, so it is no longer listed.

Every command that changes the database should:

- show what it would change and require `--yes` before doing it;
- take an automatic backup first;
- refuse to run while a `run` or `upload` is using the same database.

**Done when:** every manual cleanup needed so far can be done with these commands, with no `sqlite3`. That covers diagnosing a zip stuck in `error`, removing stale or junk rows, re-running a zip, backing up, and clearing a manually uploaded file from the `large-files` report.

**Limits:**

- These commands only change the local ledger. They cannot remove or change anything in Google Photos, because the API this tool uses has no delete operation.
- `db check` can only compare against the zips present in `takeout-dir`. A zip that has been moved or deleted is reported as missing, and its rows are left alone.

### 4. Interactive review of held-back Locked Folder files

**Problem:** files from Google Photos' Locked Folder are held back from upload (status `locked`). The API cannot put them into the destination's Locked Folder, so uploading them would make private photos visible in the main library. Some may be fine to upload, but today the only way to decide is to extract them with `locked-files --extract` and inspect them by hand. There is also no way to tell the tool which ones may be uploaded.

**Approach:** add an interactive review session, for example `photo-migrator locked-files --review`:

- Extract the held-back files from their source zips into a private temporary directory (mode `0700`). This can reuse `takeout.ExtractFiles`, one zip at a time, so only the files under review are on disk.
- For each file, open it in an external viewer and prompt in the terminal: **[a]pprove for upload**, **[k]eep held back**, **[n]ext** (decide later), **[q]uit**. The viewer is set by a `viewer` config key or the `PHOTO_MIGRATOR_VIEWER` environment variable; the default is the platform opener (`xdg-open` / `open`). Videos are reviewed the same way.
- **Approve:** set the file's status back to `pending`, and put its zip back to `pending` so the next `run` checks it again (files already uploaded are still skipped). An approved file is uploaded to the library root with no album. The `run` side of this already works and is covered by `TestProcessZips_ApprovedLockedFileIsUploaded`.
- **Keep:** record that the file was reviewed (for example a `reviewed_at` column), so later sessions show only files not yet reviewed, unless `--all` is passed.
- Delete every extracted copy when the session ends, including on Ctrl+C, and show progress (`12 / 40 reviewed`).
- Like the commands in item 3: refuse to run while a `run` or `upload` is using the same database, and take a backup before the first change.

**Done when:** a user can step through every held-back file in a viewer and approve or keep each one. The next `run` uploads exactly the approved files, to the library root. Kept files stay held back and are not shown again by default. No extracted copies remain on disk after the session.

**Limits:**

- Approving uploads a file into the main library. Making it private again means using "Move to Locked Folder" in the Google Photos mobile app. The session should say so before each approval.
- The tool cannot control what an external viewer or desktop thumbnailer caches. Image viewers and file managers may keep thumbnails (for example in `~/.cache/thumbnails`) after the extracted file has been deleted.
- Motion photos are exported as a still image plus a separate video file. Both parts should be approved together, which depends on the tool handling motion-photo video parts at all; today they are skipped as unrecognised files.

### 5. Resumable uploads for large files

**Problem:** files are uploaded in one request (`X-Goog-Upload-Protocol: raw`). If that request is cut off, the whole file starts again from zero: by a second Ctrl+C, a network drop, or the process ending. Takeout exports often contain videos of 0.5–2 GB, which take 5–10 minutes each on a typical home connection. So:

- Ctrl+C has to wait for them to finish, or lose all the progress made.
- A network error near the end of a large upload repeats the whole upload.

**Approach:** use Google's resumable upload protocol for files above a size threshold (for example 50 MB):

- Start a resumable session (`X-Goog-Upload-Protocol: resumable`, `X-Goog-Upload-Command: start`), then send the file in chunks (`upload` / `upload, finalize` with `X-Goog-Upload-Offset`).
- Record the session URL and the confirmed offset in the ledger (new columns on `media_items`, via a migration). After an interruption, the next run asks Google for the received size (`X-Goog-Upload-Command: query`) and continues from there. If the session has expired, start again.
- With progress kept between runs, the first Ctrl+C can stop large uploads at the end of the current chunk instead of waiting for the whole file. The stop report then says "will resume at 312 / 456 MB".
- Retry a failed chunk instead of the whole file. The rate limit applies per chunk request.

**Done when:**

- Interrupting a run during a large upload (one Ctrl+C) stops within one chunk's time, and the next run finishes that file from where it stopped.
- A simulated network drop mid-file resends only the remaining chunks.
- Files below the threshold keep the current single-request upload.

**Limits:** Google keeps resumable sessions only for a limited time (check the current documentation for how long), so a migration paused for longer restarts those files from zero. Chunked uploads make more requests per large file, which `rate-limit` counts.

### 6. Pixel RAW + JPEG pairs appear as two photos

**Progress:**

- [x] `raw-pairs` setting (`both` | `jpeg` | `raw`) and status `raw_pair`
- [x] `raw-pairs` report (with `--extract`)
- [ ] Pair counts in `scan --skipped`

**Problem:** Pixel phones save a RAW shot as a JPEG (`PXL_x.RAW-01.MP.COVER.jpg`, which is also a motion photo with its video in `PXL_x.RAW-01.MP.COVER`) and a DNG (`PXL_x.RAW-02.ORIGINAL.dng`). On the phone, Google Photos shows the pair as one photo with a RAW badge. Through the Library API there is no way to link them, so each pair arrives as **two separate items**, the JPEG and the RAW, and the shot appears twice in the library. One real Takeout had 786 such pairs.

**Approach:**

- Recognise the pair by name in `takeout.Classify`: the same `PXL_<date>_<time>` stem with `.RAW-01.*.jpg` and `.RAW-02.ORIGINAL.dng`. Like motion-photo halves, this must not depend on both files being in the same zip.
- Add a setting, for example `raw-pairs = "both" | "jpeg" | "raw"`, with `both` (the current behaviour) as the default so nothing changes unless asked:
  - `jpeg` uploads only the JPEG, which keeps the motion and the processed look.
  - `raw` uploads only the DNG, the larger, unprocessed original.
- Hold back the other half with its own status (for example `raw_pair`), as `motion_video` does: it doesn't block the zip, `status` counts it, a `raw-pairs` report lists it (with `--extract`), and setting it back to `pending` releases it.
- Log one summary line per zip ("held back N RAW-pair halves"), with per-file lines at debug level.

**Done when:** with `raw-pairs = "jpeg"`, a run uploads each pair's JPEG and holds back its DNG (and the reverse with `raw`), `scan --skipped` shows how many pairs it found, and the default `both` behaves exactly as today.

**Limits:**

- Files already uploaded are not removed (the API cannot delete). The setting only affects files not yet uploaded, so it is worth deciding before a migration starts.
- Holding back the DNG means the destination keeps only the processed JPEG of those shots, unless the held-back files are uploaded later by hand.

### 7. Uploaded files show the upload date instead of the date tak**Progress** (design: [wrong-dates report](docs/superpowers/specs/2026-11-26-wrong-dates-report-design.md), [sidecar names](docs/superpowers/specs/2026-11-26-sidecar-naming-design.md)):

- [x] `wrong-dates` report, with `--recheck` after fixing dates by hand
- [x] HTML checklist (`--format html`) with a link to each photo
- [x] Sidecar lookup finds edited copies, Live Photo videos, numbered duplicates and cut-short names (left about 20 of 2,168 such photos without a sidecar on one real Takeout)
- [ ] Record the result of `restore-metadata` per file in the ledger (`written`, `no_sidecar`, `no_date`, `failed`)
- [ ] Write the date tags Google Photos reads for formats that need them (videos, PNG)
- [ ] Log one summary line per zip ("no date written for N files")yet.

**Problem:** some photos arrive in the destination account dated the day this tool uploaded them, not the day they were taken, so they appear in the wrong place in the timeline. `restore-metadata` already writes the sidecar's date into each file with exiftool before upload, so these are files where that did not work. Likely causes, not yet confirmed:

- exiftool failed on the file. The worker logs a warning and uploads anyway, and the ledger does not record it.
- No sidecar was found, or it had no date, so nothing was written.
- The format has no date field that Google Photos reads, or exiftool cannot write to it (for example some PNG, GIF or video files).
- The file was uploaded while `restore-metadata` was turned off.

The Library API cannot change a media item's date after upload: `batchCreate` takes no date, and `mediaItems.patch` can change only the description. The date has to be correct in the file when it is uploaded.

**Approach:**

- First find out which files are affected and why, from the run logs and a sample of wrongly dated items, before changing the code.
- Record the result of `restore-metadata` per file in the ledger (a new column, via a migration): `written`, `no_sidecar`, `no_date`, or `failed` with exiftool's error.
- Check the date Google actually assigned. The `batchCreate` response includes the new item's `mediaMetadata.creationTime` (confirm this); compare it with the sidecar's `photoTakenTime` and record a mismatch, allowing for time-zone differences.
- Fix the causes found where the format allows it, for example by writing the date tags Google Photos reads for that format (QuickTime dates for videos, XMP for PNG).
- Log one summary line per zip ("no date written for N files"), with per-file lines at debug level.
- Add a `wrong-dates` report listing uploaded files whose date did not take effect, with the date they should have, so the user can correct them by hand.

**Done when:** after a run, every uploaded file either shows its original date in Google Photos or is listed by `wrong-dates` with the date it should have, and the log says per zip how many files had no date written and why.

**Limits:**

- Files already uploaded with the wrong date cannot be corrected through the API, only by hand in Google Photos ("Edit date & time"). Uploading them again would leave the wrongly dated copy in place, because the API cannot delete.
- Files uploaded before this change have no recorded result. Checking them means listing the app's media items, which needs the read scope from item 1.
- Sidecar times are in UTC and are written with a `+00:00` offset. The date is correct, but Google Photos may show the time in UTC rather than the local time where the photo was taken.

### 8. Edited copies are uploaded next to their originals

**Problem:** when a photo was edited in Google Photos, Takeout exports both the original (`IMG_x.jpg`) and the edited version (`IMG_x-edited.jpg`, also `-EFFECTS-edited`), each as a separate file. The tool uploads both, so the destination shows the photo twice: once as it was taken and once with the edits. One real Takeout had 1,180 edited copies. The Library API has no way to link an edit to its original.

**Approach:**

- Recognise edited copies by name in `takeout.Classify`: a name ending in `-edited` (optionally `(N)`) before the extension, whose original (the same name without `-edited`) exists. Like RAW pairs (item 6), this must not depend on both files being in the same zip.
- Add a setting, for example `edited-copies = "both" | "original" | "edited"`, with `both` (the current behaviour) as the default:
  - `edited` uploads only the edited version, which is what the source library showed.
  - `original` uploads only the untouched original.
- Hold back the other file with its own status (for example `edited_pair`), as `raw_pair` does: it doesn't block the zip, `status` counts it, a report lists it (with `--extract`), and setting it back to `pending` releases it.

**Done when:** with `edited-copies = "edited"`, a run uploads each edited copy and holds back its original (and the reverse with `original`), and the default `both` behaves exactly as today.

**Limits:**

- Files already uploaded are not removed (the API cannot delete). The setting only affects files not yet uploaded.
- Uploading only the edited version loses the unedited original from the destination, unless the held-back files are uploaded later by hand.

### 9. iPhone Live Photo videos are uploaded as separate videos

**Progress** (design: [Live Photo videos](docs/superpowers/specs/2026-11-26-live-photo-videos-design.md)):

- [x] Recognise a Live Photo video half by its still, across zips
- [x] Hold them back during `run` (status `motion_video`) and count them in `scan --skipped`
- [x] `motion-videos --uploaded --format html` cleanup page for the ones already uploaded

**Problem:** Takeout exports an iPhone Live Photo as a still image (`IMG_7297.HEIC`) plus a short video with the same name (`IMG_7297.MP4`); older Pixel motion photos come out the same way (`MVIMG_x.jpg` + `MVIMG_x.MP4`). The tool already holds back Pixel motion-photo video halves (`….MP`, `….MP.COVER`, status `motion_video`), but it does not recognise these: each video is uploaded as a separate 1–3 second video next to its still. One real Takeout had 818 such videos. The Library API cannot rebuild a Live Photo or motion photo.

**Approach:**

- Recognise the video half in `takeout.Classify`: a `.mp4` / `.mov` file with an image of the same stem (`.heic`, `.jpg`, `.jpeg`) in the same folder. Set `MotionPart`, so it is held back with `StatusMotionVideo` and listed by `motion-videos`, exactly like the Pixel halves.
- Because this needs the folder's other names, check it where the walker and `scan` already see the whole folder (or the zip listing), not from the file name alone.
- A video with no matching image is uploaded as today.

**Done when:** a run holds back `IMG_7297.MP4` next to `IMG_7297.HEIC` (status `motion_video`, listed by `motion-videos`) while still uploading a standalone `.MP4`, and `scan --skipped` counts the held-back videos.

**Limits:**

- Videos already uploaded stay in the destination (the API cannot delete). Removing them means deleting them by hand in Google Photos.
- The still image keeps no motion in the destination; the held-back video can be extracted with `motion-videos --extract` if wanted.
- A real video that happens to share its stem with a photo (rare) would be held back too; it can be released by setting its status back to `pending`.
