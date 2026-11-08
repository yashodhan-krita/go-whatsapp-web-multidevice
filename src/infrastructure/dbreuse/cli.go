package dbreuse

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

const offlineUsage = `db-reuse snapshots and checks a GOWA chatstorage database.

This binary does not connect to WhatsApp, send messages, or open whatsapp.db.
It has no rest command. history_complete is false in every report.

  revision
  snapshot --source DB --out FILE [--allow-live-validation | --attest-writers LIST]
  preflight --db FILE
  reconcile --source DB --target DB --work-dir DIR [--out REPORT]
  materialize-media --source DIR --dest DIR [--out REPORT]
  media-refs --db FILE [--archive FILE ...] [--out REPORT]
  overlay --source DB --target DB --map FILE --work-dir DIR --out FILE [--base source|target] [--allow-live-validation]
  coverage --db FILE --archive FILE [--archive FILE ...] [--out REPORT]
  import-archive --db FILE --archive FILE [--archive FILE ...] [--account JID] [--audit FILE]
  audit-verify --staged DB --source DB --target DB --map FILE [--audit FILE] [--base source|target]
  legacy-schema --db FILE
  gate --db FILE --expect-revision SHA --attest-writers LIST [--target DB --work-dir DIR]

--attest-writers must be exactly:
  source-gowa,source-periodic-merge,target-gowa

gate exits non-zero even when the mechanical checks pass. Standing limits
(unknown DB_KEYS_URI, unread archives, unread session, history_complete false)
keep real cutover blocked.
`

// RunOffline runs the db-reuse helper and returns its exit code.
func RunOffline(args []string) int {
	return runOffline(os.Stdout, os.Stderr, args)
}

func runOffline(stdout, stderr io.Writer, args []string) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(stdout, offlineUsage)
		return 0
	}
	switch args[0] {
	case "revision":
		return finish(stdout, stderr, InstalledRevision(), nil, false)
	case "snapshot":
		return cmdSnapshot(stdout, stderr, args[1:])
	case "preflight":
		return cmdPreflight(stdout, stderr, args[1:])
	case "reconcile":
		return cmdReconcile(stdout, stderr, args[1:])
	case "materialize-media":
		return cmdMedia(stdout, stderr, args[1:])
	case "media-refs":
		return cmdMediaRefs(stdout, stderr, args[1:])
	case "overlay":
		return cmdOverlay(stdout, stderr, args[1:])
	case "coverage":
		return cmdCoverage(stdout, stderr, args[1:])
	case "import-archive":
		return cmdImport(stdout, stderr, args[1:])
	case "audit-verify":
		return cmdAuditVerify(stdout, stderr, args[1:])
	case "legacy-schema":
		return cmdLegacySchema(stdout, stderr, args[1:])
	case "gate":
		return cmdGate(stdout, stderr, args[1:])
	default:
		fmt.Fprintf(stderr, "unknown command %q\n%s", args[0], offlineUsage)
		return 2
	}
}

func cmdSnapshot(stdout, stderr io.Writer, args []string) int {
	fs := newFlagSet(stderr, "snapshot")
	source := fs.String("source", "", "chatstorage.db, with its -wal left beside it")
	out := fs.String("out", "", "new snapshot file")
	live := fs.Bool("allow-live-validation", false, "copy while -shm exists; the result is not a cutover file")
	writers := fs.String("attest-writers", "", "comma-separated quiesced writers")
	if !parseSet(fs, args, stderr) {
		return 2
	}
	if *source == "" || *out == "" {
		fmt.Fprintln(stderr, "snapshot requires --source and --out")
		return 2
	}
	result, err := SnapshotWithOptions(context.Background(), *source, *out, SnapshotOptions{
		AllowLiveValidation: *live,
		AttestWriters:       splitComma(*writers),
	})
	return finish(stdout, stderr, result, err, result != nil && !result.CutoverReady && err == nil && false)
}

func cmdPreflight(stdout, stderr io.Writer, args []string) int {
	fs := newFlagSet(stderr, "preflight")
	path := fs.String("db", "", "chatstorage.db")
	if !parseSet(fs, args, stderr) {
		return 2
	}
	if *path == "" {
		fmt.Fprintln(stderr, "preflight requires --db")
		return 2
	}
	report, err := Check(*path)
	blocked := report == nil || !report.Reusable || report.HistoryComplete
	return finish(stdout, stderr, report, err, blocked)
}

func cmdReconcile(stdout, stderr io.Writer, args []string) int {
	fs := newFlagSet(stderr, "reconcile")
	source := fs.String("source", "", "source chatstorage.db")
	target := fs.String("target", "", "target chatstorage.db")
	work := fs.String("work-dir", "", "directory for the two snapshots")
	out := fs.String("out", "", "optional JSON report path")
	if !parseSet(fs, args, stderr) {
		return 2
	}
	if *source == "" || *target == "" || *work == "" {
		fmt.Fprintln(stderr, "reconcile requires --source, --target, and --work-dir")
		return 2
	}
	report, err := Reconcile(context.Background(), *source, *target, *work)
	if report != nil && *out != "" {
		if writeErr := writeReportFile(*out, report); writeErr != nil && err == nil {
			err = writeErr
		}
	}
	blocked := report != nil && report.HistoryComplete
	return finish(stdout, stderr, report, err, blocked)
}

func cmdMedia(stdout, stderr io.Writer, args []string) int {
	fs := newFlagSet(stderr, "materialize-media")
	source := fs.String("source", "", "media root")
	dest := fs.String("dest", "", "destination directory")
	out := fs.String("out", "", "optional JSON manifest path")
	if !parseSet(fs, args, stderr) {
		return 2
	}
	if *source == "" || *dest == "" {
		fmt.Fprintln(stderr, "materialize-media requires --source and --dest")
		return 2
	}
	manifest, err := MaterializeMedia(*source, *dest)
	if manifest != nil && *out != "" {
		if writeErr := writeReportFile(*out, manifest); writeErr != nil && err == nil {
			err = writeErr
		}
	}
	return finish(stdout, stderr, manifest, err, false)
}

func cmdMediaRefs(stdout, stderr io.Writer, args []string) int {
	fs := newFlagSet(stderr, "media-refs")
	path := fs.String("db", "", "chatstorage snapshot")
	out := fs.String("out", "", "optional JSON report path")
	root := fs.String("media-root", "", "refused; display names are not matched to a tree")
	var archives stringList
	fs.Var(&archives, "archive", "history-sync JSON file; repeat for each archive")
	if !parseSet(fs, args, stderr) {
		return 2
	}
	report, err := MediaReferences(MediaRefRequest{DB: *path, Archives: archives, MediaRoot: *root})
	if report != nil && *out != "" && err == nil {
		if writeErr := writeReportFile(*out, report); writeErr != nil {
			err = writeErr
		}
	}
	blocked := report == nil || report.SubsetProven || report.WholeTreeCopyAllowed || report.PurgeAllowed || report.HistoryComplete
	return finish(stdout, stderr, report, err, blocked || err != nil)
}

func cmdOverlay(stdout, stderr io.Writer, args []string) int {
	fs := newFlagSet(stderr, "overlay")
	source := fs.String("source", "", "source chatstorage.db")
	target := fs.String("target", "", "target chatstorage.db")
	mapping := fs.String("map", "", "account mapping JSON")
	work := fs.String("work-dir", "", "directory for read-only snapshots")
	out := fs.String("out", "", "new staged database")
	base := fs.String("base", "source", "source keeps the source registry; target keeps the target registry")
	live := fs.Bool("allow-live-validation", false, "allow -shm on the inputs; the output is not a cutover file")
	if !parseSet(fs, args, stderr) {
		return 2
	}
	report, err := Overlay(context.Background(), OverlayRequest{
		Source: *source, Target: *target, MapPath: *mapping, WorkDir: *work, Out: *out, AllowLiveValidation: *live, Base: *base,
	})
	blocked := err != nil || report == nil || !report.Applied || report.HistoryComplete || report.CutoverReady
	return finish(stdout, stderr, report, err, blocked)
}

func cmdCoverage(stdout, stderr io.Writer, args []string) int {
	fs := newFlagSet(stderr, "coverage")
	path := fs.String("db", "", "chatstorage snapshot")
	out := fs.String("out", "", "optional JSON report path")
	var archives stringList
	fs.Var(&archives, "archive", "history-sync JSON file; repeat for each archive")
	if !parseSet(fs, args, stderr) {
		return 2
	}
	if *path == "" {
		fmt.Fprintln(stderr, "coverage requires --db")
		return 2
	}
	report, err := Coverage(*path, archives)
	if report != nil && *out != "" {
		if writeErr := writeReportFile(*out, report); writeErr != nil && err == nil {
			err = writeErr
		}
	}
	blocked := report == nil || report.HistoryComplete
	return finish(stdout, stderr, report, err, blocked || err != nil)
}

func cmdImport(stdout, stderr io.Writer, args []string) int {
	fs := newFlagSet(stderr, "import-archive")
	path := fs.String("db", "", "staged chatstorage snapshot to update")
	account := fs.String("account", "", "registry storage JID; required when the filename has none")
	audit := fs.String("audit", "", "private audit database; default is the snapshot path plus .migration-audit.db")
	var archives stringList
	fs.Var(&archives, "archive", "history-sync JSON file; repeat for each archive")
	if !parseSet(fs, args, stderr) {
		return 2
	}
	report, err := ImportArchives(ImportRequest{DB: *path, Account: *account, Audit: *audit, Archives: archives})
	blocked := report == nil || report.HistoryComplete || report.OldBridgeAssessed || report.MetadataOnlyIsProof
	return finish(stdout, stderr, report, err, blocked || err != nil)
}

func cmdAuditVerify(stdout, stderr io.Writer, args []string) int {
	fs := newFlagSet(stderr, "audit-verify")
	staged := fs.String("staged", "", "staged chatstorage database")
	source := fs.String("source", "", "source snapshot")
	target := fs.String("target", "", "target snapshot")
	mapping := fs.String("map", "", "account mapping JSON")
	audit := fs.String("audit", "", "private audit database; default is the staged path plus .migration-audit.db")
	base := fs.String("base", "target", "source or target, matching the overlay that wrote the staged file")
	if !parseSet(fs, args, stderr) {
		return 2
	}
	report, err := AuditVerify(context.Background(), AuditVerifyRequest{
		Staged: *staged, Source: *source, Target: *target, MapPath: *mapping, Audit: *audit, Base: *base,
	})
	blocked := report == nil || !report.Verified || report.HistoryComplete || report.CutoverReady
	return finish(stdout, stderr, report, err, blocked || err != nil)
}

func cmdLegacySchema(stdout, stderr io.Writer, args []string) int {
	fs := newFlagSet(stderr, "legacy-schema")
	path := fs.String("db", "", "SQLite history database, such as messages.db")
	if !parseSet(fs, args, stderr) {
		return 2
	}
	report, err := InspectLegacySchema(*path)
	blocked := report == nil || report.SessionRefused || report.HistoryComplete || report.OldBridgeAssessed || report.ImportFormatDefined
	return finish(stdout, stderr, report, err, blocked || err != nil)
}

func cmdGate(stdout, stderr io.Writer, args []string) int {
	fs := newFlagSet(stderr, "gate")
	path := fs.String("db", "", "single-file chatstorage snapshot")
	revision := fs.String("expect-revision", "", "full 40-character source SHA")
	writers := fs.String("attest-writers", "", "comma-separated quiesced writers")
	target := fs.String("target", "", "optional target chatstorage.db")
	work := fs.String("work-dir", "", "required with --target")
	if !parseSet(fs, args, stderr) {
		return 2
	}
	if *path == "" || *revision == "" {
		fmt.Fprintln(stderr, "gate requires --db and --expect-revision")
		return 2
	}
	report, err := Gate(context.Background(), GateRequest{
		DB: *path, ExpectRevision: *revision, Writers: splitComma(*writers), Target: *target, WorkDir: *work,
	})
	blocked := report == nil || report.RealCutoverBlocked || report.HistoryComplete || (report != nil && !report.MechanicalGatesPassed)
	if report != nil && report.MechanicalGatesPassed && report.RealCutoverBlocked {
		blocked = true
	}
	return finish(stdout, stderr, report, err, blocked)
}

func finish(stdout, stderr io.Writer, report any, err error, blocked bool) int {
	if report != nil {
		if writeErr := writeJSON(stdout, report); writeErr != nil {
			fmt.Fprintln(stderr, writeErr)
			return 1
		}
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if blocked {
		return 1
	}
	return 0
}

func writeJSON(w io.Writer, value any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(value)
}

func writeReportFile(path string, value any) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	writeErr := writeJSON(f, value)
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}

func newFlagSet(stderr io.Writer, name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

func parseSet(fs *flag.FlagSet, args []string, stderr io.Writer) bool {
	if err := fs.Parse(args); err != nil {
		if err != flag.ErrHelp {
			fmt.Fprintln(stderr, err)
		}
		return false
	}
	return true
}

func splitComma(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	return strings.Split(raw, ",")
}

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(value string) error {
	*s = append(*s, value)
	return nil
}
