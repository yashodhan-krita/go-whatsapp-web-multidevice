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
	base := strings.ToLower(filepath.Base(path))
	switch base {
	case "whatsapp.db", "session.db", "keys.db":
		return fmt.Errorf("refusing %s: this filename is a WhatsApp session or key database, not a history snapshot", path)
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
	if err := rejectSessionTables(db, path); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func rejectSessionTables(db *sql.DB, path string) error {
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table'`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		if sessionOrKeyTable(name) {
			return fmt.Errorf("refusing %s: database contains session or key table %s", path, name)
		}
	}
	return rows.Err()
}

func sessionOrKeyTable(name string) bool {
	switch strings.ToLower(name) {
	case "whatsmeow_device", "whatsmeow_identity_keys", "whatsmeow_pre_keys", "whatsmeow_sessions",
		"whatsmeow_sender_keys", "whatsmeow_app_state_sync_keys", "whatsmeow_app_state_version",
		"whatsmeow_app_state_mutation_macs", "whatsmeow_privacy_tokens", "whatsmeow_lid_map",
		"whatsmeow_message_secrets", "whatsmeow_event_buffer", "identity_keys", "prekeys",
		"signed_prekeys", "sender_keys", "message_secrets", "app_state_sync_keys":
		return true
	default:
		return strings.HasPrefix(strings.ToLower(name), "whatsmeow_")
	}
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
