//go:build !purego

package dbreuse

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/mattn/go-sqlite3"
)

func backupSQLite(ctx context.Context, src *sql.DB, dstPath string) error {
	dst, err := sql.Open("sqlite3", sqliteFileURI(dstPath, false))
	if err != nil {
		return err
	}
	defer dst.Close()
	dst.SetMaxOpenConns(1)
	if _, err := dst.ExecContext(ctx, `PRAGMA journal_mode=DELETE`); err != nil {
		return err
	}

	srcConn, err := src.Conn(ctx)
	if err != nil {
		return err
	}
	defer srcConn.Close()
	dstConn, err := dst.Conn(ctx)
	if err != nil {
		return err
	}
	defer dstConn.Close()

	var source *sqlite3.SQLiteConn
	if err := srcConn.Raw(func(driverConn any) error {
		conn, ok := driverConn.(*sqlite3.SQLiteConn)
		if !ok {
			return fmt.Errorf("sqlite source connection is %T", driverConn)
		}
		source = conn
		return nil
	}); err != nil {
		return err
	}

	return dstConn.Raw(func(driverConn any) error {
		dest, ok := driverConn.(*sqlite3.SQLiteConn)
		if !ok {
			return fmt.Errorf("sqlite destination connection is %T", driverConn)
		}
		backup, err := dest.Backup("main", source, "main")
		if err != nil {
			return err
		}
		defer backup.Finish()
		for i := 0; i < 100000; i++ {
			done, err := backup.Step(1024)
			if err != nil {
				return err
			}
			if done {
				return nil
			}
		}
		return fmt.Errorf("sqlite backup did not finish")
	})
}
