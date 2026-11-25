# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

A Go CLI (`photo-migrator`) that moves a Google Photos library from one account to another. It reads a Google Takeout export of the source account (a directory of `.zip` files or an already-extracted directory) and uploads it to the destination account through the Google Photos Library API. All progress is kept in a local SQLite file (`migrator.db`), so runs can be resumed and repeated safely. The README covers end-user setup (Google Cloud project, OAuth consent screen, Takeout).

## Commands

Tools (`go`, `task`, `golangci-lint`, `sqlite3`) are installed with `mise install` (see `.mise.toml`). The Go version in `.mise.toml` must match the `go` line in `go.mod`, so change both together. Builds use `CGO_ENABLED=0`, because the SQLite driver is `modernc.org/sqlite` (pure Go).

```bash
task build                 # go build -> ./photo-migrator (version from git describe, "dev" when not in a git repo)
task test                  # go test -v -count=1 ./...
task lint                  # golangci-lint run ./...
task vet                   # go vet ./...
task fmt                   # gofmt -w .
task ci                    # lint -> test -> build

go test -run TestRestoreMetadata_GPS ./internal/metadata/   # a single test

task db:status | db:zips | db:errors | db:shell    # inspect migrator.db (DB=path to override)
```

- Tests live in `internal/metadata`, `internal/auth` and `cmd/photo-migrator`. The metadata tests run the real `exiftool` binary and fail if it isn't installed. The command tests run the full Cobra tree with a temporary `HOME` and working directory; reuse their `execute` helper, which resets the global viper state. The auth tests run the real sign-in flow against a fake token server, with the package variable `openBrowser` replaced by a fake browser.
- To try the CLI safely, give it its own `--db-path` and `HOME` so it doesn't touch the real ledger, config or OAuth token.

## Architecture

The flow is: `cmd/photo-migrator/main.go` (Cobra commands plus all the orchestration logic) → `takeout` (find, extract and walk files) → `pipeline` (worker pool) → `photos` (REST client) and `store` (SQLite ledger).

- **Zip mode and dir mode.** `takeout.IsZipMode` checks whether `--takeout-dir` contains any `.zip` file. In zip mode, `processZips` handles one zip at a time, in alphabetical order:
  1. Extract into `<staging-dir>/<zip-stem>/`.
  2. Walk the extracted files and upload them.
  3. Delete the extracted files (`os.RemoveAll`) whether or not the uploads succeeded.
  4. Mark the zip `complete` only if `AllZipItemsComplete`. Otherwise it is marked `error`, and `GetPendingZips` (`status != 'complete'`) picks it up again on the next run.

  Dir mode walks the original directory in place.
- **The ledger key is the absolute file path on disk.** `media_items.source_path` is the path the file was extracted to (in zip mode) or the original path (in dir mode). Skipping already-uploaded files (`store.IsUploaded`) depends on that path being the same on every run. If `staging-dir` changes partway through a migration, every file looks new and gets uploaded again, which creates duplicates. The `large-files` command also relies on this layout: `inZipPath` recovers a file's path inside its zip by cutting the path at `/<zip-stem>/`.
- **Producer and consumers.** `processDirectory` runs `takeout.WalkDirectory` in a goroutine. For each file it registers the row in the DB, then sends a job on a channel. `pipeline.Pool` runs `workers` goroutines that share one `rate.Limiter`. Album creation goes through `singleflight` plus a second DB check inside it, so concurrent workers never create the same album twice. Cancelling the context (SIGINT/SIGTERM through `withSignals`) lets uploads already in progress finish.
- **Upload.** `photos.UploadMedia` does a raw byte upload, then a `batchCreate` for one item. `doWithRetry` retries on 429, 5xx and network errors with exponential backoff (up to 60s, 6 retries). It rebuilds the request body with `req.GetBody`, so any new request type must set `GetBody`. `BatchCreate` can take up to 50 items, but it is only ever called with one. The `bytes_uploaded` status and `SetMediaUploadToken` exist but nothing uses them.
- **Album rules** (`takeout.albumFromPath`): remove the leading `Takeout/Google Photos/`. Files directly in that folder, and files in `Photos from YYYY` folders, go to the library root. Any other top-level folder name becomes the album name.
- **Finding a file's JSON sidecar** (`takeout.FindSidecar`) tries, in order:
  1. `<file>.supplemental-metadata.json`
  2. `<file>.json`
  3. any `<file>.*.json`, which catches the names Takeout cuts short
  4. any JSON file in the same folder whose `title` field matches the file name

  `nonSidecarJSON` lists folder-level JSON files to skip. To support a new media type, add the extension to `mediaExtensions` in `sidecar.go` **and** to `mimeType` in `upload.go`.
- **Restoring metadata** (`restore-metadata`, on by default) runs `exiftool -overwrite_original` to write the date/time and GPS position from the sidecar into the file before upload. Errors are logged and the upload continues. In dir mode this **changes the user's original files**.
- **Errors for files that are too large** are recognised only by pattern-matching `error_message` text in `store.GetLargeFileErrors` (`HTTP 413`, `too large`, …). If you change the wording of upload errors, keep these patterns matching.
- **Schema** changes are goose SQL files in `internal/store/migrations/`, embedded with `//go:embed` and applied when the DB is opened. Add a new numbered file; never edit `00001_initial.sql`. The DB is opened with `MaxOpenConns(1)`, so all writes go through one connection.
- **OAuth** (`internal/auth`): a loopback redirect flow. Each sign-in uses a random `state`, which the callback checks before accepting a code, and PKCE (`S256`), so the code exchange must present the matching verifier. Keep both if you change the flow. The token is cached at `~/.config/photo-migrator/token.json`. The scopes are `photoslibrary.appendonly` and `photoslibrary.edit.appcreateddata`. If you change them, also update the OAuth consent-screen steps in the README.

## Configuration and known pitfalls

- Settings are resolved by Viper in this order: CLI flag > `PHOTO_MIGRATOR_<KEY>` environment variable > `~/.config/photo-migrator/config.toml` > default. Config keys use the same kebab-case names as the flags. Read path settings with `viperPath()`, which expands `~`; plain `viper.GetString` does not.
- **Bind flags to viper only in `initConfig`.** It runs as `PersistentPreRunE` and binds the flags of the command actually being run. Never call `viper.BindPFlag` while building a command: viper keys are global, and several commands define the same flag (`credentials`, `takeout-dir`, `staging-dir`, …), so each binding would replace the last and only the last-registered command's flags would be read. Defaults for keys shared across commands are set with `viper.SetDefault` in `initConfig` (using the `default*` constants), so `config show` can report them. `cmd/photo-migrator/main_test.go` tests this through `newRootCmd()`.
- **A dry run must not change upload progress in the ledger.** When `pool.DryRun()` is true, `processDirectory` skips `RegisterMedia`, `processZips` leaves zip statuses alone, and the worker returns before uploading or touching the DB. Media rows are keyed by staging path, so even a `pending` row written during a dry run, under a different `staging-dir` from the real run, would leave its zip incomplete forever. Only `RegisterZip` (discovery) still happens. `TestDryRun_DoesNotChangeLedger` covers this.
- The `config init` template in `main.go` and the `config show` key list are maintained by hand. Update both when adding a config key.
- `migrator.db`, `_staging/` and the built `photo-migrator` binary in the repo root are local working files, not source code.
