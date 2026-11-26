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
