package dbreuse

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// Snapshot copies src into dst as one consistent SQLite file.
// The SQLite backup API reads committed pages, including an uncheckpointed
// WAL, and does not rewrite src. dst must not already exist.
func Snapshot(ctx context.Context, src, dst string) error {
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
