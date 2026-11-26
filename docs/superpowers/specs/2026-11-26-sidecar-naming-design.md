# Sidecar lookup: Takeout's other naming patterns — design

Part of ROADMAP item 7. Follows the `wrong-dates` report (`2026-11-26-wrong-dates-report-design.md`).

## Why

`takeout.FindSidecar` (and the zip `SidecarIndex.Lookup`, which shares its rules through `findSidecar`) finds no sidecar for 2,168 of the user's 16.7k uploaded photos. For those, `restore-metadata` wrote no date before upload, and `wrong-dates` counts them as "without a sidecar date" instead of judging them. Takeout did export a sidecar for nearly all of them, under names the four current strategies don't recognise. Fixing the lookup lets future uploads get their dates and lets the report judge about 1,750 photos it can read (they would be added to the checklist if wrong).

## What the real listings show

From the file listings of the user's 27 zips (no files extracted):

| Pattern | Rows | Example media → sidecar |
|---|---|---|
| Edited copy | ~1,080 | `IMG_x-edited.jpg` → `IMG_x.jpg.supplemental-metadata.json` (the original's) |
| Live Photo / motion video | ~640 | `IMG_7297.MP4` → `IMG_7297.HEIC.supplemental-metadata.json` (the image with the same stem) |
| Numbered duplicate | ~220 | `X(1).jpg` → `X.jpg.supplemental-metadata(1).json`, or truncated `X.jpg.supplemental-metada(1).json` |
| Long name cut short | ~210 | `…BURST20191129125653882_COV.jpg` → `…BURST20191129125653882_CO.json` |

A simulation of the rules below over the real listings finds a sidecar for 2,148 of 2,168; 20 remain.

It also found an existing bug: strategy 3 (any `<file>.*.json`) can give `X.jpg` the sidecar of its duplicate `X(1).jpg`, because `X.jpg.supplemental-metada(1).json` sorts before `X.jpg.supplemental-metada.json`. 16 of the user's photos are affected; they may have had the wrong date written.

## Goals

- The shared lookup finds the sidecar for the four patterns above.
- Strategy 3 never returns a numbered duplicate's sidecar (`…(N).json`) for a file without the same `(N)`.
- Saved expected dates are looked up again with the new rules on the next `wrong-dates` run.

## Non-goals

- Changing files already uploaded (the API can't).
- Whether edited copies or Live Photo videos should be uploaded at all (ROADMAP items 8 and 9).
- The ~20 photos still without a sidecar.

## Anti-goal

- Writing a wrong date into a file. A wrong match is worse than no match: each new rule matches only when exactly one sidecar fits, and otherwise finds nothing.

## Approaches considered

- **Extend the shared rules in `findSidecar` (chosen).** One place, used by `restore-metadata` (filesystem) and `wrong-dates` (zip index), so the two cannot drift.
- **Per-command lookups.** Rejected: the report and the upload would disagree about a photo's date.
- **Match by the sidecar's `title` field only.** Rejected: Takeout cuts long media names short but keeps the full original name in `title`, so equality fails for exactly the cases that need it.

## Design

The rules run after the existing four strategies, in this order, on the media file's base name and its folder's file list (from disk or from the zip index):

0. **Fix strategy 3:** skip candidates ending in `(N).json` unless the media name has the same `(N)` before its extension.
1. **Numbered duplicate:** for `X(N).ext`, the sidecar starts with `X.ext.` and ends with `(N).json`.
2. **Edited copy:** for a name ending in `-edited` (optionally followed by `(N)`) before the extension, look up the name without `-edited` using all rules (so `-EFFECTS-edited.jpg` finds `-EFFECTS.jpg`'s sidecar, and `-edited(1)` finds the `(1)` sidecar).
3. **Live Photo / motion video:** for a `.mp4`/`.mov` file, find an image in the same folder with the same stem (`.heic`, `.jpg`, `.jpeg`) and use that image's sidecar (found with all rules).
4. **Name cut short:** a sidecar whose name without `.json` (and without a trailing `(N)`, which must equal the media's `(N)`) is a prefix of the media's name, at least 30 characters long; the longest such prefix wins, and a tie means no match.

Every rule returns a sidecar only when exactly one candidate fits.

**Re-lookup:** a new migration sets `taken_time_source = NULL, taken_time = NULL` on every row, so the next `wrong-dates` run looks every photo up again with the new rules (it already does this for rows with no saved lookup; the sidecar index makes this a few seconds). Rows recorded during uploads are re-looked-up the same way.

## Testing

- Table tests of `findSidecar` for each rule, built from the real examples above, including: `X.jpg` with both `…metada.json` and `…metada(1).json` gets the former; `-EFFECTS-edited`; `-edited(1)`; a `.MP4` next to a `.HEIC`; a cut-short name; a tie between two prefixes → no match; a `.MP4` with two same-stem images whose sidecars differ → no match.
- The same cases through `SidecarIndex.Lookup` (zip) and `FindSidecar` (filesystem).
- Migration test: saved expected dates are cleared, nothing else changes.

## Left to the builder

- How to structure the rules inside `findSidecar`.
- The exact list of image extensions for rule 3 beyond `.heic`, `.jpg`, `.jpeg`.
