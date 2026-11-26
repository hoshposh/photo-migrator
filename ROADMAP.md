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
