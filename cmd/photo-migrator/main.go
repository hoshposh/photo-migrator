package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/hoshposh/photo-migrator/internal/auth"
	"github.com/hoshposh/photo-migrator/internal/metadata"
	"github.com/hoshposh/photo-migrator/internal/photos"
	"github.com/hoshposh/photo-migrator/internal/pipeline"
	"github.com/hoshposh/photo-migrator/internal/store"
	"github.com/hoshposh/photo-migrator/internal/takeout"
)

// configPath holds the value of the --config flag.
var configPath string

func main() {
	os.Exit(exitCode(newRootCmd().Execute(), os.Stderr))
}

// exitCode reports the result of a command on w and maps it to a process
// exit code: 0 success, 130 interrupted (the shell convention for Ctrl+C),
// 1 any other error.
func exitCode(err error, w io.Writer) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, context.Canceled):
		_, _ = fmt.Fprintln(w, "Interrupted. Progress is saved; run the same command again to resume.")
		return 130
	default:
		_, _ = fmt.Fprintln(w, "Error:", err)
		return 1
	}
}

// newRootCmd builds the full command tree.
func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "photo-migrator",
		Short: "Migrate Google Photos from a Takeout archive to a new account",
		Long: `photo-migrator migrates a Google Photos library by:

  1. Parsing a Google Takeout archive (a directory of .zip files OR a
     pre-extracted directory).
  2. Tracking every media file in a local SQLite database (migrator.db).
  3. Uploading media to a destination Google Photos account via the
     Google Photos Library API.

All progress is persisted — interrupted runs resume exactly where they left off.
New zip files added to --takeout-dir are detected automatically on each run.

Config file (loaded automatically if it exists):
  ~/.config/photo-migrator/config.toml

  Run 'photo-migrator config init' to generate a starter config file.

Quick start:
  photo-migrator auth  --credentials credentials.json
  photo-migrator run   --takeout-dir /path/to/takeout-zips/ --credentials credentials.json
  photo-migrator status`,
		// initConfig runs before every subcommand so viper is ready.
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			// Flags parsed fine, so any error from here on is a runtime
			// failure or an interrupt, not a usage mistake: don't print usage.
			cmd.SilenceUsage = true
			return initConfig(cmd)
		},
		// main prints errors itself (see exitCode), once, with the right
		// wording for interrupts.
		SilenceErrors: true,
	}

	// --config overrides the config file location.
	root.PersistentFlags().StringVar(&configPath, "config", "",
		"config file (default: ~/.config/photo-migrator/config.toml)")

	// Persistent flags — values sourced from config file when not set on the CLI.
	root.PersistentFlags().String("db-path", "./migrator.db",
		"path to the SQLite state ledger")
	root.PersistentFlags().String("log-level", "info",
		"log verbosity: debug | info | warn | error")

	root.AddCommand(
		newAuthCmd(),
		newScanCmd(),
		newUploadCmd(),
		newRunCmd(),
		newStatusCmd(),
		newLargeFilesCmd(),
		newLockedFilesCmd(),
		newMotionVideosCmd(),
		newConfigCmd(),
	)
	return root
}

// initConfig loads the TOML config file and binds persistent flags to viper.
// It is called by PersistentPreRunE so viper is fully populated before RunE runs.
func initConfig(cmd *cobra.Command) error {
	if configPath != "" {
		viper.SetConfigFile(configPath)
	} else {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("resolve home directory: %w", err)
		}
		viper.AddConfigPath(filepath.Join(home, ".config", "photo-migrator"))
		viper.SetConfigName("config")
		viper.SetConfigType("toml")
	}

	// Allow env overrides: PHOTO_MIGRATOR_DB_PATH, etc.
	viper.SetEnvPrefix("PHOTO_MIGRATOR")
	viper.AutomaticEnv()

	// Silently ignore "file not found" — the config file is optional.
	if err := viper.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			return fmt.Errorf("read config file: %w", err)
		}
	}

	// Defaults for keys whose flags live on other subcommands, so commands
	// like 'config show' still report them.
	viper.SetDefault("credentials", defaultCredentials)
	viper.SetDefault("staging-dir", defaultStagingDir)
	viper.SetDefault("workers", defaultWorkers)
	viper.SetDefault("rate-limit", defaultRateLimit)
	viper.SetDefault("dry-run", false)
	viper.SetDefault("restore-metadata", defaultRestoreMetadata)

	// Bind the flags of the command actually being executed so CLI values win
	// over the config file. Binding must happen here, not when commands are
	// constructed: viper keys are global, so binding the same key (e.g.
	// "credentials") from several commands at construction time leaves only
	// the last-registered command's flag connected.
	if err := viper.BindPFlags(cmd.Root().PersistentFlags()); err != nil {
		return fmt.Errorf("bind persistent flags: %w", err)
	}
	if err := viper.BindPFlags(cmd.Flags()); err != nil {
		return fmt.Errorf("bind flags: %w", err)
	}
	return nil
}

// Built-in defaults shared by flag definitions and viper.
const (
	defaultCredentials     = "credentials.json"
	defaultStagingDir      = "./_staging"
	defaultWorkers         = 5
	defaultRateLimit       = 4.0
	defaultRestoreMetadata = true
)

// defaultConfigPath returns the default config file path.
func defaultConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".config", "photo-migrator", "config.toml"), nil
}

// ─────────────────────────────────────────────────────────────
// auth
// ─────────────────────────────────────────────────────────────

func newAuthCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "Authenticate with the destination Google Photos account",
		Long: `Signs in to the destination Google Photos account: the account your photos
are migrated to, not the one the Takeout archive came from. A browser window
will open to complete the sign-in.

The resulting token is cached at ~/.config/photo-migrator/token.json and
reused (with automatic refresh) by subsequent commands. Run auth again at any
time to replace it, for example when a run stops because Google rejected the
saved sign-in.

Prerequisites — see README.md for step-by-step instructions:
  1. Create a Google Cloud project and enable the Photos Library API.
  2. Create an OAuth 2.0 "Desktop app" client ID and download credentials.json.
  3. Add your destination email as a Test User on the OAuth consent screen.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			credPath := viperPath("credentials")
			oauthCfg, err := auth.LoadConfig(credPath)
			if err != nil {
				return credentialErr(credPath, err)
			}
			client, err := auth.Login(context.Background(), oauthCfg)
			if err != nil {
				return fmt.Errorf("authorization failed: %w", err)
			}
			_ = client
			fmt.Println("\n✓ Authorization complete.")
			fmt.Println("  Token cached at ~/.config/photo-migrator/token.json")
			fmt.Println("  You can now run: photo-migrator run --takeout-dir <path>")
			return nil
		},
	}
	cmd.Flags().String("credentials", defaultCredentials,
		"path to the OAuth2 credentials.json downloaded from Google Cloud Console")
	return cmd
}

// ─────────────────────────────────────────────────────────────
// scan
// ─────────────────────────────────────────────────────────────

func newScanCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "scan",
		Short: "Discover zip archives and register them in the database",
		Long: `Scans --takeout-dir for .zip files and registers any new ones in the
local database. No extraction or upload is performed.

Run this command after downloading new zip files from Google Takeout to
register them ahead of the next 'run' invocation.

In directory mode (no .zip files found), this command counts media files
and reports the total without registering them individually.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			logger := buildLogger()
			takeoutDir := viperPath("takeout-dir")
			if takeoutDir == "" {
				return fmt.Errorf("--takeout-dir is required (or set takeout-dir in config file)")
			}
			st, err := store.Open(viperPath("db-path"))
			if err != nil {
				return fmt.Errorf("open database: %w", err)
			}
			defer func() {
				if cerr := st.Close(); cerr != nil && err == nil {
					err = cerr
				}
			}()

			return runScan(cmd.Context(), takeoutDir, st, logger)
		},
	}
	cmd.Flags().String("takeout-dir", "",
		"path to directory containing .zip files or a pre-extracted Takeout directory")
	return cmd
}

// ─────────────────────────────────────────────────────────────
// upload
// ─────────────────────────────────────────────────────────────

func newUploadCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "upload",
		Short: "Upload all pending items from the database",
		Long: `Reads all pending media items from the local database and uploads them
to the destination Google Photos account.

For zip-mode databases this command re-extracts each pending zip to the
staging directory, uploads its contents, then removes the staging files
before moving to the next zip.

Use 'run' instead of 'upload' when starting fresh — it combines discovery
and upload in a single step.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			logger := buildLogger()
			st, err := store.Open(viperPath("db-path"))
			if err != nil {
				return fmt.Errorf("open database: %w", err)
			}
			defer func() {
				if cerr := st.Close(); cerr != nil && err == nil {
					err = cerr
				}
			}()

			dryRun := viper.GetBool("dry-run")
			var photosClient *photos.Client
			if !dryRun {
				photosClient, err = buildPhotosClient(viperPath("credentials"))
				if err != nil {
					return err
				}
			}

			restoreMetadata := viper.GetBool("restore-metadata")
			if restoreMetadata {
				if err := metadata.CheckExiftool(); err != nil {
					return err
				}
			}

			pool := newPool(
				viper.GetInt("workers"),
				viper.GetFloat64("rate-limit"),
				dryRun, restoreMetadata, logger, photosClient, st,
			)
			return runUpload(cmd.Context(), viperPath("staging-dir"), st, pool, logger)
		},
	}
	addUploadFlags(cmd)
	return cmd
}

// ─────────────────────────────────────────────────────────────
// run
// ─────────────────────────────────────────────────────────────

func newRunCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Scan and upload in a single step (recommended)",
		Long: `Combines discovery and upload into one command:

  Zip mode  (--takeout-dir contains .zip files):
    For each pending zip file (alphabetical order):
      1. Extract to --staging-dir/<zip-name>/
      2. Walk the extracted directory, upload all media
      3. Delete staging files, mark zip complete in database
    New .zip files added to --takeout-dir since the last run are
    picked up automatically — no flags needed.

  Directory mode (--takeout-dir is a pre-extracted directory):
    Walk the directory and upload all media directly.

Runs are fully idempotent: files already marked complete in the database
are skipped.

Stopping: press Ctrl-C once to stop gracefully — no new uploads start, uploads
already in progress finish, and the current zip resumes on the next run.
Press Ctrl-C again to quit immediately.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			logger := buildLogger()

			takeoutDir := viperPath("takeout-dir")
			if takeoutDir == "" {
				return fmt.Errorf("--takeout-dir is required (or set takeout-dir in config file)")
			}

			st, err := store.Open(viperPath("db-path"))
			if err != nil {
				return fmt.Errorf("open database: %w", err)
			}
			defer func() {
				if cerr := st.Close(); cerr != nil && err == nil {
					err = cerr
				}
			}()

			dryRun := viper.GetBool("dry-run")
			var photosClient *photos.Client
			if !dryRun {
				photosClient, err = buildPhotosClient(viperPath("credentials"))
				if err != nil {
					return err
				}
			}
			restoreMetadata := viper.GetBool("restore-metadata")
			if restoreMetadata {
				if err := metadata.CheckExiftool(); err != nil {
					return err
				}
			}
			pool := newPool(
				viper.GetInt("workers"),
				viper.GetFloat64("rate-limit"),
				dryRun, restoreMetadata, logger, photosClient, st,
			)
			ctx, stopSignals := withSignals(cmd.Context())
			defer stopSignals()

			zipMode, err := takeout.IsZipMode(takeoutDir)
			if err != nil {
				return err
			}
			if zipMode {
				return runZipMode(ctx, takeoutDir, viperPath("staging-dir"), st, pool, logger)
			}
			return runDirMode(ctx, takeoutDir, st, pool, logger)
		},
	}
	cmd.Flags().String("takeout-dir", "",
		"path to directory of .zip archives or a pre-extracted Takeout directory")
	addUploadFlags(cmd)
	return cmd
}

// ─────────────────────────────────────────────────────────────
// status
// ─────────────────────────────────────────────────────────────

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show migration progress",
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := store.Open(viperPath("db-path"))
			if err != nil {
				return fmt.Errorf("open database: %w", err)
			}
			defer func() {
				if cerr := st.Close(); cerr != nil && err == nil {
					err = cerr
				}
			}()

			stats, err := st.GetStats()
			if err != nil {
				return err
			}

			fmt.Println()
			fmt.Printf("  📦  Zip Archives   %d / %d complete\n", stats.CompleteZips, stats.TotalZips)
			fmt.Printf("  🖼   Media Items    %d total\n", stats.TotalMedia)
			fmt.Printf("       ✓ complete    %d\n", stats.CompleteMedia)
			fmt.Printf("       ⏳ pending    %d\n", stats.PendingMedia)
			fmt.Printf("       ✗ errors     %d\n", stats.ErrorMedia)
			if stats.TooLargeMedia > 0 {
				fmt.Printf("       ⚠ too large  %d  (upload manually: see 'photo-migrator large-files')\n", stats.TooLargeMedia)
			}
			if stats.LockedMedia > 0 {
				fmt.Printf("       🔒 held back %d  (Locked Folder, not uploaded: see 'photo-migrator locked-files')\n", stats.LockedMedia)
			}
			if stats.MotionMedia > 0 {
				fmt.Printf("       🎞 motion    %d  (motion-photo videos, not uploaded: see 'photo-migrator motion-videos')\n", stats.MotionMedia)
			}
			fmt.Printf("  🗂   Albums         %d / %d created\n", stats.CompleteAlbums, stats.TotalAlbums)

			if stats.TotalZips > 0 {
				pct := float64(stats.CompleteZips) / float64(stats.TotalZips) * 100
				fmt.Printf("\n  Overall progress: %.1f%% (%d / %d zips)\n", pct, stats.CompleteZips, stats.TotalZips)
				if stats.TotalZips > stats.CompleteZips {
					fmt.Printf("  Note: media item total grows as new zips are opened\n")
				}
			}
			fmt.Println()
			return nil
		},
	}
}

// ─────────────────────────────────────────────────────────────
// large-files
// ─────────────────────────────────────────────────────────────

// manualFileReport is the JSON-serialisable representation of one file the
// user must handle by hand (too large, or held back from the Locked Folder).
type manualFileReport struct {
	File        string `json:"file"`
	SourceZip   string `json:"source_zip,omitempty"`
	InZipPath   string `json:"in_zip_path,omitempty"`
	ExtractedTo string `json:"extracted_to,omitempty"`
	Album       string `json:"album,omitempty"`
	Error       string `json:"error,omitempty"`
}

// manualReport describes a report of files the user must handle by hand.
type manualReport struct {
	use, short, long string
	extractHelp      string
	empty            string   // printed when there is nothing to report
	title            string   // header; %d is the file count
	advice           []string // lines printed under the header
	query            func(*store.Store) ([]store.ManualItem, error)
}

func newLargeFilesCmd() *cobra.Command {
	return newManualFilesCmd(manualReport{
		use:   "large-files",
		short: "Report files that were too large to upload via the API",
		long: `Queries the database for media items that failed because they exceeded
the Google Photos API file-size limit (~200 MB). These files were never
uploaded and must be added to your library manually via the web UI:

  https://photos.google.com/

For each file the report shows:
  - The filename and album it belongs to
  - The source .zip archive that contains it
  - The relative path inside the zip

Flags:
  --format json    Emit machine-readable JSON instead of a text table
  --extract <dir>  Re-extract the large files from their source zips into
                   <dir> so you can upload them immediately from disk`,
		extractHelp: "re-extract large files from their source zips into this directory",
		empty:       "  ✓ No files were rejected for being too large.",
		title:       "⚠️  Large Files Report — %d file(s) too large for the Google Photos API",
		advice:      []string{"  These files must be uploaded manually via:", "  → https://photos.google.com/"},
		query:       (*store.Store).GetLargeFileErrors,
	})
}

func newLockedFilesCmd() *cobra.Command {
	return newManualFilesCmd(manualReport{
		use:   "locked-files",
		short: "List Locked Folder files held back from upload",
		long: `Lists photos and videos from your Google Photos Locked Folder. They are
private, and the Google Photos API cannot add files to the destination
account's Locked Folder, so photo-migrator does not upload them: they would
appear in the main library, visible on every device.

To move them over yourself:
  1. Extract them:  photo-migrator locked-files --extract <dir>
  2. Add them to the destination account (for example from your phone).
  3. In the Google Photos mobile app, select them and choose
     "Move to Locked Folder".

Flags:
  --format json    Emit machine-readable JSON instead of a text table
  --extract <dir>  Extract the held-back files from their source zips into <dir>`,
		extractHelp: "extract the held-back files from their source zips into this directory",
		empty:       "  ✓ No Locked Folder files are being held back.",
		title:       "🔒  Locked Folder — %d file(s) held back (not uploaded, to keep them private)",
		advice: []string{
			"  The Google Photos API cannot add files to a Locked Folder. To move them",
			"  yourself: extract them (--extract <dir>), add them to the destination",
			"  account, then use \"Move to Locked Folder\" in the Google Photos mobile app.",
		},
		query: (*store.Store).GetLockedFiles,
	})
}

func newMotionVideosCmd() *cobra.Command {
	return newManualFilesCmd(manualReport{
		use:   "motion-videos",
		short: "List motion-photo video parts held back from upload",
		long: `Lists the video halves of motion photos (for example PXL_x.MP or
PXL_x.MP.COVER, exported next to the still image PXL_x.MP.jpg). The still
image is uploaded as a normal photo, but the Google Photos API cannot rebuild
a motion photo: its video would appear as a separate video next to the
still. So photo-migrator holds these videos back.

Flags:
  --format json    Emit machine-readable JSON instead of a text table
  --extract <dir>  Extract the videos from their source zips into <dir>`,
		extractHelp: "extract the motion-photo videos from their source zips into this directory",
		empty:       "  ✓ No motion-photo videos are being held back.",
		title:       "🎞  Motion photos — %d video part(s) held back (the still images were uploaded)",
		advice: []string{
			"  The Google Photos API cannot rebuild motion photos; uploading these would",
			"  add each as a separate video next to its still image.",
		},
		query: (*store.Store).GetMotionVideos,
	})
}

// newManualFilesCmd builds a report command (large-files, locked-files).
func newManualFilesCmd(spec manualReport) *cobra.Command {
	var format string
	var extractDir string

	cmd := &cobra.Command{
		Use:   spec.use,
		Short: spec.short,
		Long:  spec.long,
		RunE: func(cmd *cobra.Command, args []string) error {
			logger := buildLogger()
			st, err := store.Open(viperPath("db-path"))
			if err != nil {
				return fmt.Errorf("open database: %w", err)
			}
			defer func() {
				if cerr := st.Close(); cerr != nil && err == nil {
					err = cerr
				}
			}()

			items, err := spec.query(st)
			if err != nil {
				return fmt.Errorf("query %s: %w", spec.use, err)
			}

			if len(items) == 0 {
				fmt.Println()
				fmt.Println(spec.empty)
				fmt.Println()
				return nil
			}

			reports := make([]manualFileReport, 0, len(items))
			for _, it := range items {
				rep := manualFileReport{
					File:      filepath.Base(it.SourcePath),
					SourceZip: it.SourceZip,
					Album:     it.AlbumName,
					Error:     it.ErrorMsg,
				}
				if it.SourceZip != "" {
					rep.InZipPath = zipEntryOf(it)
				}
				reports = append(reports, rep)
			}

			// --extract: pull the files back out of their source zips.
			if extractDir != "" {
				if err := extractManualFiles(items, extractDir, logger); err != nil {
					return fmt.Errorf("extract: %w", err)
				}
				// Annotate each report with where the file landed.
				for i, it := range items {
					if it.SourceZip != "" {
						reports[i].ExtractedTo = filepath.Join(extractDir, filepath.FromSlash(zipEntryOf(it)))
					} else {
						// Dir mode: original path is stable.
						reports[i].ExtractedTo = it.SourcePath
					}
				}
			}

			switch format {
			case "json":
				return printManualFilesJSON(reports)
			default:
				return printManualFilesText(spec, reports, extractDir)
			}
		},
	}

	cmd.Flags().StringVar(&format, "format", "text", "output format: text | json")
	cmd.Flags().StringVar(&extractDir, "extract", "", spec.extractHelp)
	return cmd
}

// zipEntryOf returns the item's path inside its source zip: the recorded
// zip_entry, or (for rows predating that column) one derived from source_path.
func zipEntryOf(it store.ManualItem) string {
	if it.ZipEntry != "" {
		return it.ZipEntry
	}
	return inZipPath(it.SourcePath, it.SourceZip)
}

// inZipPath derives the relative path of a file inside its source zip by
// stripping the staging directory prefix that was added during extraction.
//
// The staging layout is: <stagingDir>/<zip-stem>/<in-zip-contents>
// source_path looks like:  /abs/staging/takeout-001/Google Photos/Album/file.mp4
// source_zip  looks like:  /abs/takeout-dir/takeout-001.zip
//
// We strip everything up to and including "/<zip-stem>/" to recover the
// original zip-relative path.
func inZipPath(sourcePath, sourceZip string) string {
	// stem is e.g. "takeout-20240101-001"
	stem := strings.TrimSuffix(filepath.Base(sourceZip), filepath.Ext(sourceZip))
	// Find the stem directory component in the source path.
	needle := string(os.PathSeparator) + stem + string(os.PathSeparator)
	if idx := strings.Index(sourcePath, needle); idx >= 0 {
		relOS := sourcePath[idx+len(needle):]
		return filepath.ToSlash(relOS)
	}
	// Fallback: just the basename.
	return filepath.Base(sourcePath)
}

// extractManualFiles groups items by their source zip and calls
// takeout.ExtractFiles for each zip, pulling only the oversized files.
func extractManualFiles(items []store.ManualItem, destDir string, logger *zap.Logger) error {
	// Group by source zip.
	type entry struct {
		item  store.ManualItem
		inZip string
	}
	zipGroups := make(map[string][]entry)
	var dirItems []store.ManualItem

	for _, it := range items {
		if it.SourceZip == "" {
			dirItems = append(dirItems, it)
			continue
		}
		zipGroups[it.SourceZip] = append(zipGroups[it.SourceZip], entry{
			item:  it,
			inZip: zipEntryOf(it),
		})
	}

	for zipPath, entries := range zipGroups {
		want := make(map[string]struct{}, len(entries))
		for _, e := range entries {
			want[e.inZip] = struct{}{}
		}
		logger.Info("extracting files from zip",
			zap.String("zip", filepath.Base(zipPath)),
			zap.Int("count", len(want)),
		)
		if err := takeout.ExtractFiles(zipPath, destDir, want, logger); err != nil {
			return fmt.Errorf("%s: %w", filepath.Base(zipPath), err)
		}
	}

	// Dir-mode items: the original path is still on disk; just note them.
	for _, it := range dirItems {
		logger.Info("large file (dir mode — original path intact)",
			zap.String("path", it.SourcePath),
		)
	}
	return nil
}

func printManualFilesText(spec manualReport, reports []manualFileReport, extractDir string) error {
	fmt.Printf("\n  "+spec.title+"\n", len(reports))
	fmt.Println()
	for _, line := range spec.advice {
		fmt.Println(line)
	}
	if extractDir != "" {
		fmt.Printf("  → Files extracted to: %s\n", extractDir)
	}
	fmt.Println()

	dividerStr := "  " + strings.Repeat("-", 80)

	for i, r := range reports {
		fmt.Printf("  %d. %s\n", i+1, r.File)
		if r.Album != "" {
			fmt.Printf("     Album     : %s\n", r.Album)
		}
		if r.SourceZip != "" {
			fmt.Printf("     Source zip: %s\n", filepath.Base(r.SourceZip))
			if r.InZipPath != "" {
				fmt.Printf("     In-zip    : %s\n", r.InZipPath)
			}
		}
		if r.ExtractedTo != "" {
			fmt.Printf("     Extracted : %s\n", r.ExtractedTo)
		}
		if i < len(reports)-1 {
			fmt.Println(dividerStr)
		}
	}
	fmt.Println()
	return nil
}

func printManualFilesJSON(reports []manualFileReport) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(reports)
}

// ─────────────────────────────────────────────────────────────
// config
// ─────────────────────────────────────────────────────────────

func newConfigCmd() *cobra.Command {
	configCmd := &cobra.Command{
		Use:   "config",
		Short: "Manage the photo-migrator configuration file",
	}
	configCmd.AddCommand(newConfigInitCmd())
	configCmd.AddCommand(newConfigShowCmd())
	return configCmd
}

func newConfigInitCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Write a starter config file to the default location",
		Long: `Creates ~/.config/photo-migrator/config.toml with all available keys and
their default values, pre-populated with comments explaining each option.

Re-run with --force to overwrite an existing file.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			dest := configPath
			if dest == "" {
				var err error
				dest, err = defaultConfigPath()
				if err != nil {
					return err
				}
			}

			if _, err := os.Stat(dest); err == nil && !force {
				return fmt.Errorf("config file already exists at %s\n  Use --force to overwrite", dest)
			}

			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				return fmt.Errorf("create config directory: %w", err)
			}

			content := `# photo-migrator configuration
# Location: ~/.config/photo-migrator/config.toml
#
# CLI flags always override values set here.
# Environment variables (PHOTO_MIGRATOR_<KEY>) also override config values.

# ── Paths ─────────────────────────────────────────────────────

# Path to the SQLite state ledger.
db-path = "~/data/migrator.db"

# Directory containing Google Takeout .zip files, or a pre-extracted directory.
takeout-dir = "~/data/takeout-zips"

# Temporary directory used during zip extraction (cleaned up after each zip).
staging-dir = "~/data/staging"

# OAuth2 credentials.json downloaded from Google Cloud Console.
credentials = "~/.config/photo-migrator/credentials.json"

# ── Upload tuning ─────────────────────────────────────────────

# Number of concurrent upload goroutines.
workers = 5

# Max Google Photos API requests per second (stays under the 75 req/15s quota).
rate-limit = 4.0

# ── Logging ───────────────────────────────────────────────────

# Log verbosity: debug | info | warn | error
log-level = "info"

# ── Misc ──────────────────────────────────────────────────────

# Log planned uploads without actually uploading anything.
dry-run = false

# Write original date/time and GPS from Takeout sidecar JSON into file
# metadata before uploading. Requires exiftool installed on the system.
# If true and exiftool is not found, the program exits with an error.
# Set to false if exiftool is not available.
restore-metadata = true
`
			if err := os.WriteFile(dest, []byte(content), 0o600); err != nil {
				return fmt.Errorf("write config file: %w", err)
			}

			fmt.Printf("✓ Config file written to: %s\n", dest)
			fmt.Println("  Edit it to set your paths, then run photo-migrator without flags.")
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing config file")
	return cmd
}

func newConfigShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "Print the active configuration",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfgFile := viper.ConfigFileUsed()
			if cfgFile == "" {
				fmt.Println("No config file loaded (using CLI flags and defaults).")
			} else {
				fmt.Printf("Config file: %s\n\n", cfgFile)
			}
			keys := []struct{ key, desc string }{
				{"db-path", "SQLite state ledger"},
				{"takeout-dir", "Takeout source directory"},
				{"staging-dir", "Zip extraction staging directory"},
				{"credentials", "OAuth2 credentials.json"},
				{"workers", "Concurrent upload goroutines"},
				{"rate-limit", "API requests per second"},
				{"log-level", "Log verbosity"},
				{"dry-run", "Dry-run mode"},
				{"restore-metadata", "Restore metadata from sidecar before upload"},
			}
			for _, k := range keys {
				val := viper.Get(k.key)
				// Show expanded paths so users can confirm tilde resolution.
				if s, ok := val.(string); ok {
					val = expandPath(s)
				}
				fmt.Printf("  %-14s = %v  (%s)\n", k.key, val, k.desc)
			}
			return nil
		},
	}
}

// ─────────────────────────────────────────────────────────────
// Core logic helpers
// ─────────────────────────────────────────────────────────────

// runScan discovers and registers zip files (zip mode) or reports stats (dir mode).
func runScan(ctx context.Context, takeoutDir string, st *store.Store, logger *zap.Logger) error {
	zipMode, err := takeout.IsZipMode(takeoutDir)
	if err != nil {
		return err
	}
	if !zipMode {
		logger.Info("directory mode: use 'run' to walk and upload the directory")
		return nil
	}

	zips, err := takeout.ListZips(takeoutDir)
	if err != nil {
		return err
	}
	registered := 0
	for _, z := range zips {
		if err := st.RegisterZip(z); err != nil {
			return fmt.Errorf("register %s: %w", z, err)
		}
		registered++
	}
	logger.Info("scan complete",
		zap.Int("discovered", len(zips)),
		zap.Int("registered", registered),
	)
	return nil
}

// runUpload uploads pending items from the database (used by 'upload' command).
func runUpload(ctx context.Context, stagingDir string, st *store.Store, pool *pipeline.Pool, logger *zap.Logger) error {
	pendingZips, err := st.GetPendingZips()
	if err != nil {
		return err
	}
	if len(pendingZips) > 0 {
		if err := os.MkdirAll(stagingDir, 0o755); err != nil {
			return fmt.Errorf("create staging dir: %w", err)
		}
		return processZips(ctx, pendingZips, stagingDir, st, pool, logger)
	}

	// Dir mode: upload items already registered in the DB.
	return uploadFromDB(ctx, st, pool, logger)
}

// runZipMode discovers zips, registers new ones, then processes them sequentially.
func runZipMode(ctx context.Context, takeoutDir, stagingDir string, st *store.Store, pool *pipeline.Pool, logger *zap.Logger) error {
	zips, err := takeout.ListZips(takeoutDir)
	if err != nil {
		return err
	}
	// Register any newly downloaded zips.
	for _, z := range zips {
		if err := st.RegisterZip(z); err != nil {
			return fmt.Errorf("register zip: %w", err)
		}
	}
	pendingZips, err := st.GetPendingZips()
	if err != nil {
		return err
	}
	logger.Info("starting migration",
		zap.Int("total_zips", len(zips)),
		zap.Int("pending_zips", len(pendingZips)),
	)
	if len(pendingZips) == 0 {
		logger.Info("nothing to do — all zips are complete")
		return nil
	}

	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		return fmt.Errorf("create staging dir: %w", err)
	}
	return processZips(ctx, pendingZips, stagingDir, st, pool, logger)
}

// processZips extracts and uploads each zip sequentially, cleaning up staging
// files after each zip completes.
//
// In dry-run mode zips are still extracted and walked (so planned uploads are
// logged) but their status in the ledger is never changed.
func processZips(ctx context.Context, zips []string, stagingDir string, st *store.Store, pool *pipeline.Pool, logger *zap.Logger) error {
	setZipStatus := func(zipPath string, status store.Status, errMsg string) error {
		if pool.DryRun() {
			return nil
		}
		return st.SetZipStatus(zipPath, status, errMsg)
	}

	for i, zipPath := range zips {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		logger.Info("processing zip",
			zap.Int("n", i+1), zap.Int("total", len(zips)),
			zap.String("zip", zipPath),
		)

		if err := setZipStatus(zipPath, store.StatusExtracting, ""); err != nil {
			return err
		}
		extractRoot, err := takeout.ExtractZip(ctx, zipPath, stagingDir, logger)
		if err != nil {
			if ctx.Err() != nil {
				return interrupted(ctx, zipPath, setZipStatus, logger)
			}
			logger.Error("extraction failed", zap.String("zip", zipPath), zap.Error(err))
			_ = setZipStatus(zipPath, store.StatusError, err.Error())
			continue // try next zip
		}

		if err := setZipStatus(zipPath, store.StatusUploading, ""); err != nil {
			return err
		}

		uploadErr := processDirectory(ctx, extractRoot, zipPath, st, pool, logger)

		// Remove staging files regardless of success/error — completed items
		// are tracked in the DB; failures will be re-extracted on the next run.
		if err := os.RemoveAll(extractRoot); err != nil {
			logger.Warn("cleanup staging", zap.String("dir", extractRoot), zap.Error(err))
		}

		if ctx.Err() != nil {
			return interrupted(ctx, zipPath, setZipStatus, logger)
		}
		if errors.Is(uploadErr, photos.ErrAuthFailed) {
			// Not this zip's fault: leave it pending and stop the whole run.
			_ = setZipStatus(zipPath, store.StatusPending, "")
			return uploadErr
		}
		if uploadErr != nil {
			logger.Error("upload phase failed", zap.String("zip", zipPath), zap.Error(uploadErr))
			_ = setZipStatus(zipPath, store.StatusError, uploadErr.Error())
		}

		if pool.DryRun() {
			logger.Info("dry-run: zip walked, status unchanged", zap.String("zip", zipPath))
			continue
		}

		// Mark the zip complete only when all its items succeeded.
		allDone, err := st.AllZipItemsComplete(zipPath)
		if err != nil {
			return err
		}
		if allDone {
			if err := st.SetZipStatus(zipPath, store.StatusComplete, ""); err != nil {
				return err
			}
			logger.Info("zip complete ✓", zap.String("zip", zipPath))
		} else {
			logger.Warn("zip has errors — will retry on next run", zap.String("zip", zipPath))
			_ = st.SetZipStatus(zipPath, store.StatusError, "some items failed")
		}
	}
	return nil
}

// interrupted handles a Ctrl+C during a zip: the zip goes back to pending
// with no error (it resumes on the next run; finished uploads are skipped)
// and ctx.Err() is returned so the command exits as interrupted.
func interrupted(ctx context.Context, zipPath string, setZipStatus func(string, store.Status, string) error, logger *zap.Logger) error {
	_ = setZipStatus(zipPath, store.StatusPending, "")
	logger.Info("interrupted — zip will resume on the next run", zap.String("zip", zipPath))
	return ctx.Err()
}

// processDirectory walks a directory, registers and uploads all media files.
func processDirectory(ctx context.Context, dir, sourceZip string, st *store.Store, pool *pipeline.Pool, logger *zap.Logger) error {
	jobs := make(chan pipeline.Job, 200)
	walkErr := make(chan error, 1)

	// The walker gets its own context so it can be released if the pool
	// stops early (fatal error) and nothing is left to drain the channel.
	walkCtx, cancelWalk := context.WithCancel(ctx)
	defer cancelWalk()

	go func() {
		defer close(jobs)
		err := takeout.WalkDirectory(walkCtx, dir, sourceZip, func(path, zipEntry string) (bool, error) {
			return st.IsDone(sourceZip, zipEntry, path)
		}, func(j takeout.MigrationJob) {
			// Locked Folder files are private: hold them back unless a review
			// has approved them (status set back to pending).
			if j.Locked {
				if pool.DryRun() {
					logger.Info("dry-run: Locked Folder file would be held back", zap.String("file", filepath.Base(j.MediaPath)))
					return
				}
				held, err := st.RegisterHeld(store.StatusLocked, j.MediaPath, j.SourceZip, j.ZipEntry)
				if err != nil {
					logger.Warn("register locked file", zap.String("path", j.MediaPath), zap.Error(err))
					return // never upload a Locked Folder file we could not record
				}
				if held {
					logger.Info("held back: Locked Folder file not uploaded (see 'photo-migrator locked-files')",
						zap.String("file", filepath.Base(j.MediaPath)))
					return
				}
			} else if j.MotionPart {
				// The API cannot rebuild a motion photo: its video half would
				// become a separate video. Hold it back unless released.
				if pool.DryRun() {
					logger.Info("dry-run: motion-photo video would be held back", zap.String("file", filepath.Base(j.MediaPath)))
					return
				}
				held, err := st.RegisterHeld(store.StatusMotionVideo, j.MediaPath, j.SourceZip, j.ZipEntry)
				if err != nil {
					logger.Warn("register motion video", zap.String("path", j.MediaPath), zap.Error(err))
					return
				}
				if held {
					logger.Info("held back: motion-photo video not uploaded (see 'photo-migrator motion-videos')",
						zap.String("file", filepath.Base(j.MediaPath)))
					return
				}
			} else if !pool.DryRun() {
				// Register in DB before handing off to the worker pool. Skipped on
				// dry runs: rows are keyed by (staging) path, and rows a real run
				// never revisits would keep their zip incomplete forever.
				if err := st.RegisterMedia(j.MediaPath, j.SourceZip, j.ZipEntry, j.AlbumName); err != nil {
					logger.Warn("register media", zap.String("path", j.MediaPath), zap.Error(err))
				}
			}
			select {
			case jobs <- pipeline.Job{MigrationJob: j}:
			case <-walkCtx.Done():
			}
		}, logger)
		walkErr <- err
	}()

	poolErr := pool.Run(ctx, jobs)
	cancelWalk()
	werr := <-walkErr

	switch {
	case poolErr != nil:
		return poolErr
	case ctx.Err() != nil:
		return ctx.Err() // interrupted
	case werr != nil:
		return fmt.Errorf("walk directory: %w", werr)
	}
	return nil
}

// runDirMode walks a pre-extracted directory and uploads everything.
func runDirMode(ctx context.Context, takeoutDir string, st *store.Store, pool *pipeline.Pool, logger *zap.Logger) error {
	logger.Info("directory mode", zap.String("dir", takeoutDir))
	return processDirectory(ctx, takeoutDir, "", st, pool, logger)
}

// uploadFromDB uploads pending items already registered in the DB (dir mode resume).
func uploadFromDB(ctx context.Context, st *store.Store, pool *pipeline.Pool, logger *zap.Logger) error {
	items, err := st.GetPendingMedia()
	if err != nil {
		return err
	}
	logger.Info("uploading from database", zap.Int("pending", len(items)))

	jobs := make(chan pipeline.Job, 200)
	prodCtx, cancelProd := context.WithCancel(ctx)
	defer cancelProd() // release the producer if the pool stops early
	go func() {
		defer close(jobs)
		for _, item := range items {
			sidecarPath, sidecar, _ := takeout.FindSidecar(item.SourcePath)
			select {
			case jobs <- pipeline.Job{
				MigrationJob: takeout.MigrationJob{
					MediaPath:   item.SourcePath,
					SidecarPath: sidecarPath,
					AlbumName:   item.AlbumName,
					SourceZip:   item.SourceZip,
					Sidecar:     sidecar,
				},
			}:
			case <-prodCtx.Done():
				return
			}
		}
	}()

	if err := pool.Run(ctx, jobs); err != nil {
		return err
	}
	return ctx.Err() // non-nil only if interrupted
}

// ─────────────────────────────────────────────────────────────
// Shared flag/helper utilities
// ─────────────────────────────────────────────────────────────

// addUploadFlags registers upload-related flags on cmd. They are bound to
// viper by initConfig when cmd is the command being executed.
func addUploadFlags(cmd *cobra.Command) {
	f := cmd.Flags()
	f.String("credentials", defaultCredentials,
		"path to OAuth2 credentials.json from Google Cloud Console")
	f.String("staging-dir", defaultStagingDir,
		"temporary directory for zip extraction (auto-cleaned per zip)")
	f.Int("workers", defaultWorkers,
		"number of concurrent upload goroutines")
	f.Float64("rate-limit", defaultRateLimit,
		"max Google Photos API requests per second (default stays under the 75 req/15s quota)")
	f.Bool("dry-run", false,
		"log planned uploads without actually uploading")
	f.Bool("restore-metadata", defaultRestoreMetadata,
		"write original date/time and GPS from Takeout sidecar into file metadata before uploading (requires exiftool)")
}

func newPool(workers int, rateLimit float64, dryRun, restoreMetadata bool, logger *zap.Logger, client *photos.Client, st *store.Store) *pipeline.Pool {
	return pipeline.NewPool(pipeline.Config{
		Workers:         workers,
		RateLimit:       rateLimit,
		DryRun:          dryRun,
		RestoreMetadata: restoreMetadata,
		Logger:          logger,
	}, client, st)
}

func buildPhotosClient(credPath string) (*photos.Client, error) {
	oauthCfg, err := auth.LoadConfig(credPath)
	if err != nil {
		return nil, credentialErr(credPath, err)
	}
	httpClient, err := auth.GetClient(context.Background(), oauthCfg)
	if err != nil {
		return nil, fmt.Errorf("get authenticated client: %w", err)
	}
	return photos.New(httpClient), nil
}

func credentialErr(credPath string, err error) error {
	return fmt.Errorf(
		"could not load credentials from %q: %w\n\nHave you completed setup?\n"+
			"  1. Enable the Google Photos Library API in Google Cloud Console.\n"+
			"  2. Create an OAuth 2.0 \"Desktop app\" client ID and download credentials.json.\n"+
			"  3. Run: photo-migrator auth --credentials %s\n"+
			"  See README.md for step-by-step instructions",
		credPath, err, credPath)
}

func buildLogger() *zap.Logger {
	level := zapcore.InfoLevel
	switch viper.GetString("log-level") {
	case "debug":
		level = zapcore.DebugLevel
	case "warn":
		level = zapcore.WarnLevel
	case "error":
		level = zapcore.ErrorLevel
	}
	cfg := zap.NewProductionConfig()
	cfg.Level = zap.NewAtomicLevelAt(level)
	// The production default samples repeated messages (~100/s each), which
	// silently drops per-file lines. Every upload and error must be logged.
	cfg.Sampling = nil
	cfg.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	logger, _ := cfg.Build()
	return logger
}

// expandPath expands a leading "~/" to the user's home directory.
// The shell performs this expansion for CLI arguments, but NOT for values read
// from a config file, so we must do it ourselves.
func expandPath(p string) string {
	if p == "~" {
		home, err := os.UserHomeDir()
		if err != nil {
			return p
		}
		return home
	}
	if strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return p
		}
		return filepath.Join(home, p[2:])
	}
	return p
}

// viperPath returns a viper string value with tilde expansion applied.
func viperPath(key string) string {
	return expandPath(viper.GetString(key))
}

// withSignals returns a context that is cancelled on the first SIGINT or
// SIGTERM, so no new work starts while uploads already in progress finish.
// A second signal exits the process immediately with code 130. Call the
// returned stop function to release the signal handler.
func withSignals(ctx context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancel(ctx)
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		if _, ok := <-sigs; !ok {
			return
		}
		fmt.Fprintln(os.Stderr, "\nStopping: finishing uploads already in progress. Press Ctrl+C again to quit immediately.")
		cancel()
		if _, ok := <-sigs; !ok {
			return
		}
		fmt.Fprintln(os.Stderr, "\nQuitting immediately. Unfinished uploads will be retried on the next run.")
		exitFunc(130)
	}()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			signal.Stop(sigs)
			close(sigs)
		})
	}
	return ctx, stop
}

// exitFunc exits the process; a variable so tests can intercept it.
var exitFunc = os.Exit
