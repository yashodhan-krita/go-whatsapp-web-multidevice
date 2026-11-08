package dbreuse

import (
	"database/sql"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/aldinokemal/go-whatsapp-web-multidevice/pkg/sqlite"
)

func sqliteFileURI(path string, readonly bool) string {
	u := url.URL{Scheme: "file", Path: filepath.Clean(path)}
	q := url.Values{}
	if readonly {
		q.Set("mode", "ro")
	} else {
		q.Set("mode", "rwc")
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func rejectDatabasePath(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("database path %s is a symlink; pass the real chatstorage.db next to its -wal", path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("database path %s is not a regular file", path)
	}
	if strings.EqualFold(filepath.Base(path), "whatsapp.db") {
		return fmt.Errorf("refusing %s: whatsapp.db holds the WhatsApp session, not chat history", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	header := make([]byte, 16)
	n, err := io.ReadFull(f, header)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return err
	}
	if n < 16 || string(header) != "SQLite format 3\x00" {
		return fmt.Errorf("%s is not a SQLite database", path)
	}
	return nil
}

func openSQLite(path string, readonly bool) (*sql.DB, error) {
	if err := rejectDatabasePath(path); err != nil {
		return nil, err
	}
	db, err := sql.Open(sqlite.DriverName, sqliteFileURI(path, readonly))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if readonly {
		if _, err := db.Exec(`PRAGMA query_only=ON`); err != nil {
			db.Close()
			return nil, fmt.Errorf("set query_only: %w", err)
		}
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func siblingFile(path, suffix string) (size int64, exists bool, err error) {
	sibling := path + suffix
	info, err := os.Lstat(sibling)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, false, nil
		}
		return 0, false, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return 0, true, fmt.Errorf("%s is a symlink; leave the real SQLite sidecar in place", sibling)
	}
	return info.Size(), true, nil
}
