package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/aldinokemal/go-whatsapp-web-multidevice/infrastructure/dbreuse"
	"github.com/spf13/cobra"
)

// dbReuseCmd inspects and snapshots chatstorage without starting WhatsApp.
var dbReuseCmd = &cobra.Command{
	Use:   "db-reuse",
	Short: "Snapshot and check a chatstorage.db before another host opens it",
	Long: `Check whether a GOWA chatstorage.db can be opened by official v9.9.0.

The subcommands do not connect to WhatsApp, send messages, mark chats read,
import an unknown bridge, open whatsapp.db, or delete rows. snapshot writes one
consistent database file, including an open WAL. preflight reports schema and
registry alignment. gate blocks a real cutover unless the on-disk schema matches
the verified Mac revision and the writers were attested frozen. reconcile lists
message ids that exist on only one side and does not merge. overlay writes
target-only rows into a new file. coverage audits history JSON without inserting
it. materialize-media copies media bytes and follows symlinks.

The offline db-reuse binaries are the copies that cannot start WhatsApp.
history_complete is false in every report. Nothing here replaces a live database.`,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return cmd.Help()
	},
}

func init() {
	rootCmd.AddCommand(dbReuseCmd)

	snapshotCmd := &cobra.Command{
		Use:   "snapshot",
		Short: "Write a consistent single-file copy of chatstorage",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			src, _ := cmd.Flags().GetString("source")
			dst, _ := cmd.Flags().GetString("out")
			if src == "" || dst == "" {
				return fmt.Errorf("snapshot requires --source and --out")
			}
			live, _ := cmd.Flags().GetBool("allow-live-validation")
			writers, _ := cmd.Flags().GetString("attest-writers")
			result, err := dbreuse.SnapshotWithOptions(cmd.Context(), src, dst, dbreuse.SnapshotOptions{
				AllowLiveValidation: live,
				AttestWriters:       splitCSV(writers),
			})
			if result != nil {
				if writeErr := writeJSON(cmd.OutOrStdout(), result); writeErr != nil {
					return writeErr
				}
			}
			return err
		},
	}
	snapshotCmd.Flags().String("source", "", "path to chatstorage.db, with its -wal left beside it")
	snapshotCmd.Flags().String("out", "", "new snapshot file to create")
	snapshotCmd.Flags().Bool("allow-live-validation", false, "copy while -shm exists; not a cutover file")
	snapshotCmd.Flags().String("attest-writers", "", "source-gowa,source-periodic-merge,target-gowa")
	dbReuseCmd.AddCommand(snapshotCmd)

	preflightCmd := &cobra.Command{
		Use:   "preflight",
		Short: "Report whether chatstorage.db is reusable on v9.9.0",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path, _ := cmd.Flags().GetString("db")
			if path == "" {
				return fmt.Errorf("preflight requires --db")
			}
			report, err := dbreuse.Check(path)
			if report != nil {
				if writeErr := writeJSON(cmd.OutOrStdout(), report); writeErr != nil {
					return writeErr
				}
			}
			if err != nil {
				return err
			}
			if report == nil || !report.Reusable {
				return fmt.Errorf("chatstorage is not reusable")
			}
			return nil
		},
	}
	preflightCmd.Flags().String("db", "", "path to chatstorage.db")
	dbReuseCmd.AddCommand(preflightCmd)

	reconcileCmd := &cobra.Command{
		Use:   "reconcile",
		Short: "Compare two chatstorage databases without modifying them",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			source, _ := cmd.Flags().GetString("source")
			target, _ := cmd.Flags().GetString("target")
			work, _ := cmd.Flags().GetString("work-dir")
			out, _ := cmd.Flags().GetString("out")
			if source == "" || target == "" || work == "" {
				return fmt.Errorf("reconcile requires --source, --target, and --work-dir")
			}
			report, err := dbreuse.Reconcile(cmd.Context(), source, target, work)
			if report != nil && out != "" {
				f, createErr := os.OpenFile(out, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
				if createErr != nil {
					return createErr
				}
				writeErr := writeJSON(f, report)
				closeErr := f.Close()
				if writeErr != nil {
					return writeErr
				}
				if closeErr != nil {
					return closeErr
				}
			}
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "source_only=%d target_only=%d changed=%d same=%d history_complete=false\n",
				len(report.SourceOnly), len(report.TargetOnly), len(report.Changed), report.SameCount)
			return nil
		},
	}
	reconcileCmd.Flags().String("source", "", "Mac or other source chatstorage.db")
	reconcileCmd.Flags().String("target", "", "Linux chatstorage.db to keep as the backup of target-only rows")
	reconcileCmd.Flags().String("work-dir", "", "directory for the two snapshots")
	reconcileCmd.Flags().String("out", "", "optional JSON report path")
	dbReuseCmd.AddCommand(reconcileCmd)

	mediaCmd := &cobra.Command{
		Use:   "materialize-media",
		Short: "Copy a media tree, replacing symlinks with the target bytes",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			source, _ := cmd.Flags().GetString("source")
			dest, _ := cmd.Flags().GetString("dest")
			out, _ := cmd.Flags().GetString("out")
			if source == "" || dest == "" {
				return fmt.Errorf("materialize-media requires --source and --dest")
			}
			manifest, err := dbreuse.MaterializeMedia(source, dest)
			if manifest != nil && out != "" {
				f, createErr := os.OpenFile(out, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
				if createErr != nil {
					return createErr
				}
				writeErr := writeJSON(f, manifest)
				closeErr := f.Close()
				if writeErr != nil {
					return writeErr
				}
				if closeErr != nil {
					return closeErr
				}
			}
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "copied=%d present=%d missing=%d complete=%t\n",
				manifest.Copied, manifest.Present, manifest.Missing, manifest.Complete)
			return nil
		},
	}
	mediaCmd.Flags().String("source", "", "media root; symlinks such as _mac are followed")
	mediaCmd.Flags().String("dest", "", "directory that receives regular files")
	mediaCmd.Flags().String("out", "", "optional JSON manifest path")
	dbReuseCmd.AddCommand(mediaCmd)

	overlayCmd := &cobra.Command{
		Use:   "overlay",
		Short: "Stage target-only history onto a new copy of the source database",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			source, _ := cmd.Flags().GetString("source")
			target, _ := cmd.Flags().GetString("target")
			mapping, _ := cmd.Flags().GetString("map")
			work, _ := cmd.Flags().GetString("work-dir")
			out, _ := cmd.Flags().GetString("out")
			live, _ := cmd.Flags().GetBool("allow-live-validation")
			base, _ := cmd.Flags().GetString("base")
			report, err := dbreuse.Overlay(cmd.Context(), dbreuse.OverlayRequest{
				Source: source, Target: target, MapPath: mapping, WorkDir: work, Out: out, AllowLiveValidation: live, Base: base,
			})
			if report != nil {
				if writeErr := writeJSON(cmd.OutOrStdout(), report); writeErr != nil {
					return writeErr
				}
			}
			return err
		},
	}
	overlayCmd.Flags().String("source", "", "source chatstorage.db")
	overlayCmd.Flags().String("target", "", "target chatstorage.db")
	overlayCmd.Flags().String("map", "", "account mapping JSON")
	overlayCmd.Flags().String("work-dir", "", "directory for read-only snapshots")
	overlayCmd.Flags().String("out", "", "new staged database")
	overlayCmd.Flags().Bool("allow-live-validation", false, "allow -shm on the inputs")
	overlayCmd.Flags().String("base", "source", "source keeps the source registry; target keeps the target registry")
	dbReuseCmd.AddCommand(overlayCmd)

	mediaRefsCmd := &cobra.Command{
		Use:   "media-refs",
		Short: "Report message media fields without selecting local bytes",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path, _ := cmd.Flags().GetString("db")
			archives, _ := cmd.Flags().GetStringArray("archive")
			root, _ := cmd.Flags().GetString("media-root")
			report, err := dbreuse.MediaReferences(dbreuse.MediaRefRequest{DB: path, Archives: archives, MediaRoot: root})
			if report != nil {
				if writeErr := writeJSON(cmd.OutOrStdout(), report); writeErr != nil {
					return writeErr
				}
			}
			if err != nil {
				return err
			}
			if report != nil && (report.SubsetProven || report.WholeTreeCopyAllowed || report.PurgeAllowed || report.HistoryComplete) {
				return fmt.Errorf("media-refs must not claim a proven byte subset")
			}
			return nil
		},
	}
	mediaRefsCmd.Flags().String("db", "", "chatstorage snapshot")
	mediaRefsCmd.Flags().StringArray("archive", nil, "history-sync JSON file")
	mediaRefsCmd.Flags().String("media-root", "", "refused; display names are not matched to a tree")
	dbReuseCmd.AddCommand(mediaRefsCmd)

	coverageCmd := &cobra.Command{
		Use:   "coverage",
		Short: "Report which history-sync JSON ids are already in a snapshot",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path, _ := cmd.Flags().GetString("db")
			archives, _ := cmd.Flags().GetStringArray("archive")
			report, err := dbreuse.Coverage(path, archives)
			if report != nil {
				if writeErr := writeJSON(cmd.OutOrStdout(), report); writeErr != nil {
					return writeErr
				}
			}
			if err != nil {
				return err
			}
			if report != nil && report.HistoryComplete {
				return fmt.Errorf("coverage must not claim complete history")
			}
			return nil
		},
	}
	importCmd := &cobra.Command{
		Use:   "import-archive",
		Short: "Insert absent history-sync messages into a staged snapshot",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path, _ := cmd.Flags().GetString("db")
			account, _ := cmd.Flags().GetString("account")
			audit, _ := cmd.Flags().GetString("audit")
			archives, _ := cmd.Flags().GetStringArray("archive")
			report, err := dbreuse.ImportArchives(dbreuse.ImportRequest{DB: path, Account: account, Audit: audit, Archives: archives})
			if report != nil {
				if writeErr := writeJSON(cmd.OutOrStdout(), report); writeErr != nil {
					return writeErr
				}
			}
			if err != nil {
				return err
			}
			if report != nil && (report.HistoryComplete || report.MetadataOnlyIsProof) {
				return fmt.Errorf("import-archive must not claim complete history")
			}
			return nil
		},
	}
	importCmd.Flags().String("db", "", "staged chatstorage snapshot")
	importCmd.Flags().String("account", "", "registry storage JID")
	importCmd.Flags().String("audit", "", "private audit database")
	importCmd.Flags().StringArray("archive", nil, "history-sync JSON file")
	dbReuseCmd.AddCommand(importCmd)

	auditVerifyCmd := &cobra.Command{
		Use:   "audit-verify",
		Short: "Check staged history against the source, target, and private audit",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			staged, _ := cmd.Flags().GetString("staged")
			source, _ := cmd.Flags().GetString("source")
			target, _ := cmd.Flags().GetString("target")
			mapping, _ := cmd.Flags().GetString("map")
			audit, _ := cmd.Flags().GetString("audit")
			base, _ := cmd.Flags().GetString("base")
			retain, _ := cmd.Flags().GetBool("retain-missing")
			report, err := dbreuse.AuditVerify(cmd.Context(), dbreuse.AuditVerifyRequest{
				Staged: staged, Source: source, Target: target, MapPath: mapping, Audit: audit, Base: base, RetainMissing: retain,
			})
			if report != nil {
				if writeErr := writeJSON(cmd.OutOrStdout(), report); writeErr != nil {
					return writeErr
				}
			}
			if err != nil {
				return err
			}
			if report == nil || !report.Verified || report.HistoryComplete || report.CutoverReady {
				return fmt.Errorf("audit verify failed")
			}
			return nil
		},
	}
	auditVerifyCmd.Flags().String("staged", "", "staged chatstorage database")
	auditVerifyCmd.Flags().String("source", "", "source snapshot")
	auditVerifyCmd.Flags().String("target", "", "target snapshot")
	auditVerifyCmd.Flags().String("map", "", "account mapping JSON")
	auditVerifyCmd.Flags().String("audit", "", "private audit database")
	auditVerifyCmd.Flags().String("base", "target", "source or target")
	auditVerifyCmd.Flags().Bool("retain-missing", false, "append missing source originals to the private audit")
	dbReuseCmd.AddCommand(auditVerifyCmd)

	legacyCoverageCmd := &cobra.Command{
		Use:   "legacy-coverage",
		Short: "Compare declared legacy history files with a staged GOWA database",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path, _ := cmd.Flags().GetString("db")
			mapping, _ := cmd.Flags().GetString("map")
			report, err := dbreuse.LegacyCoverage(cmd.Context(), dbreuse.LegacyRequest{Staged: path, MapPath: mapping})
			if report != nil {
				if writeErr := writeJSON(cmd.OutOrStdout(), report); writeErr != nil {
					return writeErr
				}
			}
			if err != nil {
				return err
			}
			if report == nil || report.HistoryComplete || report.CutoverReady || report.OldBridgeAssessed {
				return fmt.Errorf("legacy coverage was refused")
			}
			return nil
		},
	}
	legacyCoverageCmd.Flags().String("db", "", "staged GOWA chatstorage database")
	legacyCoverageCmd.Flags().String("map", "", "legacy file to storage JID map")
	dbReuseCmd.AddCommand(legacyCoverageCmd)

	legacyImportCmd := &cobra.Command{
		Use:   "legacy-import",
		Short: "Append missing legacy messages into a staged GOWA database",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path, _ := cmd.Flags().GetString("db")
			mapping, _ := cmd.Flags().GetString("map")
			audit, _ := cmd.Flags().GetString("audit")
			report, err := dbreuse.LegacyImport(cmd.Context(), dbreuse.LegacyRequest{Staged: path, MapPath: mapping, Audit: audit})
			if report != nil {
				if writeErr := writeJSON(cmd.OutOrStdout(), report); writeErr != nil {
					return writeErr
				}
			}
			if err != nil {
				return err
			}
			if report == nil || !report.Applied || report.HistoryComplete || report.CutoverReady || report.OldBridgeAssessed {
				return fmt.Errorf("legacy import was refused")
			}
			return nil
		},
	}
	legacyImportCmd.Flags().String("db", "", "staged GOWA chatstorage database")
	legacyImportCmd.Flags().String("map", "", "legacy file to storage JID map")
	legacyImportCmd.Flags().String("audit", "", "private audit database")
	dbReuseCmd.AddCommand(legacyImportCmd)

	legacySchemaCmd := &cobra.Command{
		Use:   "legacy-schema",
		Short: "Report a SQLite history database's tables without reading rows",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path, _ := cmd.Flags().GetString("db")
			report, err := dbreuse.InspectLegacySchema(path)
			if report != nil {
				if writeErr := writeJSON(cmd.OutOrStdout(), report); writeErr != nil {
					return writeErr
				}
			}
			if err != nil {
				return err
			}
			if report == nil || report.SessionRefused || report.HistoryComplete || report.OldBridgeAssessed || report.ImportFormatDefined {
				return fmt.Errorf("legacy schema inspection was refused")
			}
			return nil
		},
	}
	legacySchemaCmd.Flags().String("db", "", "SQLite history database, such as messages.db")
	dbReuseCmd.AddCommand(legacySchemaCmd)

	coverageCmd.Flags().String("db", "", "chatstorage snapshot")
	coverageCmd.Flags().StringArray("archive", nil, "history-sync JSON file")
	dbReuseCmd.AddCommand(coverageCmd)

	gateCmd := &cobra.Command{
		Use:   "gate",
		Short: "Block real cutover unless schema, revision, and writer checks pass",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path, _ := cmd.Flags().GetString("db")
			revision, _ := cmd.Flags().GetString("expect-revision")
			writers, _ := cmd.Flags().GetString("attest-writers")
			target, _ := cmd.Flags().GetString("target")
			work, _ := cmd.Flags().GetString("work-dir")
			report, err := dbreuse.Gate(cmd.Context(), dbreuse.GateRequest{
				DB: path, ExpectRevision: revision, Writers: splitCSV(writers), Target: target, WorkDir: work,
			})
			if report != nil {
				if writeErr := writeJSON(cmd.OutOrStdout(), report); writeErr != nil {
					return writeErr
				}
			}
			if err != nil {
				return err
			}
			if report == nil || report.RealCutoverBlocked || !report.MechanicalGatesPassed {
				return fmt.Errorf("real cutover is blocked")
			}
			return nil
		},
	}
	gateCmd.Flags().String("db", "", "single-file chatstorage snapshot")
	gateCmd.Flags().String("expect-revision", "", "full 40-character source SHA")
	gateCmd.Flags().String("attest-writers", "", "source-gowa,source-periodic-merge,target-gowa")
	gateCmd.Flags().String("target", "", "target chatstorage.db")
	gateCmd.Flags().String("work-dir", "", "required with --target")
	dbReuseCmd.AddCommand(gateCmd)

	revisionCmd := &cobra.Command{
		Use:   "revision",
		Short: "Print the verified installed Mac revision record",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			rev := dbreuse.InstalledRevision()
			if rev.HistoryComplete || rev.ForkBaseIsInstalled || rev.DatabaseProvesRevision {
				return fmt.Errorf("revision record is inconsistent")
			}
			return writeJSON(cmd.OutOrStdout(), rev)
		},
	}
	dbReuseCmd.AddCommand(revisionCmd)
}

func splitCSV(raw string) []string {
	if raw == "" {
		return nil
	}
	return strings.Split(raw, ",")
}

func writeJSON(w interface{ Write([]byte) (int, error) }, value any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(value)
}
