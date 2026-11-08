//go:build purego

package dbreuse

import (
	"context"
	"database/sql"
	"fmt"

	"modernc.org/sqlite"
)

type sqliteBackuper interface {
	NewBackup(dstURI string) (*sqlite.Backup, error)
}

func backupSQLite(ctx context.Context, src *sql.DB, dstPath string) error {
	srcConn, err := src.Conn(ctx)
	if err != nil {
		return err
	}
	defer srcConn.Close()
	return srcConn.Raw(func(driverConn any) error {
		backuper, ok := driverConn.(sqliteBackuper)
		if !ok {
			return fmt.Errorf("sqlite source connection is %T", driverConn)
		}
		backup, err := backuper.NewBackup(sqliteFileURI(dstPath, false))
		if err != nil {
			return err
		}
		defer backup.Finish()
		for i := 0; i < 100000; i++ {
			more, err := backup.Step(-1)
			if err != nil {
				return err
			}
			if !more {
				return nil
			}
		}
		return fmt.Errorf("sqlite backup did not finish")
	})
}
