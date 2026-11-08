package cmd

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/aldinokemal/go-whatsapp-web-multidevice/infrastructure/dbreuse"
	"github.com/spf13/cobra"
)

// dbReuseCmd inspects and snapshots chatstorage without starting WhatsApp.
var dbReuseCmd = &cobra.Command{
	Use:   "db-reuse",
	Short: "Snapshot and check a chatstorage.db before another host opens it",
	Long: `Check whether a GOWA chatstorage.db can be opened by official v9.9.0.

The subcommands do not connect to WhatsApp, send messages, mark chats read,
import history archives, open whatsapp.db, or delete rows. snapshot writes one
consistent database file, including an open WAL. preflight reports schema and
registry alignment. reconcile lists message ids that exist on only one side.
materialize-media copies media bytes and follows symlinks.

Dry-run is the whole command set: nothing here replaces a live database.`,
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
			if err := dbreuse.Snapshot(cmd.Context(), src, dst); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "snapshot %s\n", dst)
			return nil
		},
	}
	snapshotCmd.Flags().String("source", "", "path to chatstorage.db, with its -wal left beside it")
	snapshotCmd.Flags().String("out", "", "new snapshot file to create")
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
}

func writeJSON(w interface{ Write([]byte) (int, error) }, value any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(value)
}
