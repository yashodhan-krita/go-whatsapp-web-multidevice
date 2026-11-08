package dbreuse

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aldinokemal/go-whatsapp-web-multidevice/infrastructure/chatstorage"
	"github.com/aldinokemal/go-whatsapp-web-multidevice/pkg/sqlite"
)

func newSchemaDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "chatstorage.db")
	db, err := sql.Open(sqlite.DriverName, path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := chatstorage.NewStorageRepository(db).InitializeSchema(); err != nil {
		t.Fatalf("schema: %v", err)
	}
	return db, path
}

func insertSyntheticHistory(t *testing.T, db *sql.DB) {
	t.Helper()
	_, err := db.Exec(`
		INSERT INTO devices (device_id, display_name, jid, ad_jid, webhook_secret)
		VALUES ('synthetic-slot', 'synthetic', '15550001000@s.whatsapp.net', '15550001000:18@s.whatsapp.net', 'synthetic-webhook-secret')`)
	if err != nil {
		t.Fatalf("device: %v", err)
	}
	_, err = db.Exec(`
		INSERT INTO chats (jid, device_id, name, last_message_time)
		VALUES ('15550002000@s.whatsapp.net', '15550001000@s.whatsapp.net', 'synthetic peer', '2026-01-02 03:04:05')`)
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	_, err = db.Exec(`
		INSERT INTO messages (id, chat_jid, device_id, sender, content, timestamp, is_from_me)
		VALUES ('synthetic-m1', '15550002000@s.whatsapp.net', '15550001000@s.whatsapp.net', '15550002000@s.whatsapp.net', 'synthetic hello', '2026-01-02 03:04:05', 0)`)
	if err != nil {
		t.Fatalf("message: %v", err)
	}
	_, err = db.Exec(`
		INSERT INTO message_reactions (message_id, chat_jid, device_id, reactor_jid, emoji, reaction_timestamp)
		VALUES ('synthetic-m1', '15550002000@s.whatsapp.net', '15550001000@s.whatsapp.net', '15550002000@s.whatsapp.net', 'ok', '2026-01-02 03:06:00')`)
	if err != nil {
		t.Fatalf("reaction: %v", err)
	}
	_, err = db.Exec(`
		INSERT INTO message_edits (original_message_id, edit_event_id, chat_jid, device_id, edited_at, previous_content, new_content)
		VALUES ('synthetic-m1', 'synthetic-e1', '15550002000@s.whatsapp.net', '15550001000@s.whatsapp.net', '2026-01-02 03:05:00', 'synthetic hello', 'synthetic hello')`)
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	_, err = db.Exec(`
		INSERT INTO poll_definitions (device_id, chat_jid, poll_message_id, question)
		VALUES ('15550001000@s.whatsapp.net', '15550002000@s.whatsapp.net', 'synthetic-m1', 'synthetic?')`)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	_, err = db.Exec(`
		INSERT INTO chatwoot_device_configs (device_id, device_jid, api_token, webhook_secret)
		VALUES ('synthetic-slot', '15550001000@s.whatsapp.net', 'synthetic-chatwoot-token', 'synthetic-chatwoot-webhook')`)
	if err != nil {
		t.Fatalf("chatwoot: %v", err)
	}
	_, err = db.Exec(`
		INSERT INTO scheduled_sends (id, device_id, message_type, payload_json, scheduled_at, next_run_at, timezone, attempts, status)
		VALUES
			('synthetic-idle', 'synthetic-slot', 'text', '{"synthetic":true}', '2026-12-01 00:00:00', '2026-12-01 00:00:00', 'UTC', 0, 'active'),
			('synthetic-tried', 'synthetic-slot', 'text', '{"synthetic":true}', '2026-12-01 00:00:00', '2026-12-01 00:00:00', 'UTC', 2, 'active')`)
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
}

// applyOfficialV990Tail runs the only migrations added after this fork.
// They are the v9.9.0 getMigrations entries 51-54.
func applyOfficialV990Tail(t *testing.T, db *sql.DB) {
	t.Helper()
	statements := []string{
		`ALTER TABLE scheduled_sends ADD COLUMN pending_message_id VARCHAR(255) NOT NULL DEFAULT ''`,
		`ALTER TABLE scheduled_sends ADD COLUMN legacy_attempt_unknown BOOLEAN NOT NULL DEFAULT FALSE`,
		`UPDATE scheduled_sends SET legacy_attempt_unknown = TRUE WHERE status = 'running' OR attempts > 0`,
		`ALTER TABLE scheduled_sends ADD COLUMN pending_poll_secret BLOB`,
	}
	for i, statement := range statements {
		tx, err := db.Begin()
		if err != nil {
			t.Fatalf("begin %d: %v", i, err)
		}
		if _, err := tx.Exec(statement); err != nil {
			tx.Rollback()
			t.Fatalf("migration %d: %v", 51+i, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_info (version) VALUES (?)`, 51+i); err != nil {
			tx.Rollback()
			t.Fatalf("version %d: %v", 51+i, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit %d: %v", 51+i, err)
		}
	}
}

func historyFingerprint(t *testing.T, db *sql.DB) string {
	t.Helper()
	rows, err := db.Query(`
		SELECT 'm', id, chat_jid, device_id, content FROM messages
		UNION ALL SELECT 'c', jid, device_id, name, '' FROM chats
		UNION ALL SELECT 'r', message_id, chat_jid, device_id, emoji FROM message_reactions
		UNION ALL SELECT 'e', original_message_id, edit_event_id, device_id, new_content FROM message_edits
		UNION ALL SELECT 'p', poll_message_id, chat_jid, device_id, question FROM poll_definitions
		UNION ALL SELECT 'd', device_id, jid, ad_jid, '' FROM devices
		ORDER BY 1, 2, 3, 4`)
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	defer rows.Close()
	h := sha256.New()
	for rows.Next() {
		var a, b, c, d, e string
		if err := rows.Scan(&a, &b, &c, &d, &e); err != nil {
			t.Fatalf("scan: %v", err)
		}
		h.Write([]byte(a + "\x00" + b + "\x00" + c + "\x00" + d + "\x00" + e + "\n"))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return string(h.Sum(nil))
}

func TestOfficialV990UpgradePreservesHistory(t *testing.T) {
	db, path := newSchemaDB(t)
	insertSyntheticHistory(t, db)
	before := historyFingerprint(t, db)

	applyOfficialV990Tail(t, db)

	if got := historyFingerprint(t, db); got != before {
		t.Fatal("v9.9.0 schedule migrations changed history or registry rows")
	}
	var idle, tried bool
	if err := db.QueryRow(`SELECT legacy_attempt_unknown FROM scheduled_sends WHERE id='synthetic-idle'`).Scan(&idle); err != nil {
		t.Fatalf("idle schedule: %v", err)
	}
	if err := db.QueryRow(`SELECT legacy_attempt_unknown FROM scheduled_sends WHERE id='synthetic-tried'`).Scan(&tried); err != nil {
		t.Fatalf("tried schedule: %v", err)
	}
	if idle || !tried {
		t.Fatalf("migration 53 flags idle=%t tried=%t", idle, tried)
	}

	db.Close()
	report, err := Check(path)
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if !report.Reusable || report.Mode != "unchanged" || report.SchemaVersion != 54 {
		t.Fatalf("schema 54 report %+v", report)
	}
	if report.HistoryComplete {
		t.Fatal("preflight claimed complete history")
	}
	encoded, _ := json.Marshal(report)
	for _, secret := range []string{"synthetic-webhook-secret", "synthetic-chatwoot-token"} {
		if contains(encoded, secret) {
			t.Fatalf("preflight JSON contains %s", secret)
		}
	}
}

func TestSchema50IsReusableByOfficialUpgrade(t *testing.T) {
	db, path := newSchemaDB(t)
	insertSyntheticHistory(t, db)
	db.Close()

	report, err := Check(path)
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if !report.Reusable || report.Mode != "official-v9.9.0-upgrade" || report.SchemaVersion != 50 {
		t.Fatalf("schema 50 report %+v", report)
	}
	if report.MessageCount != 1 || len(report.OrphanHistoryDeviceIDs) != 0 {
		t.Fatalf("coverage %+v", report)
	}
	if len(report.Registry) != 1 || report.Registry[0].StorageID != "15550001000@s.whatsapp.net" {
		t.Fatalf("registry %+v", report.Registry)
	}
}

func TestPreflightRejectsUnknownSchemaOrphanAndSessionDB(t *testing.T) {
	db, path := newSchemaDB(t)
	insertSyntheticHistory(t, db)
	if _, err := db.Exec(`ALTER TABLE messages ADD COLUMN mac_extra TEXT`); err != nil {
		t.Fatalf("alter: %v", err)
	}
	db.Close()
	report, err := Check(path)
	if err != nil {
		t.Fatalf("check extra column: %v", err)
	}
	if report.Reusable {
		t.Fatal("extra column was accepted")
	}

	db2, path2 := newSchemaDB(t)
	if _, err := db2.Exec(`INSERT INTO messages (id, chat_jid, device_id, sender, content, timestamp) VALUES ('synthetic-orphan', '15550002000@s.whatsapp.net', '15550009999@s.whatsapp.net', '15550002000@s.whatsapp.net', 'orphan', '2026-01-03 00:00:00')`); err != nil {
		t.Fatalf("orphan insert: %v", err)
	}
	db2.Close()
	report, err = Check(path2)
	if err != nil {
		t.Fatalf("check orphan: %v", err)
	}
	if report.Reusable || len(report.OrphanHistoryDeviceIDs) != 1 {
		t.Fatalf("orphan report %+v", report)
	}

	session := filepath.Join(t.TempDir(), "whatsapp.db")
	raw, err := os.ReadFile(path2)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(session, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Check(session); err == nil {
		t.Fatal("whatsapp.db was opened")
	}

	link := filepath.Join(t.TempDir(), "linked.db")
	if err := os.Symlink(path2, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Check(link); err == nil {
		t.Fatal("symlink database was accepted")
	}
}

func TestSnapshotIncludesUncheckpointedWAL(t *testing.T) {
	db, path := newSchemaDB(t)
	db.Close()
	writer, err := sql.Open(sqlite.DriverName, sqlite.FormatChatStorageURI("file:"+path, true, false))
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err := writer.Exec(`PRAGMA wal_autocheckpoint=0`); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Exec(`INSERT INTO messages (id, chat_jid, device_id, sender, content, timestamp) VALUES ('synthetic-wal', '15550002000@s.whatsapp.net', '15550001000@s.whatsapp.net', '15550002000@s.whatsapp.net', 'only in wal', '2026-02-02 00:00:00')`); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Exec(`INSERT INTO devices (device_id, jid, ad_jid) VALUES ('synthetic-slot', '15550001000@s.whatsapp.net', '15550001000:4@s.whatsapp.net')`); err != nil {
		t.Fatal(err)
	}
	walInfo, err := os.Stat(path + "-wal")
	if err != nil || walInfo.Size() == 0 {
		t.Fatalf("expected a WAL sidecar, stat=%v err=%v", walInfo, err)
	}
	beforeDB, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	beforeWAL, err := os.ReadFile(path + "-wal")
	if err != nil {
		t.Fatal(err)
	}

	fragment := filepath.Join(t.TempDir(), "fragment.db")
	if err := os.WriteFile(fragment, beforeDB, 0o600); err != nil {
		t.Fatal(err)
	}
	fragDB, err := sql.Open(sqlite.DriverName, fragment)
	if err != nil {
		t.Fatal(err)
	}
	var fragCount int
	fragErr := fragDB.QueryRow(`SELECT COUNT(*) FROM messages WHERE id='synthetic-wal'`).Scan(&fragCount)
	fragDB.Close()
	if fragErr == nil && fragCount != 0 {
		t.Fatal("copying the main file without the WAL already contained the new row; the fixture did not keep it uncheckpointed")
	}

	dst := filepath.Join(t.TempDir(), "snapshot.db")
	if err := Snapshot(context.Background(), path, dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dst + "-wal"); !os.IsNotExist(err) {
		t.Fatalf("snapshot created a wal sidecar: %v", err)
	}
	snap, err := sql.Open(sqlite.DriverName, dst)
	if err != nil {
		t.Fatal(err)
	}
	defer snap.Close()
	var content string
	if err := snap.QueryRow(`SELECT content FROM messages WHERE id='synthetic-wal'`).Scan(&content); err != nil {
		t.Fatalf("snapshot missing WAL row: %v", err)
	}
	if content != "only in wal" {
		t.Fatalf("content %q", content)
	}
	afterDB, _ := os.ReadFile(path)
	afterWAL, _ := os.ReadFile(path + "-wal")
	if sha256.Sum256(afterDB) != sha256.Sum256(beforeDB) || sha256.Sum256(afterWAL) != sha256.Sum256(beforeWAL) {
		t.Fatal("snapshot modified the source database or WAL")
	}

	report, err := Check(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Reusable || report.MessageCount != 1 {
		t.Fatalf("snapshot preflight %+v", report)
	}
}

func TestReconcileKeepsTargetOnlyRowsInTheTargetFile(t *testing.T) {
	srcDB, srcPath := newSchemaDB(t)
	insertSyntheticHistory(t, srcDB)
	srcDB.Close()
	dstDB, dstPath := newSchemaDB(t)
	insertSyntheticHistory(t, dstDB)
	if _, err := dstDB.Exec(`INSERT INTO messages (id, chat_jid, device_id, sender, content, timestamp) VALUES ('synthetic-target-only', '15550002000@s.whatsapp.net', '15550001000@s.whatsapp.net', '15550002000@s.whatsapp.net', 'newer on target', '2026-03-03 00:00:00')`); err != nil {
		t.Fatal(err)
	}
	dstDB.Close()
	before, err := os.ReadFile(dstPath)
	if err != nil {
		t.Fatal(err)
	}

	work := filepath.Join(t.TempDir(), "work")
	first, err := Reconcile(context.Background(), srcPath, dstPath, work)
	if err != nil {
		t.Fatal(err)
	}
	if first.HistoryComplete {
		t.Fatal("reconcile claimed complete history")
	}
	if len(first.TargetOnly) != 1 || first.TargetOnly[0].ID != "synthetic-target-only" {
		t.Fatalf("target-only %+v", first.TargetOnly)
	}
	if len(first.SourceOnly) != 0 || first.SameCount != 1 {
		t.Fatalf("report %+v", first)
	}
	if len(first.Chats) != 1 || first.Chats[0].TargetCount != 2 || first.Chats[0].SourceCount != 1 {
		t.Fatalf("chats %+v", first.Chats)
	}
	second, err := Reconcile(context.Background(), srcPath, dstPath, work)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.TargetOnly) != 1 || second.SameCount != first.SameCount {
		t.Fatal("reconcile rerun changed the diff")
	}
	after, err := os.ReadFile(dstPath)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(after) != sha256.Sum256(before) {
		t.Fatal("reconcile modified the target database")
	}
	snap, err := sql.Open(sqlite.DriverName, first.TargetSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	defer snap.Close()
	var kept string
	if err := snap.QueryRow(`SELECT content FROM messages WHERE id='synthetic-target-only'`).Scan(&kept); err != nil {
		t.Fatalf("target snapshot lost the target-only row: %v", err)
	}
	if kept != "newer on target" {
		t.Fatalf("kept %q", kept)
	}
}

func TestMaterializeSymlinkAndMissingMedia(t *testing.T) {
	root := t.TempDir()
	bridge := filepath.Join(root, "bridge")
	if err := os.Mkdir(bridge, 0o700); err != nil {
		t.Fatal(err)
	}
	payload := []byte("synthetic-bridge-media")
	if err := os.WriteFile(filepath.Join(bridge, "note.bin"), payload, 0o600); err != nil {
		t.Fatal(err)
	}
	media := filepath.Join(root, "media")
	if err := os.Mkdir(media, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(bridge, filepath.Join(media, "_mac")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "missing-target"), filepath.Join(media, "dangling")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(media, "local.txt"), []byte("synthetic-local"), 0o600); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(root, "dest")
	manifest, err := MaterializeMedia(media, dest)
	if !errors.Is(err, ErrIncompleteMedia) {
		t.Fatalf("err=%v manifest=%+v", err, manifest)
	}
	if manifest.Complete || manifest.Missing != 1 || manifest.Copied != 2 {
		t.Fatalf("manifest %+v", manifest)
	}
	copied := filepath.Join(dest, "_mac", "note.bin")
	info, err := os.Lstat(copied)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("materialized file is still a symlink")
	}
	got, err := os.ReadFile(copied)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Fatalf("bytes %q", got)
	}
	var linked bool
	for _, file := range manifest.Files {
		if file.LogicalPath == "_mac/note.bin" {
			linked = file.Symlink && file.SHA256 != ""
		}
	}
	if !linked {
		t.Fatalf("files %+v", manifest.Files)
	}

	again, err := MaterializeMedia(media, dest)
	if !errors.Is(err, ErrIncompleteMedia) {
		t.Fatalf("rerun err=%v", err)
	}
	if again.Copied != 0 || again.Present != 2 {
		t.Fatalf("rerun %+v", again)
	}

	if err := os.WriteFile(filepath.Join(dest, "local.txt"), []byte("different"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = MaterializeMedia(media, dest)
	if !errors.Is(err, ErrMediaConflict) {
		t.Fatalf("conflict err=%v", err)
	}
	got, err = os.ReadFile(filepath.Join(dest, "local.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "different" {
		t.Fatal("conflict overwrote destination bytes")
	}
}

func contains(b []byte, s string) bool {
	return strings.Contains(string(b), s)
}
