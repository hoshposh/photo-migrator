# Sidecar Lookup for Takeout's Other Names — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The shared sidecar lookup finds the sidecar for edited copies, Live Photo videos, numbered duplicates and cut-short names, stops giving a file its duplicate's sidecar, and every saved expected date is looked up again.

**Architecture:** All rules live in `findSidecar` (`internal/takeout/sidecar.go`), which both `FindSidecar` (filesystem, used by the upload walker / `restore-metadata`) and `SidecarIndex.Lookup` (zip, used by `wrong-dates`) call. The rules use only the folder's JSON names plus `parse`, because the zip index stores JSON names only. A goose migration clears saved expected dates so `wrong-dates` re-looks them up.

**Tech Stack:** Go, goose migrations, `archive/zip` test fixtures.

**Spec:** `docs/superpowers/specs/2026-11-26-sidecar-naming-design.md`

## Global Constraints

- A wrong match is worse than no match: every new rule returns a sidecar only when exactly one candidate fits; otherwise no sidecar.
- Rules run after the existing strategies 1–4, in the spec's order: numbered duplicate, edited copy, Live Photo video, cut-short name. Strategy 3 is fixed in place (rule 0).
- `FindSidecar`'s signature and return contract (`"", nil, nil` on a miss) and `SidecarIndex.Lookup`'s signature do not change.
- Cut-short minimum: 30 characters of sidecar-name prefix.
- Never edit existing migrations; the new one is `internal/store/migrations/00009_relookup_taken_dates.sql`.
- Builds stay `CGO_ENABLED=0`; `task ci` passes after every task; no new dependencies.
- Commits: plain imperative subjects like the history; no `Co-Authored-By` or `Claude-Session` lines.

## Review Focus

1. **Two different photos share a long name prefix** (`…_1687607112.jpg` and `…_1687607113.jpg`) and one cut-short sidecar `…_168760711.json`. Expected: only the photo whose stem is a prefix of the sidecar's `title` gets it; the other gets none. Test in Task 1.
2. **A file with both `X.jpg.supplemental-metada.json` and `X.jpg.supplemental-metada(1).json` in its folder.** Expected: `X.jpg` gets the first, `X(1).jpg` the second. Test in Task 1.
3. **A Live Photo video whose stem matches two images with sidecars** (`IMG_1.HEIC` and `IMG_1.JPG` both with sidecars). Expected: no match. Test in Task 1.
4. **An edited copy of a numbered duplicate** (`DSC_0180-edited(1).JPG`). Expected: `DSC_0180.JPG.supplemental-metadata(1).json`. Test in Task 1.
5. **Running the migration on a ledger in the middle of a migration** (rows `pending`, `complete`, held back). Expected: only `taken_time` and `taken_time_source` change. Test in Task 2.

---

### Task 1: The four naming rules and the duplicate fix in `findSidecar`

**Files:**
- Modify: `internal/takeout/sidecar.go` (`findSidecar` and helpers; `FindSidecar` unchanged in signature)
- Test: `internal/takeout/sidecar_test.go`, `internal/takeout/sidecarindex_test.go`
- Modify: `CLAUDE.md` (the "Finding a file's JSON sidecar" bullet: list the new rules in one or two lines each, and the "only when exactly one fits" rule)

**Interfaces:**
- Consumes: `findSidecar(base string, parse func(name string) (*Sidecar, error), list func() []string) (name string, s *Sidecar)`; `nonSidecarJSON`.
- Produces: same signatures. `list` may return non-JSON names (filesystem) or only JSON names (zip index); rules must work with JSON names only.

**Rules (from the spec, with the decisions the spec leaves open):**

- `(N)` means a parenthesised number directly before the extension: `X(1).jpg`. For a sidecar name, `(N)` directly before `.json`.
- **Rule 0 (strategy 3 fix):** a candidate ending in `(N).json` is skipped unless the media name has the same `(N)`.
- **Rule 1, numbered duplicate:** for media `X(N).ext`, candidates are JSON names that start with `X.ext.` and end with `(N).json`. Exactly one → match.
- **Rule 2, edited copy:** if the media name matches `^(.*)-edited(\(\d+\))?(\.[^.]+)$`, look up `<1><2><3>` (the name without `-edited`) with all strategies and rules; its answer is the answer. (`-EFFECTS-edited.jpg` → `-EFFECTS.jpg`.)
- **Rule 3, Live Photo / motion video:** if the extension is `.mp4` or `.mov` (any case), take the stem; candidate images are JSON names that start with `<stem>.` followed by one of `heic`, `jpg`, `jpeg` (any case) and then `.`; for each distinct image name `<stem>.<ext>` among them, look it up with all strategies and rules. Exactly one image yields a sidecar → match.
- **Rule 4, cut-short name:** strip `(N)` from the media name (remember N). A candidate JSON name, minus `.json` and minus a trailing `(M)` (M must equal N, both absent counts as equal), must be a prefix of the stripped media name and at least 30 characters long. Parse each candidate; keep only those whose `Title` starts with the media's stem (stripped name without its extension). Exactly one left → match. (Longest-prefix tie-breaking is replaced by the title check plus "exactly one".)

- [ ] **Step 1: Write the failing tests** — one table test `TestFindSidecar_TakeoutNames` in `sidecar_test.go` that writes each case's files into a temp dir (sidecar JSON with `title` and `photoTakenTime`) and calls `FindSidecar`; and `TestSidecarIndex_TakeoutNames` in `sidecarindex_test.go` running the same table through a zip and `Lookup`. Share the table (a package-level `var takeoutNameCases`). Cases (media → expected sidecar file name, or "" for none):
  - `X.jpg` with `X.jpg.supplemental-metada.json` and `X.jpg.supplemental-metada(1).json` → `X.jpg.supplemental-metada.json` (Review Focus 2)
  - `X(1).jpg` (same folder) → `X.jpg.supplemental-metada(1).json`
  - `image000000(2).jpg` with `image000000.jpg.supplemental-metadata(2).json` → that file
  - `IMG_20141108_150533-edited.jpg` with `IMG_20141108_150533.jpg.supplemental-metadata.json` → that file
  - `IMG_0359-EFFECTS-edited.jpg` with `IMG_0359-EFFECTS.jpg.supplemental-metadata.json` → that file
  - `DSC_0180-edited(1).JPG` with `DSC_0180.JPG.supplemental-metadata.json` and `DSC_0180.JPG.supplemental-metadata(1).json` → the `(1)` file (Review Focus 4)
  - `IMG_7297.MP4` with `IMG_7297.HEIC.supplemental-metadata.json` → that file
  - `MVIMG_20190901_150451.MP4` with `MVIMG_20190901_150451.jpg.supplemental-metadata.json` → that file
  - `IMG_1.MP4` with `IMG_1.HEIC.supplemental-metadata.json` and `IMG_1.JPG.supplemental-metadata.json` → "" (Review Focus 3)
  - `00100dPORTRAIT_00100_BURST20191129125653882_COV.jpg` with `00100dPORTRAIT_00100_BURST20191129125653882_CO.json` (title `00100dPORTRAIT_00100_BURST20191129125653882_COVER.jpg`) → that file
  - `PXL_20220924_162743977_exported_1397_1687607112.jpg` and `PXL_20220924_162743977_exported_1397_1687607113.jpg` with `PXL_20220924_162743977_exported_1397_168760711.json` (title `PXL_20220924_162743977_exported_1397_1687607112098.jpg`) → the first gets it, the second gets "" (Review Focus 1)
  - `PXL_20220924_162743977_exported_1397_1687607112(1).jpg` with `PXL_20220924_162743977_exported_1397_168760711(1).json` (title `PXL_20220924_162743977_exported_1397_1687607112098~2.jpg`) → that file
  - `short(1).jpg` with only `short.jpg.supplemental-metadata.json` → "" (no `(1)` sidecar; must not fall back to the original's)
  - A short name under 30 characters whose prefix sidecar exists (`IMG_1234.jpg` with `IMG_12.json`, title `IMG_1234.jpg`) → "" (below the minimum)
  
  The existing `TestFindSidecar_Strategies` and `TestSidecarIndex_Strategies` must pass unchanged.
- [ ] **Step 2: Run them to verify they fail** — `go test -count=1 -run 'TakeoutNames' ./internal/takeout/` → FAIL (most cases find no sidecar; the `X.jpg` case gets the `(1)` file).
- [ ] **Step 3: Implement the rules in `sidecar.go`** — keep `findSidecar` readable: one small helper per rule, called in order after strategies 1–4; recursion for rules 2 and 3 goes through `findSidecar` itself (guard against `-edited` loops by stripping only once per call).
- [ ] **Step 4: Run the takeout and metadata tests** — `go test -count=1 ./internal/takeout/ ./internal/metadata/` → PASS; then `task ci`.
- [ ] **Step 5: Update the CLAUDE.md bullet and commit** — `git commit -m "Find sidecars for edited copies, Live Photo videos, duplicates and cut-short names"`

---

### Task 2: Look every expected date up again

**Files:**
- Create: `internal/store/migrations/00009_relookup_taken_dates.sql`
- Test: `internal/store/dates_test.go`
- Modify: `ROADMAP.md` (item 7 Status line: the naming gaps are fixed; ~20 photos remain without a sidecar), `README.md` only if its `wrong-dates` section says expected dates are never looked up again (add: "after an upgrade that improves sidecar matching, the next run looks all photos up again")

**Interfaces:**
- Consumes: migrations 00001–00008; `store.Open`, `GetDateRows`.
- Produces: nothing new.

- [ ] **Step 1: Write the failing test** — `TestMigration00009_ClearsTakenDates`: build a DB at version 8 with `goose.UpTo` (as the existing migration tests do), insert rows: `complete` with `taken_time=1560535320, taken_time_source='sidecar'`, `complete` with `taken_time_source='none'`, a `pending` row, and a `motion_video` row, each with a Google date and URL. Open the store. Every row has `taken_time IS NULL` and `taken_time_source IS NULL`; `status`, `photos_media_id`, `google_time`, `google_checked_at`, `google_missing`, `google_url` are unchanged (Review Focus 5).
- [ ] **Step 2: Run it to verify it fails** — `go test -count=1 -run TestMigration00009 ./internal/store/` → FAIL.
- [ ] **Step 3: Write the migration** — Up: `UPDATE media_items SET taken_time = NULL, taken_time_source = NULL;` with a two-line comment saying why (sidecar matching improved; the next `wrong-dates` run looks every photo up again). Down: no-op (`SELECT 1;`), with a comment that cleared dates are simply looked up again.
- [ ] **Step 4: `go test -count=1 ./internal/store/ ./cmd/photo-migrator/` → PASS; `task ci`**
- [ ] **Step 5: Update ROADMAP/README and commit** — `git commit -m "Look every photo's expected date up again after the sidecar fixes"`

---

### Task 3: Check on real data (controller, not a subagent)

- [ ] **Step 1:** With no upload running, copy the user's ledger: `sqlite3 -readonly ~/data/migrator.db ".backup <scratchpad>/relookup.db"`.
- [ ] **Step 2:** Build the branch and run `wrong-dates --db-path <scratchpad>/relookup.db --format json > <scratchpad>/relookup.json`.
- [ ] **Step 3:** Expected: "without a sidecar date" drops from ~1.6k photos to a few dozen; report the new wrong count to the user, and spot-check three newly judged photos (one per rule) by comparing their `should_be` with the sidecar's `photoTakenTime` read from the zip.
