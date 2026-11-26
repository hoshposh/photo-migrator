# photo-migrator

A CLI tool to migrate a Google Photos library from one account to another using a [Google Takeout](https://takeout.google.com) archive.

**Key features:**
- Processes Takeout `.zip` files **one at a time** — you only need disk space for the largest single zip, not the entire archive.
- Fully **resumable** — all state is tracked in a local SQLite database; interrupted runs pick up exactly where they left off.
- **Incremental** — download more zip files and re-run; new ones are detected and queued automatically.
- **Idempotent** — files already uploaded are skipped, even across multiple runs.
- **Best-effort album preservation** — named album folders are recreated in the destination account; year-based folders (`Photos from YYYY`), print-order folders (`Print Order <number>`) and Takeout's special `Archive` and `Failed Videos` folders upload to the library root. (Archived status itself cannot be recreated through the API.)

---

## Prerequisites

- Go 1.26.11 (the version in `go.mod`)
- [mise](https://mise.jdx.dev/) (recommended) — installs the pinned Go version plus `task`, `golangci-lint`, and the `sqlite3` CLI for inspecting the database (see [`.mise.toml`](.mise.toml))
- [exiftool](https://exiftool.org/) — used to write original dates and GPS from Takeout metadata into each file before upload (`sudo apt install libimage-exiftool-perl`). Optional if you set `restore-metadata = false`.
- A [Google Takeout](https://takeout.google.com) export from the **source** account (Account A)
- A **destination** Google Photos account (Account B) with enough storage

---

## Step 1 — Install tools and build

This project uses [Task](https://taskfile.dev) as its build runner. All tasks
are defined in [`Taskfile.yml`](Taskfile.yml).

```bash
git clone https://github.com/hoshposh/photo-migrator
cd photo-migrator

# Install project dev tools (go, task, golangci-lint, sqlite3) via mise
mise install

# Build the binary
task build
```

The binary is placed at `./photo-migrator`.

### Common tasks

| Command | Description |
|---|---|
| `task setup` | Install dev tools via mise |
| `task build` | Build the binary (incremental — skips if sources unchanged) |
| `task build:race` | Build with Go race detector |
| `task test` | Run all unit tests |
| `task test:cover` | Tests + HTML coverage report |
| `task lint` | Run `golangci-lint` |
| `task vet` | Run `go vet` |
| `task fmt` | Auto-format all Go files |
| `task db:status` | Show upload counts per status from `migrator.db` |
| `task db:zips` | Show zip archive statuses |
| `task db:errors` | List files with upload errors |
| `task db:shell` | Open interactive sqlite3 shell on `migrator.db` |
| `task clean` | Remove binary and coverage artifacts |
| `task clean:staging` | Remove the `_staging` extraction directory |
| `task ci` | Full pipeline: lint → test → build |

Run `task --list` at any time to see all available tasks.

---

## Configuration file

All CLI flags can be stored in a TOML config file so you don't have to repeat them on every invocation.

```bash
# Write a starter config to ~/.config/photo-migrator/config.toml
./photo-migrator config init

# Edit it to set your paths
$EDITOR ~/.config/photo-migrator/config.toml

# Print the active configuration (shows which file was loaded)
./photo-migrator config show
```

The config file is **optional** — the tool works fine without one. Values are resolved in this order (highest priority first):

1. Explicit CLI flag
2. Environment variable (`PHOTO_MIGRATOR_<KEY>`, e.g. `PHOTO_MIGRATOR_DB_PATH`)
3. Config file value
4. Built-in default

Example `config.toml`:

```toml
db-path     = "~/data/migrator.db"
takeout-dir = "~/data/takeout-zips"
staging-dir = "~/data/staging"
credentials = "~/.config/photo-migrator/credentials.json"
workers     = 5
rate-limit  = 4.0
log-level   = "info"
dry-run     = false
restore-metadata = true
```

Use `--config /path/to/other.toml` to load an alternate config file.

> [!TIP]
> If you store `credentials.json` at `~/.config/photo-migrator/credentials.json` and set all paths in the config file, subsequent commands need **no flags at all**: `./photo-migrator run`, `./photo-migrator status`, etc.

---

## Step 2 — Set up Google Cloud credentials

The tool uploads to Account B via the Google Photos Library API. You need an OAuth 2.0 client ID to authenticate.

> [!NOTE]
> These steps only need to be done **once**. The tool is used only by you; it does not need to be published to the App Store or verified by Google (unverified apps can have up to 100 test users).

### 2.1 Create a Google Cloud project

1. Go to [console.cloud.google.com](https://console.cloud.google.com/) and sign in with **any** Google account (does not have to be Account B).
2. Click **Select a project → New Project**.
3. Give it a name (e.g. `photo-migration`) and click **Create**.

### 2.2 Enable the Google Photos Library API

1. In the left menu, go to **APIs & Services → Library**.
2. Search for **Google Photos Library API** and click **Enable**.

### 2.3 Configure the OAuth consent screen

1. Go to **APIs & Services → OAuth consent screen**.
2. Select **External** as the User Type and click **Create**.
3. Fill in the required fields (App name, support email). You can use any values.
4. On the **Scopes** page, click **Add or Remove Scopes** and add both:
   ```
   https://www.googleapis.com/auth/photoslibrary.appendonly
   https://www.googleapis.com/auth/photoslibrary.edit.appcreateddata
   ```
   The first allows uploading new media; the second allows creating albums and adding the app's uploads to them. (The older catch-all `photoslibrary` scope is deprecated.)
5. On the **Test users** page, click **Add Users** and enter **Account B's email address**.
6. Click **Save and Continue**, then **Back to Dashboard**.

> [!IMPORTANT]
> While the app is in **Testing** mode you must add yourself as a test user. The token lasts 7 days in Testing mode; you may need to re-run `photo-migrator auth` once a week during a long migration.

### 2.4 Create an OAuth 2.0 client ID

1. Go to **APIs & Services → Credentials → Create Credentials → OAuth client ID**.
2. Application type: **Desktop app**.
3. Click **Create**, then **Download JSON**.
4. Save the downloaded file somewhere accessible, e.g. `~/.config/photo-migrator/credentials.json`.

---

## Step 3 — Authenticate

```bash
./photo-migrator auth --credentials credentials.json
```

Or, if `credentials` is already set in your config file:

```bash
./photo-migrator auth
```

A browser window will open. **Sign in as Account B** (the destination account). After authorizing, the token is cached at `~/.config/photo-migrator/token.json`.

---

## Step 4 — Prepare your Takeout archive

1. Request a Google Photos export from **Account A** at [takeout.google.com](https://takeout.google.com).
   - Select only **Google Photos**.
   - Choose **.zip** as the delivery format.
2. Download all the zip files (e.g. `takeout-20240101-001.zip` … `takeout-20240101-027.zip`) into a single directory, for example: `/data/takeout-zips/`.

> [!TIP]
> You can start the migration **before all zip files are downloaded**. The tool will process whichever zips are present, and newly downloaded ones are picked up automatically on the next run.

---

## Step 5 — Run the migration

```bash
./photo-migrator run \
  --takeout-dir /data/takeout-zips/ \
  --credentials credentials.json
```

The tool will:
1. Discover all `.zip` files in the directory and register any new ones in the database.
2. Extract each zip to `./_staging/<zip-name>/` (a few GB of temporary disk space).
3. Upload every media file via the Google Photos API.
4. Delete the staging directory for that zip before moving to the next one.

If your paths are set in the config file, no flags are needed:

```bash
./photo-migrator run
```

### Useful flags

| Flag | Default | Description |
|---|---|---|
| `--config` | `~/.config/photo-migrator/config.toml` | Alternate config file path |
| `--credentials` | `credentials.json` | OAuth2 client credentials file |
| `--workers` | `5` | Concurrent upload goroutines |
| `--rate-limit` | `4` | Max Photos API requests/sec |
| `--staging-dir` | `./_staging` | Temporary extraction directory |
| `--db-path` | `./migrator.db` | SQLite state ledger location |
| `--dry-run` | false | Extract and walk each zip, logging what would be uploaded, without uploading, editing files, or changing upload progress in the database (new zips are still registered) |
| `--restore-metadata` | true | Write original date/time and GPS from Takeout sidecar JSON into each file with exiftool before upload |
| `--log-level` | `info` | `debug` \| `info` \| `warn` \| `error` |

All of these flags (except `--config` itself) can be set in the config file instead.

With `restore-metadata` enabled, exiftool edits files in place. In zip mode these are throwaway staging copies, but in directory mode (a pre-extracted `--takeout-dir`) **your original files are modified**.

---

## Monitoring progress

```bash
./photo-migrator status
```

```
  📦  Zip Archives   3 / 27 complete
  🖼   Media Items    14823 total
       ✓ complete    14703
       ⏳ pending    114
       ✗ errors     6
  🗂   Albums         18 / 21 created

  Overall progress: 11.1% (3 / 27 zips)
  Note: media item total grows as new zips are opened
```

---

## Handling oversized files

The Google Photos API rejects files larger than **~200 MB** with an HTTP 413
error. These files are recorded in the database with `status='too_large'`: they
are **not retried** on later runs, they don't stop their zip from completing,
and `status` lists them separately. They must be uploaded manually through the
[Google Photos web UI](https://photos.google.com/).

Use the `large-files` command to generate a report of every affected file:

```bash
# Human-readable report (default)
./photo-migrator large-files

# Machine-readable JSON
./photo-migrator large-files --format json
```

Example output:

```
  ⚠️  Large Files Report — 1 file(s) too large for the Google Photos API

  These files must be uploaded manually via:
  → https://photos.google.com/

  1. VID_20200101_120000.mp4
     Album     : Holiday 2020
     Source zip: takeout-20240101-007.zip
     In-zip    : Takeout/Google Photos/Holiday 2020/VID_20200101_120000.mp4
```

### Re-extracting oversized files for manual upload

If the staging directory has already been cleaned up, the files only exist
inside their source `.zip` archives. Use `--extract` to pull them back out into
a directory you can browse and upload from:

```bash
./photo-migrator large-files --extract ~/data/large-files-to-upload
```

The command re-reads each source zip, extracts only the oversized files, and
prints the on-disk path of each extracted file in the report:

```
  1. VID_20200101_120000.mp4
     Album     : Holiday 2020
     Source zip: takeout-20240101-007.zip
     In-zip    : Takeout/Google Photos/Holiday 2020/VID_20200101_120000.mp4
     Extracted : /home/user/data/large-files-to-upload/Takeout/Google Photos/Holiday 2020/VID_20200101_120000.mp4
```

You can then open [photos.google.com](https://photos.google.com/), click
**Upload → From computer**, and select the extracted files.

> [!NOTE]
> The `large-files` command only reports and extracts — it does **not** retry
> the API upload, since the Google Photos API enforces the 200 MB limit
> regardless. Manual upload via the web UI bypasses this restriction.

---

## Checking for files that won't be migrated

A migration uploads every recognised photo and video, holds some back (Locked Folder files, motion-photo video parts) and skips everything else. To see what is skipped, before or during a migration:

```bash
./photo-migrator scan --skipped                 # grouped report
./photo-migrator scan --skipped --format json   # full list
```

This reads the zips in place (nothing is extracted) and does not open the database, so it is safe to run at any time, including on new Takeout downloads and while a `run` is in progress. It reports files that are not a recognised photo or video (for example PDFs), macOS `._` files, and how many `.json` metadata files there are. During a run, each zip that contains unrecognised files also logs one warning listing their types.

---

## Motion photos and videos with unusual names

Takeout exports a motion photo (e.g. from a Pixel phone) as two files: the still image (`PXL_x.MP.jpg` or `PXL_x.RAW-01.MP.COVER.jpg`) and its video (`PXL_x.MP`, `PXL_x.MP~2` or `PXL_x.RAW-01.MP.COVER`). The still may be in a different zip from its video. The still is uploaded as a normal photo. The Google Photos API cannot rebuild a motion photo, though, so its video would appear as a separate video next to the still. These videos are therefore **held back** with status `motion_video`; `status` counts them and `motion-videos` lists them (`--extract <dir>` pulls them out).

Other videos whose names don't end in a known extension (for example `Concert - 3.12.20`) are recognised by their content and uploaded with a matching extension added (`Concert - 3.12.20.mp4`).

---

## Locked Folder files are held back

Photos and videos from your Google Photos **Locked Folder** are private, and the Google Photos API cannot add files to the destination account's Locked Folder. Uploading them would put them in the main library, visible on every device. So `run` **does not upload them**: they are recorded with status `locked`, they don't stop their zip from completing, and `status` shows how many there are.

To list them, or extract them to move them over yourself:

```bash
./photo-migrator locked-files
./photo-migrator locked-files --extract ~/locked-to-review
```

Add the extracted files to the destination account (for example from your phone), then use **Move to Locked Folder** in the Google Photos mobile app.

---

## Stopping and resuming

Press **Ctrl+C** once to stop gracefully: no new uploads start, uploads already in progress are allowed to finish, and the tool exits with code 130. Press **Ctrl+C** a second time to quit immediately; any upload cut off that way is simply retried on the next run.

To resume, just re-run the same `run` command. The database tracks every file's state, so:
- Completed files are skipped instantly.
- Files that errored are retried.
- Zips that were partially extracted are re-extracted (already-uploaded files within them are skipped).

```bash
./photo-migrator run \
  --takeout-dir /data/takeout-zips/ \
  --credentials credentials.json
```

---

## Adding new zip files mid-migration

Download the new zip files into the same `--takeout-dir` directory and re-run `run`. The tool detects files not yet in the database and queues them automatically.

```bash
# New zips were downloaded while the previous run was processing...
./photo-migrator run \
  --takeout-dir /data/takeout-zips/ \
  --credentials credentials.json
```

---

## All commands

```
photo-migrator config init                     # write starter config to ~/.config/photo-migrator/config.toml
photo-migrator config show                     # print active configuration
photo-migrator auth        --credentials creds.json  # one-time OAuth2 setup
photo-migrator scan        --takeout-dir /path/to/zips/
photo-migrator scan        --skipped [--format json]   # read-only: files a migration would not upload
photo-migrator upload      [--staging-dir ...] [--workers N] [--rate-limit N]
photo-migrator run         --takeout-dir /path/to/zips/ --credentials creds.json
photo-migrator status
photo-migrator large-files                     # report files too large for the API
photo-migrator large-files --format json       # machine-readable JSON report
photo-migrator large-files --extract <dir>     # re-extract oversized files for manual upload
photo-migrator locked-files                    # list Locked Folder files held back from upload
photo-migrator locked-files --extract <dir>    # extract them to handle by hand
photo-migrator motion-videos                   # list motion-photo video parts held back from upload
```

Flags in brackets are optional if set in the config file. Use `--help` on any command for full flag documentation.

---

## Database schema

The SQLite database (`migrator.db`) uses [goose](https://github.com/pressly/goose) for schema migrations. The schema is embedded in the binary and applied automatically at startup. To inspect the database manually:

```bash
sqlite3 migrator.db ".schema"
sqlite3 migrator.db "SELECT status, COUNT(*) FROM media_items GROUP BY status"
sqlite3 migrator.db "SELECT zip_path, status FROM zip_archives ORDER BY zip_path"
```

---

## Rate limits

The Google Photos API allows approximately **75 requests per 15 seconds** per user. The default `--rate-limit 4` (requests/sec) stays comfortably within this. If you encounter `429 Too Many Requests` errors, lower the rate limit:

```bash
./photo-migrator run ... --rate-limit 2
```

The limit counts every API request, including retries; each file needs two (the upload, then a `batchCreate` that adds it to the library). Files upload in parallel, but `batchCreate` calls are made one at a time, as Google requires. On `429` and `5xx` responses the tool backs off exponentially (up to 60 s), and waits longer if Google's `Retry-After` header asks it to (up to 2 minutes).

---

## Troubleshooting

| Error | Resolution |
|---|---|
| `could not load credentials` | Run `photo-migrator auth --credentials credentials.json` |
| `authorization timed out` | Re-run `auth`; complete the browser flow within 3 minutes |
| `batchCreate item error: The caller does not have permission` | Your Google Cloud project may need Photos Library API access approval; check the OAuth consent screen and ensure Account B is listed as a test user |
| `byte upload: open media file: no such file or directory` | The staging directory was cleaned up; re-run `run` to re-extract and retry |
| `too large for the Google Photos API` / `HTTP 413` | The file exceeds the ~200 MB API limit and won't be retried; run `photo-migrator large-files --extract <dir>` and upload manually via [photos.google.com](https://photos.google.com/) |
| `google rejected the saved sign-in` / `invalid_grant` | Your saved sign-in has expired or been revoked (apps in Testing mode expire after 7 days). The run stops at the first such error and leaves the remaining files pending. Run `photo-migrator auth` to sign in again, then re-run `run`. |
