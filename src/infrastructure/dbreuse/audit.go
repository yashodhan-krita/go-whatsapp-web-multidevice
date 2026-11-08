package dbreuse

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/aldinokemal/go-whatsapp-web-multidevice/pkg/sqlite"
)

const sourceAuthoritativePolicy = "source_authoritative_same_identity"

const auditPolicyText = "Same account, chat, and message id: the source row is the staged primary and the other full row is retained in this file. A message id under another chat is a separate row; no LID or phone alias is inferred. Orphan reactions and polls stay in their tables when the schema allows it, without a parent message being invented. Orphan edits stay in this file because message_edits has a foreign key. history_complete is false. Nothing here authorizes a purge."

type auditMessage struct {
	device string
	chat   string
	id     string
	origin string
	role   string
	row    sqlRow
}

type auditOrphan struct {
	table    string
	device   string
	chat     string
	id       string
	origin   string
	row      sqlRow
	inStaged bool
}

func writeMigrationAudit(path string, messages []auditMessage, orphans []auditOrphan) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("audit file already exists: %s", path)
	}
	db, err := sql.Open(sqlite.DriverName, sqliteFileURI(path, false))
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA journal_mode=DELETE`); err != nil {
		return err
	}
	stmts := []string{
		`CREATE TABLE audit_policy (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE message_versions (
			device_id TEXT NOT NULL,
			chat_jid TEXT NOT NULL,
			message_id TEXT NOT NULL,
			origin TEXT NOT NULL,
			role TEXT NOT NULL,
			row_json TEXT NOT NULL
		)`,
		`CREATE TABLE orphan_rows (
			table_name TEXT NOT NULL,
			device_id TEXT NOT NULL,
			chat_jid TEXT NOT NULL,
			message_id TEXT NOT NULL,
			origin TEXT NOT NULL,
			retained_in_staged_db INTEGER NOT NULL,
			parent_invented INTEGER NOT NULL,
			row_json TEXT NOT NULL
		)`,
		`CREATE TABLE archive_messages (
			archive_path TEXT NOT NULL,
			archive_sha256 TEXT NOT NULL,
			device_id TEXT NOT NULL,
			chat_jid TEXT NOT NULL,
			message_id TEXT NOT NULL,
			imported INTEGER NOT NULL,
			reason TEXT NOT NULL,
			raw_json TEXT NOT NULL
		)`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			return err
		}
	}
	if _, err := db.Exec(`INSERT INTO audit_policy (key, value) VALUES ('policy', ?), ('statement', ?), ('history_complete', 'false')`, sourceAuthoritativePolicy, auditPolicyText); err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	for _, message := range messages {
		raw, err := privateRowJSON(message.row)
		if err != nil {
			tx.Rollback()
			return err
		}
		if _, err := tx.Exec(`INSERT INTO message_versions (device_id, chat_jid, message_id, origin, role, row_json) VALUES (?, ?, ?, ?, ?, ?)`,
			message.device, message.chat, message.id, message.origin, message.role, raw); err != nil {
			tx.Rollback()
			return err
		}
	}
	for _, orphan := range orphans {
		raw, err := privateRowJSON(orphan.row)
		if err != nil {
			tx.Rollback()
			return err
		}
		kept := 0
		if orphan.inStaged {
			kept = 1
		}
		if _, err := tx.Exec(`INSERT INTO orphan_rows (table_name, device_id, chat_jid, message_id, origin, retained_in_staged_db, parent_invented, row_json) VALUES (?, ?, ?, ?, ?, ?, 0, ?)`,
			orphan.table, orphan.device, orphan.chat, orphan.id, orphan.origin, kept, raw); err != nil {
			tx.Rollback()
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

func appendArchiveAudit(path, archivePath, sum, device, chat, id, reason string, imported bool, raw string) error {
	db, err := openSQLite(path, false)
	if err != nil {
		return err
	}
	defer db.Close()
	flag := 0
	if imported {
		flag = 1
	}
	_, err = db.Exec(`INSERT INTO archive_messages (archive_path, archive_sha256, device_id, chat_jid, message_id, imported, reason, raw_json) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		archivePath, sum, device, chat, id, flag, reason, raw)
	return err
}

func privateRowJSON(row sqlRow) (string, error) {
	obj := make(map[string]any, len(row.cols))
	for i, col := range row.cols {
		obj[col] = jsonSafe(row.vals[i])
	}
	raw, err := json.Marshal(obj)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func jsonSafe(value any) any {
	switch typed := value.(type) {
	case []byte:
		return base64.StdEncoding.EncodeToString(typed)
	case time.Time:
		return typed.UTC().Format(time.RFC3339Nano)
	default:
		return value
	}
}

func auditPathFor(out string) string {
	return out + ".migration-audit.db"
}

// rowsInAudit reports whether a private audit file contains the given text.
// Tests use it; the overlay report does not copy audit bodies.
func rowsInAudit(path, needle string) (bool, error) {
	db, err := openSQLite(path, true)
	if err != nil {
		return false, err
	}
	defer db.Close()
	var n int
	err = db.QueryRow(`
		SELECT COUNT(*) FROM (
			SELECT row_json AS body FROM message_versions
			UNION ALL
			SELECT row_json FROM orphan_rows
			UNION ALL
			SELECT raw_json FROM archive_messages
		) WHERE body LIKE ?`, "%"+needle+"%").Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}
