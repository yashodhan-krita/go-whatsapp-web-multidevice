package dbreuse

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aldinokemal/go-whatsapp-web-multidevice/pkg/sqlite"
)

func TestArchiveMessageTextUsesSupportedFieldsOnly(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
		ok   bool
	}{
		{"ephemeral", `{"ephemeralMessage":{"message":{"conversation":"disappearing hello"}}}`, "disappearing hello", true},
		{"caption", `{"imageMessage":{"caption":"look at this"}}`, "look at this", true},
		{"quoted", `{"imageMessage":{"contextInfo":{"quotedMessage":{"conversation":"quoted secret"}}}}`, "", false},
		{"matched", `{"extendedTextMessage":{"matchedText":"https://example.invalid/a"}}`, "https://example.invalid/a", true},
		{"template", `{"templateMessage":{"Format":{"HydratedFourRowTemplate":{"hydratedContentText":"shipped notice","Title":{"HydratedTitleText":"Order confirmed"}}}}}`, "Order confirmed\nshipped notice", true},
		{"edit", `{"protocolMessage":{"editedMessage":{"conversation":"edited line"}}}`, "edited line", true},
		{"contact", `{"contactMessage":{"displayName":"Ada","vcard":"TEL:+15550009999"}}`, "Contact: Ada (+15550009999)", true},
		{"location", `{"locationMessage":{"degreesLatitude":1.5,"degreesLongitude":2.5}}`, "https://maps.google.com/?q=1.5,2.5", true},
		{"poll", `{"pollCreationMessageV3":{"name":"Which day"}}`, "Which day", true},
		{"document caption", `{"documentWithCaptionMessage":{"message":{"documentMessage":{"caption":"the file"}}}}`, "the file", true},
		{"album child", `{"associatedChildMessage":{"message":{"imageMessage":{"caption":"album caption"}}}}`, "album caption", true},
		{"audio", `{"audioMessage":{"mimetype":"audio/ogg"}}`, "", false},
		{"placeholder", `{"placeholderMessage":{"type":0}}`, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var message map[string]json.RawMessage
			if err := json.Unmarshal([]byte(tc.raw), &message); err != nil {
				t.Fatal(err)
			}
			got, ok := archiveMessageText(message)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("text %q ok %v, want %q ok %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestImportArchiveDecodesNestedTextWithoutGuessingSender(t *testing.T) {
	db, path := newSchemaDB(t)
	insertSyntheticHistory(t, db)
	db.Close()
	archive := filepath.Join(t.TempDir(), "history-1-15550001000@s.whatsapp.net-1-RECENT.json")
	body := `{
		"syncType":2,
		"conversations":[
			{"ID":"120363000@g.us","messages":[
				{"message":{"key":{"remoteJid":"120363000@g.us","fromMe":false,"id":"synthetic-group-open"},"message":{"conversation":"group body"},"messageTimestamp":1700000003}},
				{"message":{"key":{"remoteJid":"120363000@g.us","fromMe":false,"id":"synthetic-group-known"},"participant":"999001@lid","message":{"conversation":"known sender"},"messageTimestamp":1700000004}}
			]},
			{"ID":"15550002000@s.whatsapp.net","messages":[
				{"message":{"key":{"remoteJid":"15550002000@s.whatsapp.net","fromMe":false,"id":"synthetic-caption"},"message":{"imageMessage":{"caption":"look at this","url":"https://example.invalid/secret"}},"messageTimestamp":1700000005}},
				{"message":{"key":{"remoteJid":"15550002000@s.whatsapp.net","fromMe":true,"id":"synthetic-ephemeral"},"message":{"ephemeralMessage":{"message":{"conversation":"disappearing hello"}}},"messageTimestamp":1700000006}},
				{"message":{"key":{"remoteJid":"15550002000@s.whatsapp.net","fromMe":false,"id":"synthetic-quote"},"message":{"imageMessage":{"contextInfo":{"quotedMessage":{"conversation":"quoted secret"}}}},"messageTimestamp":1700000007}},
				{"message":{"key":{"remoteJid":"15550002000@s.whatsapp.net","fromMe":false,"id":"synthetic-audio"},"message":{"audioMessage":{"mimetype":"audio/ogg"}},"messageTimestamp":1700000008}},
				{"message":{"key":{"remoteJid":"15550002000@s.whatsapp.net","fromMe":false,"id":"synthetic-template"},"message":{"templateMessage":{"Format":{"HydratedFourRowTemplate":{"hydratedContentText":"shipped notice","Title":{"HydratedTitleText":"Order confirmed"}}}}},"messageTimestamp":1700000009}},
				{"message":{"key":{"remoteJid":"15550002000@s.whatsapp.net","fromMe":false,"id":"synthetic-m1"},"message":{"conversation":"archive replacement"},"messageTimestamp":1700000010}}
			]}
		]
	}`
	if err := writeTestFile(t, archive, body); err != nil {
		t.Fatal(err)
	}
	report, err := ImportArchives(ImportRequest{DB: path, Archives: []string{archive}})
	if err != nil {
		t.Fatal(err)
	}
	if report.HistoryComplete || report.OldBridgeAssessed || report.SenderUnresolved != 1 || report.TextRetainedOnlyInAudit != 2 {
		t.Fatalf("report %+v", report)
	}
	encoded, _ := json.Marshal(report)
	for _, hidden := range []string{"group body", "known sender", "look at this", "disappearing hello", "quoted secret", "shipped notice", "Order confirmed", "archive replacement", "example.invalid"} {
		if strings.Contains(string(encoded), hidden) {
			t.Fatalf("report contains %s", hidden)
		}
	}
	if len(report.SenderUnresolvedShapes) != 1 || !strings.Contains(report.SenderUnresolvedShapes[0].Shape, "chat=g.us") || !strings.Contains(report.SenderUnresolvedShapes[0].Shape, "key_participant=absent") || !strings.Contains(report.SenderUnresolvedShapes[0].Shape, "info_participant=absent") {
		t.Fatalf("sender shapes %+v", report.SenderUnresolvedShapes)
	}
	shapes := map[string]int{}
	for _, shape := range report.TextUnresolvedShapes {
		shapes[shape.Shape] = shape.Count
	}
	if shapes["imageMessage"] != 1 || shapes["audioMessage"] != 1 {
		t.Fatalf("text shapes %+v", report.TextUnresolvedShapes)
	}
	staged, err := sql.Open(sqlite.DriverName, path)
	if err != nil {
		t.Fatal(err)
	}
	defer staged.Close()
	var sender, content, original, quoted, audio string
	var missing int
	if err := staged.QueryRow(`SELECT sender, content FROM messages WHERE id='synthetic-group-known'`).Scan(&sender, &content); err != nil || sender != "999001@lid" || content != "known sender" {
		t.Fatalf("group sender %q content %q %v", sender, content, err)
	}
	if err := staged.QueryRow(`SELECT COUNT(*) FROM messages WHERE id='synthetic-group-open'`).Scan(&missing); err != nil || missing != 0 {
		t.Fatalf("unresolved group inserted %d %v", missing, err)
	}
	if err := staged.QueryRow(`SELECT content FROM messages WHERE id='synthetic-caption'`).Scan(&content); err != nil || content != "look at this" {
		t.Fatalf("caption %q %v", content, err)
	}
	if err := staged.QueryRow(`SELECT content FROM messages WHERE id='synthetic-ephemeral'`).Scan(&content); err != nil || content != "disappearing hello" {
		t.Fatalf("ephemeral %q %v", content, err)
	}
	if err := staged.QueryRow(`SELECT content FROM messages WHERE id='synthetic-template'`).Scan(&content); err != nil || content != "Order confirmed\nshipped notice" {
		t.Fatalf("template %q %v", content, err)
	}
	if err := staged.QueryRow(`SELECT content FROM messages WHERE id='synthetic-quote'`).Scan(&quoted); err != nil || quoted != "" {
		t.Fatalf("quoted content %q %v", quoted, err)
	}
	if err := staged.QueryRow(`SELECT content FROM messages WHERE id='synthetic-audio'`).Scan(&audio); err != nil || audio != "" {
		t.Fatalf("audio content %q %v", audio, err)
	}
	if err := staged.QueryRow(`SELECT content FROM messages WHERE id='synthetic-m1'`).Scan(&original); err != nil || original != "synthetic hello" {
		t.Fatalf("existing content changed to %q %v", original, err)
	}
	kept, err := rowsInAudit(report.AuditPath, "quoted secret")
	if err != nil || !kept {
		t.Fatalf("quoted audit %v", err)
	}
	kept, err = rowsInAudit(report.AuditPath, "group body")
	if err != nil || !kept {
		t.Fatalf("group audit %v", err)
	}
}

func TestImportArchiveFillsOnlyPreviouslyEmptyImportedContent(t *testing.T) {
	db, path := newSchemaDB(t)
	insertSyntheticHistory(t, db)
	if _, err := db.Exec(`INSERT INTO messages (id, chat_jid, device_id, sender, content, timestamp, is_from_me) VALUES ('synthetic-empty', '15550002000@s.whatsapp.net', '15550001000@s.whatsapp.net', '15550002000@s.whatsapp.net', '', '2026-04-04 00:00:00', 0)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	audit := path + ".migration-audit.db"
	if err := writeMigrationAudit(audit, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	raw := `{"message":{"key":{"remoteJid":"15550002000@s.whatsapp.net","fromMe":false,"id":"synthetic-empty"},"message":{"imageMessage":{"caption":"filled caption"}},"messageTimestamp":1700000011}}`
	if err := appendArchiveAudit(audit, "archive.json", "abc", "15550001000@s.whatsapp.net", "15550002000@s.whatsapp.net", "synthetic-empty", "imported", true, raw); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "history-1-15550001000@s.whatsapp.net-1-RECENT.json")
	body := `{"syncType":2,"conversations":[{"ID":"15550002000@s.whatsapp.net","messages":[
		{"message":{"key":{"remoteJid":"15550002000@s.whatsapp.net","fromMe":false,"id":"synthetic-empty"},"message":{"imageMessage":{"caption":"filled caption"}},"messageTimestamp":1700000011}},
		{"message":{"key":{"remoteJid":"15550002000@s.whatsapp.net","fromMe":false,"id":"synthetic-m1"},"message":{"conversation":"archive replacement"},"messageTimestamp":1700000012}}
	]}]}`
	if err := writeTestFile(t, archive, body); err != nil {
		t.Fatal(err)
	}
	report, err := ImportArchives(ImportRequest{DB: path, Audit: audit, Archives: []string{archive}})
	if err != nil {
		t.Fatal(err)
	}
	if report.TextFilled != 1 || report.Imported != 0 || report.HistoryComplete {
		t.Fatalf("fill %+v", report)
	}
	encoded, _ := json.Marshal(report)
	if strings.Contains(string(encoded), "filled caption") || strings.Contains(string(encoded), "archive replacement") {
		t.Fatal("fill report included text")
	}
	staged, err := sql.Open(sqlite.DriverName, path)
	if err != nil {
		t.Fatal(err)
	}
	defer staged.Close()
	var filled, original string
	if err := staged.QueryRow(`SELECT content FROM messages WHERE id='synthetic-empty'`).Scan(&filled); err != nil || filled != "filled caption" {
		t.Fatalf("filled %q %v", filled, err)
	}
	if err := staged.QueryRow(`SELECT content FROM messages WHERE id='synthetic-m1'`).Scan(&original); err != nil || original != "synthetic hello" {
		t.Fatalf("original %q %v", original, err)
	}
}

func TestAuditVerifyChecksRetentionWithoutPrintingPayloads(t *testing.T) {
	src, srcPath := newSchemaDB(t)
	insertSyntheticHistory(t, src)
	freezeClocks(t, src)
	src.Close()
	dst, dstPath := newSchemaDB(t)
	insertSyntheticHistory(t, dst)
	freezeClocks(t, dst)
	if _, err := dst.Exec(`UPDATE messages SET content='box wording' WHERE id='synthetic-m1'`); err != nil {
		t.Fatal(err)
	}
	if _, err := dst.Exec(`INSERT INTO devices (device_id, display_name, jid, ad_jid) VALUES ('synthetic-slot-b', 'synthetic b', '15550004000@s.whatsapp.net', '15550004000:1@s.whatsapp.net')`); err != nil {
		t.Fatal(err)
	}
	if _, err := dst.Exec(`INSERT INTO messages (id, chat_jid, device_id, sender, content, timestamp, is_from_me, created_at, updated_at) VALUES ('synthetic-kept', '15550002000@s.whatsapp.net', '15550004000@s.whatsapp.net', '15550002000@s.whatsapp.net', 'kept body', '2026-04-04 00:00:00', 0, '2026-01-02 03:04:05', '2026-01-02 03:04:05')`); err != nil {
		t.Fatal(err)
	}
	if _, err := dst.Exec(`INSERT INTO chats (jid, device_id, name, last_message_time) VALUES ('15550002000@s.whatsapp.net', '15550004000@s.whatsapp.net', 'kept', '2026-04-04 00:00:00')`); err != nil {
		t.Fatal(err)
	}
	dst.Close()
	srcSum, dstSum := fileSum(t, srcPath), fileSum(t, dstPath)
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
	verified, err := AuditVerify(context.Background(), AuditVerifyRequest{
		Staged: out, Source: srcPath, Target: dstPath, MapPath: mapping, Audit: overlay.AuditPath, Base: "target",
	})
	if err != nil || !verified.Verified || verified.HistoryComplete || verified.CutoverReady || !verified.RegistryMatchesBase || !verified.AuditSchemaOK {
		t.Fatalf("verify %+v %v", verified, err)
	}
	if verified.SourceRetained != 1 || verified.SourceMissing != 0 || verified.SourcePayloadMismatch != 0 || verified.TargetRetainedInAudit != 1 || verified.TargetMissing != 0 || verified.PreservedTargetDevices != 1 {
		t.Fatalf("counts %+v", verified)
	}
	if fileSum(t, srcPath) != srcSum || fileSum(t, dstPath) != dstSum {
		t.Fatal("verify changed an input")
	}
	encoded, _ := json.Marshal(verified)
	for _, hidden := range []string{"synthetic hello", "box wording", "kept body", "synthetic-webhook-secret", "synthetic-chatwoot-token"} {
		if strings.Contains(string(encoded), hidden) {
			t.Fatalf("verify report contains %s", hidden)
		}
	}
	audit, err := sql.Open(sqlite.DriverName, overlay.AuditPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := audit.Exec(`DELETE FROM message_versions`); err != nil {
		t.Fatal(err)
	}
	audit.Close()
	failed, err := AuditVerify(context.Background(), AuditVerifyRequest{
		Staged: out, Source: srcPath, Target: dstPath, MapPath: mapping, Audit: overlay.AuditPath, Base: "target",
	})
	if err == nil || failed.Verified || failed.TargetMissing == 0 {
		t.Fatalf("tampered audit %+v %v", failed, err)
	}
	encoded, _ = json.Marshal(failed)
	if strings.Contains(string(encoded), "box wording") {
		t.Fatal("failed verify printed a payload")
	}
}

func TestLegacySchemaIsMetadataOnlyAndRefusesSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "messages.db")
	db, err := sql.Open(sqlite.DriverName, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE messages (id TEXT PRIMARY KEY, body TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO messages (id, body) VALUES ('m1', 'private body')`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	report, err := InspectLegacySchema(path)
	if err != nil || report.GOWAChatStorage || report.OldBridgeAssessed || report.HistoryComplete || report.ImportFormatDefined || report.SessionRefused {
		t.Fatalf("schema %+v %v", report, err)
	}
	if len(report.Tables) != 1 || report.Tables[0].Name != "messages" || report.Tables[0].RowCount != 1 || len(report.Tables[0].Columns) != 2 {
		t.Fatalf("tables %+v", report.Tables)
	}
	encoded, _ := json.Marshal(report)
	if strings.Contains(string(encoded), "private body") {
		t.Fatal("schema report included a row")
	}
	copyPath := filepath.Join(t.TempDir(), "messages-copy.db")
	snap, err := SnapshotWithOptions(context.Background(), path, copyPath, SnapshotOptions{})
	if err != nil || !snap.LegacySchema || snap.CutoverReady || snap.HistoryComplete {
		t.Fatalf("snapshot %+v %v", snap, err)
	}
	session := filepath.Join(t.TempDir(), "store.db")
	sessionDB, err := sql.Open(sqlite.DriverName, session)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessionDB.Exec(`CREATE TABLE whatsmeow_sessions (session BLOB)`); err != nil {
		t.Fatal(err)
	}
	if _, err := sessionDB.Exec(`INSERT INTO whatsmeow_sessions (session) VALUES (x'001122')`); err != nil {
		t.Fatal(err)
	}
	sessionDB.Close()
	refused, err := InspectLegacySchema(session)
	if err == nil || refused == nil || !refused.SessionRefused || len(refused.Tables) != 0 {
		t.Fatalf("session %+v %v", refused, err)
	}
	keys := filepath.Join(t.TempDir(), "keys.db")
	keyDB, err := sql.Open(sqlite.DriverName, keys)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := keyDB.Exec(`CREATE TABLE messages (id TEXT)`); err != nil {
		t.Fatal(err)
	}
	keyDB.Close()
	refused, err = InspectLegacySchema(keys)
	if err == nil || refused == nil || !refused.SessionRefused {
		t.Fatalf("keys %+v %v", refused, err)
	}
}

func writeTestFile(t *testing.T, path, body string) error {
	t.Helper()
	return os.WriteFile(path, []byte(body), 0o600)
}
