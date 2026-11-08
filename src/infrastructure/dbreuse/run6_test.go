package dbreuse

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aldinokemal/go-whatsapp-web-multidevice/pkg/sqlite"
)

func TestValueRelationTreatsEncodingAsNormalization(t *testing.T) {
	stamp := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	cases := []struct {
		column string
		left   any
		right  any
		want   string
	}{
		{"is_from_me", false, int64(0), "type_normalization"},
		{"is_from_me", true, int64(1), "type_normalization"},
		{"timestamp", "2026-01-02 03:04:05", stamp, "timestamp_normalization"},
		{"timestamp", "2026-01-02 03:04:05", "2026-01-02T08:34:05+05:30", "timestamp_normalization"},
		{"content", nil, "", "empty_normalization"},
		{"file_length", nil, int64(0), "numeric_default"},
		{"content", "synthetic hello", "other", "different"},
		{"timestamp", "2026-01-02 03:04:05", "2026-01-02 04:04:05", "different"},
	}
	for _, tc := range cases {
		got, err := valueRelation(tc.column, tc.left, tc.right)
		if err != nil || got != tc.want {
			t.Fatalf("%s %v vs %v: got %s err %v", tc.column, tc.left, tc.right, got, err)
		}
	}
	stored, class, ok := legacyStoredTimestamp("2026-01-02T08:34:05+05:30")
	if !ok || stored != "2026-01-02 03:04:05" || class != "zoned" {
		t.Fatalf("zoned timestamp %s %s %v", stored, class, ok)
	}
	stored, class, ok = legacyStoredTimestamp("2026-01-02 03:04:05")
	if !ok || stored != "2026-01-02 03:04:05" || class != "zone_unspecified" {
		t.Fatalf("unzoned timestamp %s %s %v", stored, class, ok)
	}
	if _, _, ok = legacyStoredTimestamp("not-a-time"); ok {
		t.Fatal("unparseable timestamp was accepted")
	}
}

func TestAuditVerifyRequiresSourceOriginalForPolicyDrift(t *testing.T) {
	src, srcPath := newSchemaDB(t)
	insertSyntheticHistory(t, src)
	freezeClocks(t, src)
	if _, err := src.Exec(`UPDATE messages SET created_at='2026-02-02 03:04:05' WHERE id='synthetic-m1'`); err != nil {
		t.Fatal(err)
	}
	src.Close()
	dst, dstPath := newSchemaDB(t)
	insertSyntheticHistory(t, dst)
	freezeClocks(t, dst)
	dst.Close()
	staged, stagedPath := newSchemaDB(t)
	insertSyntheticHistory(t, staged)
	freezeClocks(t, staged)
	staged.Close()
	mapping := writeMap(t, AccountMapping{
		Label: "synthetic-a", Phone: "15550001000",
		SourceStorageJID: "15550001000@s.whatsapp.net", TargetStorageJID: "15550001000@s.whatsapp.net",
	})
	auditPath := filepath.Join(t.TempDir(), "audit.db")
	if err := writeMigrationAudit(auditPath, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	srcSum, stagedSum := fileSum(t, srcPath), fileSum(t, stagedPath)
	failed, err := AuditVerify(context.Background(), AuditVerifyRequest{
		Staged: stagedPath, Source: srcPath, Target: dstPath, MapPath: mapping, Audit: auditPath, Base: "target",
	})
	if err == nil || failed.Verified || failed.SourceOriginalsMissing != 1 || failed.SourceUnexplained != 0 || failed.TargetMissing != 0 {
		t.Fatalf("policy drift %+v %v", failed, err)
	}
	if len(failed.ColumnClasses) != 1 || failed.ColumnClasses[0].Column != "created_at" || failed.ColumnClasses[0].Class != "provenance" {
		t.Fatalf("classes %+v", failed.ColumnClasses)
	}
	encoded, _ := json.Marshal(failed)
	if strings.Contains(string(encoded), "synthetic hello") {
		t.Fatal("diagnostics printed message text")
	}
	kept, err := AuditVerify(context.Background(), AuditVerifyRequest{
		Staged: stagedPath, Source: srcPath, Target: dstPath, MapPath: mapping, Audit: auditPath, Base: "target", RetainMissing: true,
	})
	if err != nil || !kept.Verified || kept.OriginalsRecorded != 1 || kept.SourcePolicyAudited != 1 || kept.HistoryComplete || kept.CutoverReady {
		t.Fatalf("retained %+v %v", kept, err)
	}
	if fileSum(t, srcPath) != srcSum || fileSum(t, stagedPath) != stagedSum {
		t.Fatal("retain changed a message database")
	}
	stagedDB, err := sql.Open(sqlite.DriverName, stagedPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stagedDB.Exec(`UPDATE messages SET content='changed body' WHERE id='synthetic-m1'`); err != nil {
		t.Fatal(err)
	}
	stagedDB.Close()
	genuine, err := AuditVerify(context.Background(), AuditVerifyRequest{
		Staged: stagedPath, Source: srcPath, Target: dstPath, MapPath: mapping, Audit: auditPath, Base: "target", RetainMissing: true,
	})
	if err == nil || genuine.Verified || genuine.SourceUnexplained != 1 {
		t.Fatalf("genuine diff was accepted %+v %v", genuine, err)
	}
	encoded, _ = json.Marshal(genuine)
	if strings.Contains(string(encoded), "changed body") || strings.Contains(string(encoded), "synthetic hello") {
		t.Fatal("genuine diagnostic printed a payload")
	}
}

func TestOverlayAuditsSourceWhenTimestampIsKept(t *testing.T) {
	src, srcPath := newSchemaDB(t)
	insertSyntheticHistory(t, src)
	freezeClocks(t, src)
	src.Close()
	dst, dstPath := newSchemaDB(t)
	insertSyntheticHistory(t, dst)
	freezeClocks(t, dst)
	if _, err := dst.Exec(`UPDATE messages SET timestamp='2026-05-05 00:00:00' WHERE id='synthetic-m1'`); err != nil {
		t.Fatal(err)
	}
	dst.Close()
	mapping := writeMap(t, AccountMapping{
		Label: "synthetic-a", Phone: "15550001000",
		SourceStorageJID: "15550001000@s.whatsapp.net", TargetStorageJID: "15550001000@s.whatsapp.net",
	})
	out := filepath.Join(t.TempDir(), "union.db")
	overlay, err := Overlay(context.Background(), OverlayRequest{
		Source: srcPath, Target: dstPath, MapPath: mapping, WorkDir: t.TempDir(), Out: out, Base: "target",
	})
	if err != nil || !overlay.Applied {
		t.Fatalf("overlay %+v %v", overlay, err)
	}
	found, err := rowsInAudit(overlay.AuditPath, "synthetic hello")
	if err != nil || !found {
		t.Fatalf("source original missing from audit: %v", err)
	}
	verified, err := AuditVerify(context.Background(), AuditVerifyRequest{
		Staged: out, Source: srcPath, Target: dstPath, MapPath: mapping, Audit: overlay.AuditPath, Base: "target",
	})
	if err != nil || !verified.Verified || verified.SourceUnexplained != 0 || verified.SourceOriginalsMissing != 0 {
		t.Fatalf("verify %+v %v", verified, err)
	}
	staged, err := sql.Open(sqlite.DriverName, out)
	if err != nil {
		t.Fatal(err)
	}
	defer staged.Close()
	var stamp string
	if err := staged.QueryRow(`SELECT timestamp FROM messages WHERE id='synthetic-m1'`).Scan(&stamp); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stamp, "2026-05-05") {
		t.Fatalf("base timestamp was replaced: %s", stamp)
	}
}

func TestLegacyImportAppendsMissingWithoutReplacingOrGuessing(t *testing.T) {
	staged, stagedPath := newSchemaDB(t)
	insertSyntheticHistory(t, staged)
	if _, err := staged.Exec(`INSERT INTO devices (device_id, display_name, jid) VALUES ('synthetic-slot-b', 'synthetic b', '15550004000@s.whatsapp.net')`); err != nil {
		t.Fatal(err)
	}
	if _, err := staged.Exec(`UPDATE messages SET content='gowa primary' WHERE id='synthetic-m1'`); err != nil {
		t.Fatal(err)
	}
	staged.Close()
	accountA := filepath.Join(t.TempDir(), "legacy-a.db")
	accountB := filepath.Join(t.TempDir(), "legacy-b.db")
	writeLegacyFixture(t, accountA, []string{
		`INSERT INTO chats (jid, name, last_message_time) VALUES ('15550002000@s.whatsapp.net', 'peer', '2026-01-02 03:04:05'), ('120363@g.us', 'group', '2026-01-02 05:00:00'), ('123@broadcast', 'cast', '2026-01-02 06:00:00'), ('15550003000@s.whatsapp.net', 'other', '2026-01-02 07:00:00')`,
		`INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, filename, direct_path, media_key, quoted_text_preview) VALUES
			('legacy-new', '15550002000@s.whatsapp.net', '15550002000@s.whatsapp.net', 'legacy new', '2026-01-02T08:34:05+05:30', 0, 'note.pdf', 'legacy/note.pdf', x'0102', 'quoted secret'),
			('synthetic-m1', '15550002000@s.whatsapp.net', '15550002000@s.whatsapp.net', 'legacy old', '2026-01-02 04:00:00', 0, '', '', NULL, ''),
			('legacy-group', '120363@g.us', '', 'group secret', '2026-01-02 05:00:00', 0, '', '', NULL, ''),
			('legacy-cast', '123@broadcast', '15550001000@s.whatsapp.net', 'broadcast body', '2026-01-02 06:00:00', 0, '', '', NULL, ''),
			('legacy-same', '15550002000@s.whatsapp.net', '15550002000@s.whatsapp.net', 'chat one', '2026-01-02 07:00:00', 0, '', '', NULL, ''),
			('legacy-same', '15550003000@s.whatsapp.net', '15550003000@s.whatsapp.net', 'chat two', '2026-01-02 07:05:00', 0, '', '', NULL, ''),
			('legacy-bad', '15550002000@s.whatsapp.net', '15550002000@s.whatsapp.net', 'bad time', 'not-a-time', 0, '', '', NULL, ''),
			('legacy-clock', '15550002000@s.whatsapp.net', '15550002000@s.whatsapp.net', 'clock body', '2026-01-02 03:04:05', 0, '', '', NULL, '')`,
		`UPDATE messages SET sender_name='hidden name' WHERE id='legacy-group'`,
		`INSERT INTO contact_nicknames (jid, nickname) VALUES ('15550002000@s.whatsapp.net', 'nick-private')`,
		`INSERT INTO poll_options (message_id, chat_jid, option_index, option_name) VALUES ('legacy-new', '15550002000@s.whatsapp.net', 0, 'option text')`,
		`INSERT INTO webhook_configs (id, name, webhook_url, secret_token) VALUES (1, 'hook', 'https://example.invalid/hook', 'legacy-webhook-secret')`,
		`INSERT INTO webhook_logs (id, webhook_config_id, payload, response_body) VALUES (1, 1, 'payload secret', 'response secret')`,
	})
	writeLegacyFixture(t, accountB, []string{
		`INSERT INTO chats (jid, name, last_message_time) VALUES ('15550002000@s.whatsapp.net', 'peer', '2026-01-02 03:04:05')`,
		`INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) VALUES ('legacy-new', '15550002000@s.whatsapp.net', '15550002000@s.whatsapp.net', 'account b', '2026-01-02T08:34:05+05:30', 0)`,
	})
	beforeA, beforeB := fileSum(t, accountA), fileSum(t, accountB)
	mapping := writeLegacyMap(t,
		legacyMapEntry(t, "legacy-a", accountA, "15550001000@s.whatsapp.net"),
		legacyMapEntry(t, "legacy-b", accountB, "15550004000@s.whatsapp.net"),
	)
	coverage, err := LegacyCoverage(context.Background(), LegacyRequest{Staged: stagedPath, MapPath: mapping})
	if err != nil || coverage.Applied || coverage.HistoryComplete || coverage.OldBridgeAssessed || coverage.FilesRead != 2 {
		t.Fatalf("coverage %+v %v", coverage, err)
	}
	if coverage.Accounts[0].Inserted != 5 || coverage.Accounts[0].SenderUnresolved != 1 || coverage.Accounts[0].TimestampUnresolved != 1 || coverage.Accounts[0].Conflict != 1 || coverage.Accounts[0].ZoneUnspecified == 0 || coverage.Accounts[0].BroadcastOrNewsletter != 1 || strings.HasPrefix(coverage.Accounts[0].TimestampMin, "0001") {
		t.Fatalf("coverage counts %+v", coverage.Accounts[0])
	}
	imported, err := LegacyImport(context.Background(), LegacyRequest{Staged: stagedPath, MapPath: mapping})
	if err != nil || !imported.Applied || imported.MediaBytesCopied || imported.HistoryComplete || imported.CutoverReady {
		t.Fatalf("import %+v %v", imported, err)
	}
	if fileSum(t, accountA) != beforeA || fileSum(t, accountB) != beforeB {
		t.Fatal("legacy import wrote a source file")
	}
	encoded, _ := json.Marshal(imported)
	for _, hidden := range []string{"legacy new", "legacy old", "gowa primary", "group secret", "hidden name", "broadcast body", "note.pdf", "legacy/note.pdf", "legacy-webhook-secret", "payload secret", "nick-private", "option text", "quoted secret", "account b"} {
		if strings.Contains(string(encoded), hidden) {
			t.Fatalf("report contains %s", hidden)
		}
	}
	if len(imported.Limitations) == 0 || !strings.Contains(imported.Limitations[0], "no supported sender") {
		t.Fatalf("limitation %+v", imported.Limitations)
	}
	db, err := sql.Open(sqlite.DriverName, stagedPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var content, stamp, filename, direct string
	var media []byte
	if err := db.QueryRow(`SELECT content, timestamp, filename, direct_path, media_key FROM messages WHERE id='legacy-new' AND device_id='15550001000@s.whatsapp.net'`).Scan(&content, &stamp, &filename, &direct, &media); err != nil {
		t.Fatal(err)
	}
	if content != "legacy new" || !instantIs(stamp, "2026-01-02 03:04:05") || filename != "note.pdf" || direct != "legacy/note.pdf" || len(media) != 2 {
		t.Fatalf("inserted row %s %s %s %s %x", content, stamp, filename, direct, media)
	}
	var primary string
	if err := db.QueryRow(`SELECT content FROM messages WHERE id='synthetic-m1'`).Scan(&primary); err != nil || primary != "gowa primary" {
		t.Fatalf("conflict replaced primary %s %v", primary, err)
	}
	var groupN, castN, sameN, badN int
	var clock string
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE id='legacy-group'`).Scan(&groupN); err != nil || groupN != 0 {
		t.Fatalf("group sender was invented: %d %v", groupN, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE id='legacy-cast'`).Scan(&castN); err != nil || castN != 1 {
		t.Fatalf("broadcast dropped: %d %v", castN, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE id='legacy-same'`).Scan(&sameN); err != nil || sameN != 2 {
		t.Fatalf("same id was collapsed across chats: %d %v", sameN, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE id='legacy-bad'`).Scan(&badN); err != nil || badN != 0 {
		t.Fatalf("bad timestamp inserted: %d", badN)
	}
	if err := db.QueryRow(`SELECT timestamp FROM messages WHERE id='legacy-clock'`).Scan(&clock); err != nil || !instantIs(clock, "2026-01-02 03:04:05") {
		t.Fatalf("unzoned clock shifted: %s %v", clock, err)
	}
	var otherDevice int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE id='legacy-new' AND device_id='15550004000@s.whatsapp.net' AND content='account b'`).Scan(&otherDevice); err != nil || otherDevice != 1 {
		t.Fatalf("accounts were mixed: %d %v", otherDevice, err)
	}
	var polls int
	if err := db.QueryRow(`SELECT COUNT(*) FROM poll_definitions WHERE question='option text'`).Scan(&polls); err != nil || polls != 0 {
		t.Fatalf("poll question was invented: %d", polls)
	}
	audit := imported.AuditPath
	for _, needle := range []string{"group secret", "legacy old", "nick-private", "option text", "quoted secret"} {
		found, err := legacyAuditHas(audit, needle)
		if err != nil || !found {
			t.Fatalf("audit missing %s: %v", needle, err)
		}
	}
	found, err := legacyAuditHas(audit, "legacy-webhook-secret")
	if err != nil || found {
		t.Fatal("webhook secret was copied into the audit")
	}
	again, err := LegacyImport(context.Background(), LegacyRequest{Staged: stagedPath, MapPath: mapping, Audit: audit})
	if err != nil || !again.Applied || again.Accounts[0].Inserted != 0 || again.Accounts[1].Inserted != 0 {
		t.Fatalf("repeat %+v %v", again, err)
	}
	var rows int
	auditDB, err := sql.Open(sqlite.DriverName, audit)
	if err != nil {
		t.Fatal(err)
	}
	if err := auditDB.QueryRow(`SELECT COUNT(*) FROM legacy_rows`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	auditDB.Close()
	if rows == 0 {
		t.Fatal("audit has no legacy rows")
	}
	againDB, err := sql.Open(sqlite.DriverName, audit)
	if err != nil {
		t.Fatal(err)
	}
	var rowsAgain int
	if err := againDB.QueryRow(`SELECT COUNT(*) FROM legacy_rows`).Scan(&rowsAgain); err != nil {
		t.Fatal(err)
	}
	againDB.Close()
	if rowsAgain != rows {
		t.Fatalf("repeat duplicated audit rows %d to %d", rows, rowsAgain)
	}
}

func TestLegacyMapRefusesHashMismatchAndSessionDatabases(t *testing.T) {
	staged, stagedPath := newSchemaDB(t)
	insertSyntheticHistory(t, staged)
	staged.Close()
	var before int
	db, err := sql.Open(sqlite.DriverName, stagedPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	db.Close()
	source := filepath.Join(t.TempDir(), "legacy-a.db")
	writeLegacyFixture(t, source, []string{
		`INSERT INTO chats (jid, name, last_message_time) VALUES ('15550002000@s.whatsapp.net', 'peer', '2026-01-02 03:04:05')`,
		`INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) VALUES ('legacy-new', '15550002000@s.whatsapp.net', '15550002000@s.whatsapp.net', 'legacy new', '2026-01-02 03:04:05', 0)`,
	})
	entry := legacyMapEntry(t, "legacy-a", source, "15550001000@s.whatsapp.net")
	entry.SourceSHA256 = strings.Repeat("ab", 32)
	mapping := writeLegacyMap(t, entry)
	if _, err := LegacyImport(context.Background(), LegacyRequest{Staged: stagedPath, MapPath: mapping}); err == nil {
		t.Fatal("hash mismatch was accepted")
	}
	db, err = sql.Open(sqlite.DriverName, stagedPath)
	if err != nil {
		t.Fatal(err)
	}
	var after int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if after != before {
		t.Fatal("rejected import wrote the staged database")
	}
	keysPath := filepath.Join(t.TempDir(), "keys.db")
	writeLegacyFixture(t, keysPath, nil)
	keysMap := writeLegacyMap(t, legacyMapEntry(t, "legacy-keys", keysPath, "15550001000@s.whatsapp.net"))
	if _, err := LegacyCoverage(context.Background(), LegacyRequest{Staged: stagedPath, MapPath: keysMap}); err == nil || !strings.Contains(err.Error(), "session or key") {
		t.Fatalf("keys.db was not refused: %v", err)
	}
	sessionPath := filepath.Join(t.TempDir(), "messages.db")
	session, err := sql.Open(sqlite.DriverName, sessionPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.Exec(`CREATE TABLE whatsmeow_sessions (id TEXT)`); err != nil {
		t.Fatal(err)
	}
	session.Close()
	sessionMap := writeLegacyMap(t, legacyMapEntry(t, "legacy-session", sessionPath, "15550001000@s.whatsapp.net"))
	if _, err := LegacyCoverage(context.Background(), LegacyRequest{Staged: stagedPath, MapPath: sessionMap}); err == nil || !strings.Contains(err.Error(), "session or key") {
		t.Fatalf("session table was not refused: %v", err)
	}
}

func instantIs(got, want string) bool {
	if got == want || strings.Contains(got, want) {
		return true
	}
	left, leftOK := parseTimeText(got)
	right, rightOK := parseTimeText(want)
	return leftOK && rightOK && left.Equal(right)
}

func writeLegacyFixture(t *testing.T, path string, statements []string) {
	t.Helper()
	db, err := sql.Open(sqlite.DriverName, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	schema := `
		CREATE TABLE chats (jid TEXT PRIMARY KEY, name TEXT, last_message_time TIMESTAMP);
		CREATE TABLE messages (
			id TEXT, chat_jid TEXT, sender TEXT, sender_name TEXT, content TEXT, timestamp TIMESTAMP, is_from_me BOOLEAN,
			media_type TEXT, filename TEXT, url TEXT, media_key BLOB, file_sha256 BLOB, file_enc_sha256 BLOB, file_length INTEGER,
			direct_path TEXT, quoted_message_id TEXT, quoted_sender_name TEXT, quoted_text_preview TEXT, reply_to_message_id TEXT,
			edit_count INTEGER, is_edited BOOLEAN, is_forwarded BOOLEAN, forwarded_from TEXT, is_system_message BOOLEAN, system_message_type TEXT,
			PRIMARY KEY (id, chat_jid), FOREIGN KEY (chat_jid) REFERENCES chats(jid)
		);
		CREATE TABLE contact_nicknames (jid TEXT PRIMARY KEY, nickname TEXT, created_at TIMESTAMP, updated_at TIMESTAMP);
		CREATE TABLE poll_current_votes (
			poll_message_id TEXT, chat_jid TEXT, voter_jid TEXT, selected_options TEXT, vote_timestamp_ms INTEGER, updated_at TIMESTAMP,
			PRIMARY KEY (poll_message_id, chat_jid, voter_jid)
		);
		CREATE TABLE poll_options (
			message_id TEXT, chat_jid TEXT, option_index INTEGER, option_name TEXT,
			PRIMARY KEY (message_id, chat_jid, option_index)
		);
		CREATE TABLE webhook_configs (
			id INTEGER PRIMARY KEY, name TEXT NOT NULL, webhook_url TEXT NOT NULL, secret_token TEXT, enabled BOOLEAN, created_at TIMESTAMP, updated_at TIMESTAMP
		);
		CREATE TABLE webhook_logs (
			id INTEGER PRIMARY KEY, webhook_config_id INTEGER, message_id TEXT, chat_jid TEXT, trigger_type TEXT, trigger_value TEXT,
			payload TEXT, response_status INTEGER, response_body TEXT, attempt_count INTEGER, delivered_at TIMESTAMP, created_at TIMESTAMP,
			FOREIGN KEY (webhook_config_id) REFERENCES webhook_configs(id)
		);
		CREATE TABLE webhook_triggers (
			id INTEGER PRIMARY KEY, webhook_config_id INTEGER, trigger_type TEXT NOT NULL, trigger_value TEXT, match_type TEXT, enabled BOOLEAN,
			FOREIGN KEY (webhook_config_id) REFERENCES webhook_configs(id)
		);
	`
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
}

func legacyMapEntry(t *testing.T, label, path, jid string) LegacyAccount {
	t.Helper()
	sum, err := hashFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return LegacyAccount{Label: label, SourcePath: path, SourceSHA256: sum, TargetStorageJID: jid}
}

func writeLegacyMap(t *testing.T, accounts ...LegacyAccount) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "legacy-map.json")
	raw, err := json.Marshal(struct {
		Accounts []LegacyAccount `json:"accounts"`
	}{Accounts: accounts})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func legacyAuditHas(path, needle string) (bool, error) {
	db, err := openSQLite(path, true)
	if err != nil {
		return false, err
	}
	defer db.Close()
	var n int
	err = db.QueryRow(`SELECT COUNT(*) FROM legacy_rows WHERE row_json LIKE ?`, "%"+needle+"%").Scan(&n)
	return n > 0, err
}
