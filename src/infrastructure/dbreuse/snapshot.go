package dbreuse

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SnapshotOptions controls whether a copy may be taken while a writer is
// connected. Snapshot always leaves CutoverReady false; schema and identity
// are decided by Gate.
type SnapshotOptions struct {
	AllowLiveValidation bool
	AttestWriters       []string
}

// SnapshotResult describes a new snapshot file. CutoverReady is always false.
type SnapshotResult struct {
	Path            string   `json:"path"`
	SourceWALBytes  int64    `json:"source_wal_bytes"`
	SourceSHM       bool     `json:"source_shm"`
	WritersAttested []string `json:"writers_attested"`
	LiveValidation  bool     `json:"live_validation"`
	LegacySchema    bool     `json:"legacy_schema"`
	CutoverReady    bool     `json:"cutover_ready"`
	HistoryComplete bool     `json:"history_complete"`
	Reasons         []string `json:"reasons"`
}

// Snapshot copies src into dst as one consistent SQLite file.
// It is the comparison copy used by reconcile and allows a live -shm.
// A cutover copy must use SnapshotWithOptions after the writers are frozen.
// The SQLite backup API reads committed pages, including an uncheckpointed
// WAL, and does not rewrite src. dst must not already exist.
func Snapshot(ctx context.Context, src, dst string) error {
	return snapshot(ctx, src, dst)
}

// SnapshotWithOptions refuses a connected writer unless live validation is
// requested, and refuses a freeze attestation that disagrees with -shm.
func SnapshotWithOptions(ctx context.Context, src, dst string, opt SnapshotOptions) (*SnapshotResult, error) {
	writers, err := normalizeWriters(opt.AttestWriters)
	if err != nil {
		return nil, err
	}
	walBytes, _, err := siblingFile(src, "-wal")
	if err != nil {
		return nil, err
	}
	_, shm, err := siblingFile(src, "-shm")
	if err != nil {
		return nil, err
	}
	result := &SnapshotResult{
		SourceWALBytes:  walBytes,
		SourceSHM:       shm,
		WritersAttested: writers,
		HistoryComplete: false,
		CutoverReady:    false,
		Reasons:         []string{},
	}
	if opt.AllowLiveValidation && len(writers) > 0 {
		return result, fmt.Errorf("allow-live-validation cannot be combined with a freeze attestation")
	}
	if shm && len(writers) > 0 {
		return result, fmt.Errorf("refusing a freeze attestation while %s-shm exists; a writer is still connected", src)
	}
	legacy, err := legacySQLite(src)
	if err != nil {
		return result, err
	}
	if legacy {
		if shm && !opt.AllowLiveValidation {
			return result, fmt.Errorf("source -shm exists; pass allow-live-validation for a non-cutover copy")
		}
		if err := snapshot(ctx, src, dst); err != nil {
			return result, err
		}
		result.Path = dst
		result.LegacySchema = true
		result.LiveValidation = opt.AllowLiveValidation
		result.Reasons = append(result.Reasons, "legacy snapshot is a consistent copy of a non-chatstorage SQLite file. It exports no rows, it is not a GOWA cutover file, and it does not prove old-bridge coverage.")
		result.CutoverReady = false
		result.HistoryComplete = false
		return result, nil
	}
	if shm && !opt.AllowLiveValidation {
		return result, fmt.Errorf("source -shm exists; stop source GOWA, the periodic merge writer, and target GOWA before a cutover snapshot, or pass allow-live-validation for a non-cutover copy")
	}
	missing := missingWriters(writers)
	if !opt.AllowLiveValidation && len(missing) > 0 {
		return result, fmt.Errorf("a cutover snapshot requires attested quiesced writers: %s", strings.Join(missing, ", "))
	}
	if opt.AllowLiveValidation {
		result.LiveValidation = true
		result.Reasons = append(result.Reasons, "live validation snapshot; writers were not frozen and this file is not a cutover artifact")
	}
	if err := snapshot(ctx, src, dst); err != nil {
		return result, err
	}
	result.Path = dst
	result.Reasons = append(result.Reasons, "snapshot does not check schema, account identity, or history completeness")
	result.CutoverReady = false
	result.HistoryComplete = false
	return result, nil
}

func snapshot(ctx context.Context, src, dst string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	src, err := filepath.Abs(src)
	if err != nil {
		return err
	}
	dst, err = filepath.Abs(dst)
	if err != nil {
		return err
	}
	if src == dst {
		return fmt.Errorf("snapshot source and destination are the same file")
	}
	if _, err := os.Lstat(dst); err == nil {
		return fmt.Errorf("snapshot destination already exists: %s", dst)
	} else if !os.IsNotExist(err) {
		return err
	}
	if _, _, err := siblingFile(src, "-wal"); err != nil {
		return err
	}
	if _, _, err := siblingFile(src, "-shm"); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	source, err := openSQLite(src, true)
	if err != nil {
		return err
	}
	defer source.Close()
	if err := backupSQLite(ctx, source, dst); err != nil {
		os.Remove(dst)
		return err
	}
	if _, err := os.Lstat(dst + "-wal"); err == nil {
		os.Remove(dst)
		os.Remove(dst + "-wal")
		return fmt.Errorf("snapshot %s left a WAL sidecar; refusing a fragmented copy", dst)
	}
	return nil
}
