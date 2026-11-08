package dbreuse

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aldinokemal/go-whatsapp-web-multidevice/pkg/sqlite"
)

func TestLegacyVerifyRequiresExactOriginals(t *testing.T) {
	stagedPath, source, mapping, auditPath := importLegacyVerifyFixture(t)
	stagedSum, sourceSum, auditSum := fileSum(t, stagedPath), fileSum(t, source), fileSum(t, auditPath)
	verified, err := LegacyVerify(context.Background(), LegacyVerifyRequest{Staged: stagedPath, MapPath: mapping, Audit: auditPath})
	if err != nil || !verified.Verified || verified.HistoryComplete || verified.CutoverReady || verified.OldBridgeAssessed || !verified.SourcesUnchanged || !verified.AuditSchemaOK {
		t.Fatalf("verify %+v %v", verified, err)
	}
	if verified.Messages != 3 || verified.StagedPayloadMatch != 1 || verified.AuditOnly != 2 || verified.AuditExact < 3 || verified.Missing != 0 || verified.Tampered != 0 {
		t.Fatalf("counts %+v", verified)
	}
	if verified.Accounts[0].SenderUnresolved != 1 || verified.Accounts[0].Unsupported[0].AuditExact != 1 || verified.Accounts[0].Unsupported[1].AuditExact != 1 {
		t.Fatalf("account %+v", verified.Accounts[0])
	}
	if len(verified.Limitations) == 0 || !strings.Contains(verified.Limitations[0], "no supported sender") {
		t.Fatalf("limitations %+v", verified.Limitations)
	}
	assertNoPrivate(t, verified)
	if fileSum(t, stagedPath) != stagedSum || fileSum(t, source) != sourceSum || fileSum(t, auditPath) != auditSum {
		t.Fatal("verify changed a database")
	}
	var help bytes.Buffer
	if code := runOffline(&help, &bytes.Buffer{}, []string{"help"}); code != 0 || !strings.Contains(help.String(), "legacy-verify --db FILE --map FILE [--audit FILE]") {
		t.Fatalf("help %d %s", code, help.String())
	}

	tamperedPath := filepath.Join(t.TempDir(), "tampered.db")
	copyFile(t, auditPath, tamperedPath)
	tamperedDB, err := sql.Open(sqlite.DriverName, tamperedPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tamperedDB.Exec(`UPDATE legacy_rows SET row_json=replace(row_json, 'legacy old', 'changed old') WHERE message_id='synthetic-m1'`); err != nil {
		t.Fatal(err)
	}
	tamperedDB.Close()
	failed, err := LegacyVerify(context.Background(), LegacyVerifyRequest{Staged: stagedPath, MapPath: mapping, Audit: tamperedPath})
	if err == nil || failed.Verified || failed.Tampered != 1 || failed.Missing != 0 {
		t.Fatalf("tamper was accepted %+v %v", failed, err)
	}
	assertNoPrivate(t, failed)
	if strings.Contains(mustJSON(failed), "changed old") {
		t.Fatal("tamper report printed the replacement")
	}

	missingPath := filepath.Join(t.TempDir(), "missing.db")
	copyFile(t, auditPath, missingPath)
	missingDB, err := sql.Open(sqlite.DriverName, missingPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := missingDB.Exec(`DELETE FROM legacy_rows WHERE table_name='contact_nicknames'`); err != nil {
		t.Fatal(err)
	}
	if _, err := missingDB.Exec(`DELETE FROM legacy_rows WHERE message_id='legacy-new'`); err != nil {
		t.Fatal(err)
	}
	missingDB.Close()
	lost, err := LegacyVerify(context.Background(), LegacyVerifyRequest{Staged: stagedPath, MapPath: mapping, Audit: missingPath})
	if err == nil || lost.Verified || lost.Missing < 2 || lost.StagedPayloadMatch != 1 {
		t.Fatalf("staged identity waived a dropped original %+v %v", lost, err)
	}
	assertNoPrivate(t, lost)

	wrong := legacyMapEntry(t, "legacy-a", source, "15550001000@s.whatsapp.net")
	wrong.SourceSHA256 = strings.Repeat("ab", 32)
	wrongMap := writeLegacyMap(t, wrong)
	if _, err := LegacyVerify(context.Background(), LegacyVerifyRequest{Staged: stagedPath, MapPath: wrongMap, Audit: auditPath}); err == nil || !strings.Contains(err.Error(), "provenance") {
		t.Fatalf("wrong hash was accepted: %v", err)
	}
	if fileSum(t, stagedPath) != stagedSum {
		t.Fatal("wrong hash wrote the staged database")
	}

	other := writeLegacyMap(t, legacyMapEntry(t, "legacy-b", source, "15550004000@s.whatsapp.net"))
	mismatched, err := LegacyVerify(context.Background(), LegacyVerifyRequest{Staged: stagedPath, MapPath: other, Audit: auditPath})
	if err == nil || mismatched.Verified || mismatched.Missing == 0 || mismatched.AuditExact != 0 {
		t.Fatalf("wrong account was accepted %+v %v", mismatched, err)
	}
	assertNoPrivate(t, mismatched)

	keysPath := filepath.Join(t.TempDir(), "keys.db")
	writeLegacyFixture(t, keysPath, nil)
	keysMap := writeLegacyMap(t, legacyMapEntry(t, "legacy-keys", keysPath, "15550001000@s.whatsapp.net"))
	if _, err := LegacyVerify(context.Background(), LegacyVerifyRequest{Staged: stagedPath, MapPath: keysMap, Audit: auditPath}); err == nil || !strings.Contains(err.Error(), "session or key") {
		t.Fatalf("keys.db was not refused: %v", err)
	}
}

func importLegacyVerifyFixture(t *testing.T) (stagedPath, source, mapping, auditPath string) {
	t.Helper()
	staged, stagedPath := newSchemaDB(t)
	insertSyntheticHistory(t, staged)
	if _, err := staged.Exec(`INSERT INTO devices (device_id, display_name, jid) VALUES ('synthetic-slot-b', 'synthetic b', '15550004000@s.whatsapp.net')`); err != nil {
		t.Fatal(err)
	}
	if _, err := staged.Exec(`UPDATE messages SET content='gowa primary' WHERE id='synthetic-m1'`); err != nil {
		t.Fatal(err)
	}
	staged.Close()
	source = filepath.Join(t.TempDir(), "legacy-a.db")
	writeLegacyFixture(t, source, []string{
		`INSERT INTO chats (jid, name, last_message_time) VALUES ('15550002000@s.whatsapp.net', 'peer', '2026-01-02 03:04:05'), ('120363@g.us', 'group', '2026-01-02 05:00:00')`,
		`INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, quoted_text_preview) VALUES
			('synthetic-m1', '15550002000@s.whatsapp.net', '15550002000@s.whatsapp.net', 'legacy old', '2026-01-02 04:00:00', 0, ''),
			('legacy-new', '15550002000@s.whatsapp.net', '15550002000@s.whatsapp.net', 'legacy new', '2026-01-02T08:34:05+05:30', 0, 'quoted secret'),
			('legacy-group', '120363@g.us', '', 'group secret', '2026-01-02 05:00:00', 0, '')`,
		`UPDATE messages SET sender_name='hidden name' WHERE id='legacy-new'`,
		`INSERT INTO contact_nicknames (jid, nickname) VALUES ('15550002000@s.whatsapp.net', 'nick-private')`,
		`INSERT INTO poll_options (message_id, chat_jid, option_index, option_name) VALUES ('legacy-new', '15550002000@s.whatsapp.net', 0, 'option text')`,
		`INSERT INTO webhook_logs (id, payload, response_body) VALUES (1, 'payload secret', 'response secret')`,
	})
	mapping = writeLegacyMap(t, legacyMapEntry(t, "legacy-a", source, "15550001000@s.whatsapp.net"))
	imported, err := LegacyImport(context.Background(), LegacyRequest{Staged: stagedPath, MapPath: mapping})
	if err != nil || !imported.Applied {
		t.Fatalf("import %+v %v", imported, err)
	}
	return stagedPath, source, mapping, imported.AuditPath
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	raw, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertNoPrivate(t *testing.T, report any) {
	t.Helper()
	encoded := mustJSON(report)
	for _, hidden := range []string{"legacy new", "legacy old", "gowa primary", "group secret", "hidden name", "quoted secret", "nick-private", "option text", "payload secret", "response secret"} {
		if strings.Contains(encoded, hidden) {
			t.Fatalf("report contains %s", hidden)
		}
	}
}

func mustJSON(report any) string {
	raw, _ := json.Marshal(report)
	return string(raw)
}
