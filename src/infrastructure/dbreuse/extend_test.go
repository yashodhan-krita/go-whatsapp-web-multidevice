package dbreuse

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aldinokemal/go-whatsapp-web-multidevice/pkg/sqlite"
)

func writeMap(t *testing.T, accounts ...AccountMapping) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "accounts.json")
	raw, err := json.Marshal(accountFile{Accounts: accounts})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func fileSum(t *testing.T, path string) [32]byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(raw)
}

func TestInstalledRevisionIsNotThisFork(t *testing.T) {
	rev := InstalledRevision()
	if rev.SHA != "07c20ff1c682398cda9b6b5c4fc8be99b3b30fa8" || rev.Parent != "be8e3a252f3d091ec86effb23adc0e0c99c9a561" {
		t.Fatalf("revision %+v", rev)
	}
	if rev.ParentTag != "v9.9.0" || rev.AppVersion != "v9.9.0" || rev.SchemaVersion != 54 {
		t.Fatalf("tree %+v", rev)
	}
	if rev.GoFilesChangedFromParent || rev.ForkBaseIsInstalled || rev.DatabaseProvesRevision || rev.HistoryComplete {
		t.Fatalf("flags %+v", rev)
	}
	if rev.ForkBaseSHA != "4c4c4ed66d5130f1ef15ea2f92e331d1be8a8fbd" || rev.KeysURI != "unknown" {
		t.Fatalf("fork/keys %+v", rev)
	}
	if rev.Observed.MeasuredByTool || rev.Observed.HistoryJSONParsed || rev.Observed.ChatstorageBytes != 57929728 || rev.Observed.ChatstorageWALBytes != 4260112 {
		t.Fatalf("observed %+v", rev.Observed)
	}
	if rev.Observed.SessionBytes != 5746688 || rev.Observed.SessionWALBytes != 4124152 || rev.Observed.HistoryJSONFiles != 8 || rev.Observed.HistoryJSONBytes != 38606270 || rev.Observed.TargetChatstorageBytes != 23818240 {
		t.Fatalf("sizes %+v", rev.Observed)
	}
	if _, err := LookupRevision("07c20ff"); err == nil {
		t.Fatal("short revision was accepted")
	}
	if _, err := LookupRevision(rev.ForkBaseSHA); err == nil || !strings.Contains(err.Error(), "fork") {
		t.Fatalf("fork base err %v", err)
	}
	if _, err := LookupRevision(strings.Repeat("ab", 20)); err == nil {
		t.Fatal("unknown revision was accepted")
	}
	got, err := LookupRevision(rev.SHA)
	if err != nil || got.HistoryComplete || got.DatabaseProvesRevision {
		t.Fatalf("lookup %+v %v", got, err)
	}
}

func TestSnapshotRefusesLiveWriterUnlessValidationIsExplicit(t *testing.T) {
	db, path := newSchemaDB(t)
	insertSyntheticHistory(t, db)
	db.Close()
	if err := os.WriteFile(path+"-shm", []byte("synthetic"), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "out.db")
	if _, err := SnapshotWithOptions(context.Background(), path, dst, SnapshotOptions{}); err == nil {
		t.Fatal("live shm was snapshotted without validation")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatal("refused snapshot created an output")
	}
	writers := RequiredQuiescedWriters()
	if _, err := SnapshotWithOptions(context.Background(), path, dst, SnapshotOptions{AttestWriters: writers}); err == nil {
		t.Fatal("freeze attestation was accepted while -shm exists")
	}
	live := filepath.Join(t.TempDir(), "live.db")
	result, err := SnapshotWithOptions(context.Background(), path, live, SnapshotOptions{AllowLiveValidation: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.CutoverReady || result.HistoryComplete || !result.LiveValidation || !result.SourceSHM {
		t.Fatalf("live result %+v", result)
	}
	os.Remove(path + "-shm")
	if _, err := SnapshotWithOptions(context.Background(), path, filepath.Join(t.TempDir(), "partial.db"), SnapshotOptions{AttestWriters: []string{"source-gowa"}}); err == nil {
		t.Fatal("partial writer list was accepted")
	}
	frozen := filepath.Join(t.TempDir(), "frozen.db")
	result, err = SnapshotWithOptions(context.Background(), path, frozen, SnapshotOptions{AttestWriters: writers})
	if err != nil {
		t.Fatal(err)
	}
	if result.CutoverReady || result.HistoryComplete || result.LiveValidation {
		t.Fatalf("frozen snapshot is not a cutover decision %+v", result)
	}
}

func TestGateBlocksUnknownSchemaAndUnverifiedRevision(t *testing.T) {
	db, path := newSchemaDB(t)
	insertSyntheticHistory(t, db)
	db.Close()
	writers := strings.Join(RequiredQuiescedWriters(), ",")
	report, err := Gate(context.Background(), GateRequest{DB: path, ExpectRevision: installedRevisionSHA, Writers: []string{writers}, Target: path, WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if report.SchemaAccepted || report.MechanicalGatesPassed || !report.RealCutoverBlocked || report.HistoryComplete {
		t.Fatalf("schema 50 gate %+v", report)
	}
	if !strings.Contains(strings.Join(report.Blockers, " "), "Real cutover is blocked") {
		t.Fatalf("blockers %v", report.Blockers)
	}
	joined := strings.Join(report.StandingLimits, " ")
	for _, phrase := range []string{"DB_KEYS_URI is unknown", "history_complete is false", "whatsapp.db was not opened"} {
		if !strings.Contains(joined, phrase) {
			t.Fatalf("standing %v", report.StandingLimits)
		}
	}

	db54, path54 := newSchemaDB(t)
	insertSyntheticHistory(t, db54)
	applyOfficialV990Tail(t, db54)
	db54.Close()
	same, samePath := newSchemaDB(t)
	insertSyntheticHistory(t, same)
	applyOfficialV990Tail(t, same)
	same.Close()
	okReport, err := Gate(context.Background(), GateRequest{DB: path54, ExpectRevision: installedRevisionSHA, Writers: []string{writers}, Target: samePath, WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if !okReport.MechanicalGatesPassed || !okReport.SchemaAccepted || !okReport.RevisionVerified || !okReport.WritersQuiesced || !okReport.RealCutoverBlocked || okReport.HistoryComplete {
		t.Fatalf("mechanical gate %+v", okReport)
	}
	if okReport.Revision == nil || okReport.Revision.DatabaseProvesRevision || okReport.Revision.ForkBaseIsInstalled {
		t.Fatalf("revision %+v", okReport.Revision)
	}

	fork, err := Gate(context.Background(), GateRequest{DB: path54, ExpectRevision: forkBaseSHA, Writers: []string{writers}, Target: samePath, WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if fork.RevisionVerified || fork.MechanicalGatesPassed || !strings.Contains(strings.Join(fork.Blockers, " "), "fork") {
		t.Fatalf("fork gate %+v", fork)
	}

	extraDB, extraPath := newSchemaDB(t)
	if _, err := extraDB.Exec(`ALTER TABLE messages ADD COLUMN mac_extra TEXT`); err != nil {
		t.Fatal(err)
	}
	extraDB.Close()
	extra, err := Gate(context.Background(), GateRequest{DB: extraPath, ExpectRevision: installedRevisionSHA, Writers: []string{writers}})
	if err != nil {
		t.Fatal(err)
	}
	if extra.SchemaAccepted || extra.MechanicalGatesPassed {
		t.Fatalf("extra column %+v", extra)
	}

	if err := os.WriteFile(path54+"-shm", []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	live, err := Gate(context.Background(), GateRequest{DB: path54, ExpectRevision: installedRevisionSHA, Writers: []string{writers}, Target: samePath, WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if live.WritersQuiesced || live.MechanicalGatesPassed {
		t.Fatalf("shm gate %+v", live)
	}

	session := filepath.Join(t.TempDir(), "whatsapp.db")
	raw, err := os.ReadFile(path54)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(session, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Gate(context.Background(), GateRequest{DB: session, ExpectRevision: installedRevisionSHA, Writers: []string{writers}}); err == nil {
		t.Fatal("gate opened whatsapp.db")
	}
}

func TestGateReportsTargetOnlyWithoutMerging(t *testing.T) {
	src, srcPath := newSchemaDB(t)
	insertSyntheticHistory(t, src)
	applyOfficialV990Tail(t, src)
	src.Close()
	dst, dstPath := newSchemaDB(t)
	insertSyntheticHistory(t, dst)
	applyOfficialV990Tail(t, dst)
	if _, err := dst.Exec(`INSERT INTO messages (id, chat_jid, device_id, sender, content, timestamp) VALUES ('synthetic-target-only', '15550002000@s.whatsapp.net', '15550001000@s.whatsapp.net', '15550002000@s.whatsapp.net', 'newer on target', '2026-03-03 00:00:00')`); err != nil {
		t.Fatal(err)
	}
	dst.Close()
	before := fileSum(t, dstPath)
	report, err := Gate(context.Background(), GateRequest{
		DB: srcPath, ExpectRevision: installedRevisionSHA, Writers: []string{strings.Join(RequiredQuiescedWriters(), ",")},
		Target: dstPath, WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.TargetOnly != 1 || report.MechanicalGatesPassed || !report.RealCutoverBlocked {
		t.Fatalf("gate %+v", report)
	}
	if fileSum(t, dstPath) != before {
		t.Fatal("gate modified the target")
	}
}

func TestOverlayCopiesTargetOnlyAndPreservesOriginals(t *testing.T) {
	src, srcPath := newSchemaDB(t)
	insertSyntheticHistory(t, src)
	src.Close()
	dst, dstPath := newSchemaDB(t)
	insertSyntheticHistory(t, dst)
	if _, err := dst.Exec(`UPDATE chats SET name='renamed on target' WHERE jid='15550002000@s.whatsapp.net'`); err != nil {
		t.Fatal(err)
	}
	if _, err := dst.Exec(`INSERT INTO messages (id, chat_jid, device_id, sender, content, timestamp, is_from_me) VALUES ('synthetic-target-only', '15550002000@s.whatsapp.net', '15550001000@s.whatsapp.net', '15550002000@s.whatsapp.net', 'newer on target', '2026-03-03 00:00:00', 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := dst.Exec(`INSERT INTO message_reactions (message_id, chat_jid, device_id, reactor_jid, emoji, reaction_timestamp) VALUES ('synthetic-target-only', '15550002000@s.whatsapp.net', '15550001000@s.whatsapp.net', '15550002000@s.whatsapp.net', 'new', '2026-03-03 00:01:00')`); err != nil {
		t.Fatal(err)
	}
	dst.Close()
	srcSum := fileSum(t, srcPath)
	dstSum := fileSum(t, dstPath)
	mapping := writeMap(t, AccountMapping{
		Label: "synthetic-a", Phone: "15550001000",
		SourceStorageJID: "15550001000@s.whatsapp.net", TargetStorageJID: "15550001000@s.whatsapp.net",
	})
	out := filepath.Join(t.TempDir(), "staged.db")
	report, err := Overlay(context.Background(), OverlayRequest{
		Source: srcPath, Target: dstPath, MapPath: mapping, WorkDir: t.TempDir(), Out: out,
	})
	if err != nil {
		t.Fatalf("overlay: %v report=%+v", err, report)
	}
	if !report.Applied || report.CutoverReady || report.HistoryComplete || len(report.InsertedMessages) != 1 || report.InsertedMessages[0].ID != "synthetic-target-only" {
		t.Fatalf("report %+v", report)
	}
	if report.InsertedReactions != 1 {
		t.Fatalf("reactions %d", report.InsertedReactions)
	}
	if len(report.ChatDifferences) == 0 {
		t.Fatal("chat rename was dropped without a report")
	}
	encoded, _ := json.Marshal(report)
	for _, secret := range []string{"synthetic-webhook-secret", "synthetic-chatwoot-token", "newer on target"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("report contains %s", secret)
		}
	}
	if fileSum(t, srcPath) != srcSum || fileSum(t, dstPath) != dstSum {
		t.Fatal("overlay modified an original database")
	}
	staged, err := sql.Open(sqlite.DriverName, out)
	if err != nil {
		t.Fatal(err)
	}
	defer staged.Close()
	var device, chat, content string
	if err := staged.QueryRow(`SELECT device_id, chat_jid, content FROM messages WHERE id='synthetic-target-only'`).Scan(&device, &chat, &content); err != nil {
		t.Fatal(err)
	}
	if device != "15550001000@s.whatsapp.net" || chat != "15550002000@s.whatsapp.net" || content != "newer on target" {
		t.Fatalf("staged %s %s %s", device, chat, content)
	}
	var name, ad string
	if err := staged.QueryRow(`SELECT name FROM chats WHERE jid='15550002000@s.whatsapp.net'`).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "synthetic peer" {
		t.Fatalf("source chat name was overwritten: %s", name)
	}
	if err := staged.QueryRow(`SELECT ad_jid FROM devices WHERE device_id='synthetic-slot'`).Scan(&ad); err != nil {
		t.Fatal(err)
	}
	if ad != "15550001000:18@s.whatsapp.net" {
		t.Fatalf("registry changed %s", ad)
	}
	var attempts int
	if err := staged.QueryRow(`SELECT attempts FROM scheduled_sends WHERE id='synthetic-tried'`).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("schedule attempts %d", attempts)
	}

	againPath := filepath.Join(t.TempDir(), "again.db")
	again, err := Overlay(context.Background(), OverlayRequest{
		Source: out, Target: dstPath, MapPath: mapping, WorkDir: filepath.Join(t.TempDir(), "work2"), Out: againPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !again.Applied || len(again.InsertedMessages) != 0 || again.AlreadyPresent < 2 || again.HistoryComplete {
		t.Fatalf("rerun %+v", again)
	}
}

func TestOverlayRejectsIdentityConflictsAndKeepsOriginals(t *testing.T) {
	src, srcPath := newSchemaDB(t)
	insertSyntheticHistory(t, src)
	src.Close()
	dst, dstPath := newSchemaDB(t)
	insertSyntheticHistory(t, dst)
	if _, err := dst.Exec(`UPDATE messages SET content='changed on target' WHERE id='synthetic-m1'`); err != nil {
		t.Fatal(err)
	}
	dst.Close()
	before := fileSum(t, srcPath)
	mapping := writeMap(t, AccountMapping{
		Label: "synthetic-a", Phone: "15550001000",
		SourceStorageJID: "15550001000@s.whatsapp.net", TargetStorageJID: "15550001000@s.whatsapp.net",
	})
	out := filepath.Join(t.TempDir(), "kept.db")
	report, err := Overlay(context.Background(), OverlayRequest{Source: srcPath, Target: dstPath, MapPath: mapping, WorkDir: t.TempDir(), Out: out})
	if err != nil || !report.Applied || report.HistoryComplete || report.CutoverReady || report.AliasInferred {
		t.Fatalf("conflict publish %+v %v", report, err)
	}
	if len(report.Conflicts) != 1 || report.Conflicts[0].ID != "synthetic-m1" || report.Conflicts[0].Class != "genuine_content" || report.DivergentRetained != 1 {
		t.Fatalf("conflict %+v", report)
	}
	if fileSum(t, srcPath) != before {
		t.Fatal("conflict modified the source")
	}
	encoded, _ := json.Marshal(report)
	if strings.Contains(string(encoded), "changed on target") || strings.Contains(string(encoded), "synthetic hello") {
		t.Fatal("conflict report included message text")
	}
	kept, err := sql.Open(sqlite.DriverName, out)
	if err != nil {
		t.Fatal(err)
	}
	var stagedContent string
	if err := kept.QueryRow(`SELECT content FROM messages WHERE id='synthetic-m1'`).Scan(&stagedContent); err != nil {
		t.Fatal(err)
	}
	kept.Close()
	if stagedContent != "synthetic hello" {
		t.Fatalf("primary content %q", stagedContent)
	}
	hasAlternate, err := rowsInAudit(report.AuditPath, "changed on target")
	if err != nil || !hasAlternate {
		t.Fatalf("audit alternate %v", err)
	}
	hasPrimary, err := rowsInAudit(report.AuditPath, "synthetic hello")
	if err != nil || !hasPrimary {
		t.Fatalf("audit primary %v", err)
	}

	slotMap := writeMap(t, AccountMapping{Label: "synthetic-a", Phone: "15550001000", SourceStorageJID: "synthetic-slot", TargetStorageJID: "15550001000@s.whatsapp.net"})
	slotReport, err := Overlay(context.Background(), OverlayRequest{Source: srcPath, Target: dstPath, MapPath: slotMap, WorkDir: filepath.Join(t.TempDir(), "slot"), Out: filepath.Join(t.TempDir(), "slot.db")})
	if !errors.Is(err, ErrRejected) || slotReport.Applied || !strings.Contains(strings.Join(slotReport.Reasons, " "), "slot") {
		t.Fatalf("slot %+v %v", slotReport, err)
	}

	badDB, badPath := newSchemaDB(t)
	insertSyntheticHistory(t, badDB)
	if _, err := badDB.Exec(`UPDATE devices SET ad_jid='15550009999:2@s.whatsapp.net' WHERE device_id='synthetic-slot'`); err != nil {
		t.Fatal(err)
	}
	badDB.Close()
	badReport, err := Overlay(context.Background(), OverlayRequest{Source: srcPath, Target: badPath, MapPath: mapping, WorkDir: filepath.Join(t.TempDir(), "bad"), Out: filepath.Join(t.TempDir(), "bad.db")})
	if !errors.Is(err, ErrRejected) || badReport.Applied {
		t.Fatalf("phone mismatch %+v %v", badReport.Reasons, err)
	}

	ambDB, ambPath := newSchemaDB(t)
	insertSyntheticHistory(t, ambDB)
	if _, err := ambDB.Exec(`INSERT INTO chats (jid, device_id, name, last_message_time) VALUES ('15550003000@s.whatsapp.net', '15550001000@s.whatsapp.net', 'other', '2026-04-04 00:00:00')`); err != nil {
		t.Fatal(err)
	}
	if _, err := ambDB.Exec(`INSERT INTO messages (id, chat_jid, device_id, sender, content, timestamp) VALUES ('synthetic-m1', '15550003000@s.whatsapp.net', '15550001000@s.whatsapp.net', '15550003000@s.whatsapp.net', 'other chat', '2026-04-04 00:00:00')`); err != nil {
		t.Fatal(err)
	}
	ambDB.Close()
	ambOut := filepath.Join(t.TempDir(), "amb.db")
	amb, err := Overlay(context.Background(), OverlayRequest{Source: srcPath, Target: ambPath, MapPath: mapping, WorkDir: filepath.Join(t.TempDir(), "amb"), Out: ambOut})
	if err != nil || !amb.Applied || len(amb.Ambiguous) != 1 || amb.AliasInferred || amb.HistoryComplete {
		t.Fatalf("ambiguous %+v %v", amb, err)
	}
	ambDBCheck, err := sql.Open(sqlite.DriverName, ambOut)
	if err != nil {
		t.Fatal(err)
	}
	var distinct int
	if err := ambDBCheck.QueryRow(`SELECT COUNT(*) FROM messages WHERE id='synthetic-m1'`).Scan(&distinct); err != nil {
		t.Fatal(err)
	}
	ambDBCheck.Close()
	if distinct != 2 {
		t.Fatalf("distinct chats %d", distinct)
	}

	otherDB, otherPath := newSchemaDB(t)
	insertSyntheticHistory(t, otherDB)
	if _, err := otherDB.Exec(`INSERT INTO devices (device_id, jid, ad_jid) VALUES ('other-slot', '15550004000@s.whatsapp.net', '15550004000:3@s.whatsapp.net')`); err != nil {
		t.Fatal(err)
	}
	if _, err := otherDB.Exec(`INSERT INTO messages (id, chat_jid, device_id, sender, content, timestamp) VALUES ('synthetic-other', '15550002000@s.whatsapp.net', '15550004000@s.whatsapp.net', '15550002000@s.whatsapp.net', 'other account', '2026-05-05 00:00:00')`); err != nil {
		t.Fatal(err)
	}
	otherDB.Close()
	other, err := Overlay(context.Background(), OverlayRequest{Source: srcPath, Target: otherPath, MapPath: mapping, WorkDir: filepath.Join(t.TempDir(), "other"), Out: filepath.Join(t.TempDir(), "other.db")})
	if !errors.Is(err, ErrRejected) || other.Applied || len(other.UnmappedDeviceIDs) != 1 {
		t.Fatalf("unmapped %+v %v", other, err)
	}

	raw := append([]byte(nil), mustRead(t, mapping)...)
	raw = bytes.Replace(raw, []byte(`}`), []byte(`,"api_token":"synthetic-token"}`), 1)
	tokenMap := filepath.Join(t.TempDir(), "token.json")
	if err := os.WriteFile(tokenMap, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Overlay(context.Background(), OverlayRequest{Source: srcPath, Target: dstPath, MapPath: tokenMap, WorkDir: filepath.Join(t.TempDir(), "token"), Out: filepath.Join(t.TempDir(), "token.db")}); err == nil {
		t.Fatal("unknown map field was accepted")
	}
}

func TestOverlayRewritesDeviceIDForVerifiedLID(t *testing.T) {
	src, srcPath := newSchemaDB(t)
	if _, err := src.Exec(`INSERT INTO devices (device_id, jid, ad_jid) VALUES ('slot-src', '999001@lid', '15550001000:4@s.whatsapp.net')`); err != nil {
		t.Fatal(err)
	}
	if _, err := src.Exec(`INSERT INTO chats (jid, device_id, name, last_message_time) VALUES ('15550002000@s.whatsapp.net', '999001@lid', 'synthetic peer', '2026-01-02 03:04:05')`); err != nil {
		t.Fatal(err)
	}
	if _, err := src.Exec(`INSERT INTO messages (id, chat_jid, device_id, sender, content, timestamp) VALUES ('synthetic-src', '15550002000@s.whatsapp.net', '999001@lid', '15550002000@s.whatsapp.net', 'from mac', '2026-01-02 03:04:05')`); err != nil {
		t.Fatal(err)
	}
	src.Close()
	dst, dstPath := newSchemaDB(t)
	if _, err := dst.Exec(`INSERT INTO devices (device_id, jid, ad_jid) VALUES ('slot-dst', '15550001000@s.whatsapp.net', '15550001000:18@s.whatsapp.net')`); err != nil {
		t.Fatal(err)
	}
	if _, err := dst.Exec(`INSERT INTO chats (jid, device_id, name, last_message_time) VALUES ('15550002000@s.whatsapp.net', '15550001000@s.whatsapp.net', 'synthetic peer', '2026-03-03 00:00:00')`); err != nil {
		t.Fatal(err)
	}
	if _, err := dst.Exec(`INSERT INTO messages (id, chat_jid, device_id, sender, content, timestamp) VALUES ('synthetic-new', '15550002000@s.whatsapp.net', '15550001000@s.whatsapp.net', '15550002000@s.whatsapp.net', 'from linux', '2026-03-03 00:00:00')`); err != nil {
		t.Fatal(err)
	}
	dst.Close()
	mapping := writeMap(t, AccountMapping{
		Label: "synthetic-a", Phone: "15550001000", LID: "999001",
		SourceStorageJID: "999001@lid", TargetStorageJID: "15550001000@s.whatsapp.net",
	})
	out := filepath.Join(t.TempDir(), "lid.db")
	report, err := Overlay(context.Background(), OverlayRequest{Source: srcPath, Target: dstPath, MapPath: mapping, WorkDir: t.TempDir(), Out: out})
	if err != nil {
		t.Fatalf("%v %+v", err, report)
	}
	db, err := sql.Open(sqlite.DriverName, out)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var device, chat string
	if err := db.QueryRow(`SELECT device_id, chat_jid FROM messages WHERE id='synthetic-new'`).Scan(&device, &chat); err != nil {
		t.Fatal(err)
	}
	if device != "999001@lid" || chat != "15550002000@s.whatsapp.net" {
		t.Fatalf("rewritten %s %s", device, chat)
	}
	var slots int
	if err := db.QueryRow(`SELECT COUNT(*) FROM devices`).Scan(&slots); err != nil {
		t.Fatal(err)
	}
	if slots != 1 {
		t.Fatalf("registry rows %d", slots)
	}
}

func TestCoverageReportsArchiveIDsWithoutImporting(t *testing.T) {
	db, path := newSchemaDB(t)
	insertSyntheticHistory(t, db)
	db.Close()
	before := fileSum(t, path)
	archive := filepath.Join(t.TempDir(), "history-synthetic.json")
	body := []byte(`{"syncType":"recent","conversations":[{"id":"15550002000@s.whatsapp.net","messages":[{"message":{"key":{"remoteJid":"15550002000@s.whatsapp.net","fromMe":false,"id":"synthetic-m1"},"messageTimestamp":1735689845}},{"message":{"key":{"remoteJid":"15550002000@s.whatsapp.net","fromMe":false,"id":"synthetic-absent"}}}]}]}`)
	if err := os.WriteFile(archive, body, 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := Coverage(path, []string{archive})
	if err != nil {
		t.Fatal(err)
	}
	if report.HistoryComplete || len(report.Archives) != 1 || report.Archives[0].Present != 1 || report.Archives[0].Absent != 1 {
		t.Fatalf("coverage %+v", report)
	}
	if report.Archives[0].AbsentIDs[0].ID != "synthetic-absent" {
		t.Fatalf("absent %+v", report.Archives[0].AbsentIDs)
	}
	again, err := Coverage(path, []string{archive})
	if err != nil || again.Archives[0].Present != 1 || again.Archives[0].Absent != 1 || again.HistoryComplete {
		t.Fatalf("rerun %+v %v", again, err)
	}
	check, err := sql.Open(sqlite.DriverName, path)
	if err != nil {
		t.Fatal(err)
	}
	var count int
	if err := check.QueryRow(`SELECT COUNT(*) FROM messages WHERE id='synthetic-absent'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	check.Close()
	if count != 0 || fileSum(t, path) != before {
		t.Fatal("coverage inserted or modified the snapshot")
	}

	full := filepath.Join(t.TempDir(), "complete.json")
	if err := os.WriteFile(full, []byte(`{"conversations":[{"id":"15550002000@s.whatsapp.net","messages":[{"message":{"key":{"remoteJid":"15550002000@s.whatsapp.net","id":"synthetic-m1"}}}]}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	fullReport, err := Coverage(path, []string{full})
	if err != nil || fullReport.HistoryComplete || fullReport.Archives[0].Absent != 0 || fullReport.Archives[0].Present != 1 {
		t.Fatalf("full overlap %+v %v", fullReport, err)
	}

	other := filepath.Join(t.TempDir(), "other-chat.json")
	if err := os.WriteFile(other, []byte(`{"conversations":[{"id":"15550009999@s.whatsapp.net","messages":[{"message":{"key":{"remoteJid":"15550009999@s.whatsapp.net","id":"synthetic-m1"}}}]}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	otherReport, err := Coverage(path, []string{other})
	if err != nil || otherReport.Archives[0].OtherChat != 1 || otherReport.Archives[0].Present != 0 || otherReport.HistoryComplete {
		t.Fatalf("other chat %+v %v", otherReport, err)
	}

	bad := filepath.Join(t.TempDir(), "notes.json")
	if err := os.WriteFile(bad, []byte(`{"note":"not history"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Coverage(path, []string{bad}); err == nil {
		t.Fatal("unrecognized archive was accepted")
	}
	gzipPath := filepath.Join(t.TempDir(), "history.json.gz")
	if err := os.WriteFile(gzipPath, []byte{0x1f, 0x8b, 0x08, 0x00}, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Coverage(path, []string{gzipPath}); err == nil {
		t.Fatal("gzip archive was accepted")
	}
	sqliteArchive := filepath.Join(t.TempDir(), "history.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sqliteArchive, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Coverage(path, []string{sqliteArchive}); err == nil {
		t.Fatal("sqlite archive was accepted")
	}
}

func TestOfflineCLIDoesNotClaimCompleteHistory(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := runOffline(&out, &errBuf, []string{"--help"}); code != 0 || !strings.Contains(out.String(), "snapshot") || !strings.Contains(out.String(), "does not connect to WhatsApp") {
		t.Fatalf("help %d %s", code, out.String())
	}
	db, path := newSchemaDB(t)
	insertSyntheticHistory(t, db)
	db.Close()
	out.Reset()
	errBuf.Reset()
	if code := runOffline(&out, &errBuf, []string{"preflight", "--db", path}); code != 0 {
		t.Fatalf("preflight %d %s %s", code, out.String(), errBuf.String())
	}
	if !strings.Contains(out.String(), `"history_complete": false`) {
		t.Fatalf("preflight json %s", out.String())
	}
	out.Reset()
	if code := runOffline(&out, &errBuf, []string{"revision"}); code != 0 || !strings.Contains(out.String(), installedRevisionSHA) || strings.Contains(out.String(), `"history_complete": true`) {
		t.Fatalf("revision %d %s", code, out.String())
	}
}

func freezeClocks(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, query := range []string{
		`UPDATE messages SET created_at='2026-01-02 03:04:05', updated_at='2026-01-02 03:04:05'`,
		`UPDATE message_reactions SET created_at='2026-01-02 03:04:05', updated_at='2026-01-02 03:04:05'`,
		`UPDATE message_edits SET created_at='2026-01-02 03:04:05'`,
		`UPDATE poll_definitions SET created_at='2026-01-02 03:04:05', updated_at='2026-01-02 03:04:05'`,
	} {
		if _, err := db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOverlayTargetBaseKeepsUnmappedAccount(t *testing.T) {
	src, srcPath := newSchemaDB(t)
	insertSyntheticHistory(t, src)
	freezeClocks(t, src)
	if _, err := src.Exec(`INSERT INTO messages (id, chat_jid, device_id, sender, content, timestamp, is_from_me, created_at, updated_at) VALUES ('synthetic-mac-only', '15550002000@s.whatsapp.net', '15550001000@s.whatsapp.net', '15550002000@s.whatsapp.net', 'from mac', '2022-06-01 00:00:00', 0, '2026-01-02 03:04:05', '2026-01-02 03:04:05')`); err != nil {
		t.Fatal(err)
	}
	src.Close()
	dst, dstPath := newSchemaDB(t)
	insertSyntheticHistory(t, dst)
	freezeClocks(t, dst)
	if _, err := dst.Exec(`INSERT INTO devices (device_id, display_name, jid, ad_jid) VALUES ('synthetic-slot-b', 'synthetic b', '15550004000@s.whatsapp.net', '15550004000:1@s.whatsapp.net')`); err != nil {
		t.Fatal(err)
	}
	if _, err := dst.Exec(`INSERT INTO chats (jid, device_id, name, last_message_time) VALUES ('15550002000@s.whatsapp.net', '15550004000@s.whatsapp.net', 'kept account', '2026-04-04 00:00:00')`); err != nil {
		t.Fatal(err)
	}
	if _, err := dst.Exec(`INSERT INTO messages (id, chat_jid, device_id, sender, content, timestamp, is_from_me, created_at, updated_at) VALUES ('synthetic-box-only', '15550002000@s.whatsapp.net', '15550004000@s.whatsapp.net', '15550002000@s.whatsapp.net', 'kept on box', '2026-04-04 00:00:00', 0, '2026-01-02 03:04:05', '2026-01-02 03:04:05')`); err != nil {
		t.Fatal(err)
	}
	if _, err := dst.Exec(`INSERT INTO messages (id, chat_jid, device_id, sender, content, timestamp, is_from_me, created_at, updated_at) VALUES ('synthetic-box-newer', '15550002000@s.whatsapp.net', '15550001000@s.whatsapp.net', '15550002000@s.whatsapp.net', 'newer on box', '2026-05-05 00:00:00', 0, '2026-01-02 03:04:05', '2026-01-02 03:04:05')`); err != nil {
		t.Fatal(err)
	}
	dst.Close()
	srcSum, dstSum := fileSum(t, srcPath), fileSum(t, dstPath)
	mapping := writeMap(t, AccountMapping{
		Label: "synthetic-a", Phone: "15550001000",
		SourceStorageJID: "15550001000@s.whatsapp.net", TargetStorageJID: "15550001000@s.whatsapp.net",
	})
	out := filepath.Join(t.TempDir(), "box-base.db")
	report, err := Overlay(context.Background(), OverlayRequest{
		Source: srcPath, Target: dstPath, MapPath: mapping, WorkDir: t.TempDir(), Out: out, Base: "target",
	})
	if err != nil {
		t.Fatalf("box base: %v %+v", err, report)
	}
	if !report.Applied || report.Base != "target" || report.HistoryComplete || report.CutoverReady {
		t.Fatalf("report %+v", report)
	}
	if len(report.PreservedBaseDeviceIDs) != 1 || report.PreservedBaseDeviceIDs[0] != "15550004000@s.whatsapp.net" {
		t.Fatalf("preserved %+v", report.PreservedBaseDeviceIDs)
	}
	if len(report.InsertedMessages) != 1 || report.InsertedMessages[0].ID != "synthetic-mac-only" || report.InsertedMessages[0].DeviceID != "15550001000@s.whatsapp.net" {
		t.Fatalf("inserted %+v", report.InsertedMessages)
	}
	encoded, _ := json.Marshal(report)
	for _, secret := range []string{"from mac", "kept on box", "newer on box", "synthetic-webhook-secret"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("report contains %s", secret)
		}
	}
	if fileSum(t, srcPath) != srcSum || fileSum(t, dstPath) != dstSum {
		t.Fatal("box base modified an original")
	}
	staged, err := sql.Open(sqlite.DriverName, out)
	if err != nil {
		t.Fatal(err)
	}
	defer staged.Close()
	var devices int
	if err := staged.QueryRow(`SELECT COUNT(*) FROM devices`).Scan(&devices); err != nil {
		t.Fatal(err)
	}
	if devices != 2 {
		t.Fatalf("devices %d", devices)
	}
	var kept, newer string
	if err := staged.QueryRow(`SELECT content FROM messages WHERE id='synthetic-box-only'`).Scan(&kept); err != nil || kept != "kept on box" {
		t.Fatalf("kept %q %v", kept, err)
	}
	if err := staged.QueryRow(`SELECT content FROM messages WHERE id='synthetic-box-newer'`).Scan(&newer); err != nil || newer != "newer on box" {
		t.Fatalf("newer %q %v", newer, err)
	}
}

func TestOverlayClassifiesMetadataWithoutPublishingContentConflicts(t *testing.T) {
	src, srcPath := newSchemaDB(t)
	insertSyntheticHistory(t, src)
	freezeClocks(t, src)
	src.Close()
	dst, dstPath := newSchemaDB(t)
	insertSyntheticHistory(t, dst)
	freezeClocks(t, dst)
	if _, err := dst.Exec(`UPDATE messages SET timestamp='2026-06-01 00:00:00', filename='synthetic-display-name' WHERE id='synthetic-m1'`); err != nil {
		t.Fatal(err)
	}
	dst.Close()
	mapping := writeMap(t, AccountMapping{
		Label: "synthetic-a", Phone: "15550001000",
		SourceStorageJID: "15550001000@s.whatsapp.net", TargetStorageJID: "15550001000@s.whatsapp.net",
	})
	out := filepath.Join(t.TempDir(), "enriched.db")
	report, err := Overlay(context.Background(), OverlayRequest{
		Source: srcPath, Target: dstPath, MapPath: mapping, WorkDir: t.TempDir(), Out: out,
	})
	if err != nil || !report.Applied || len(report.Conflicts) != 0 || report.EnrichmentFills != 1 {
		t.Fatalf("enrichment %+v %v", report, err)
	}
	encoded, _ := json.Marshal(report)
	if strings.Contains(string(encoded), "synthetic-display-name") || strings.Contains(string(encoded), "synthetic hello") {
		t.Fatal("enrichment report included a filename or body")
	}
	var sawTimestamp, sawFill bool
	for _, diff := range report.ColumnDiffs {
		if diff.Column == "timestamp" && diff.Class == "timestamp" && diff.Resolution == "kept_base" && diff.BaseTimestamp != "" && diff.IncomingTimestamp != "" {
			sawTimestamp = true
		}
		if diff.Column == "filename" && diff.Class == "enrichment" && diff.Resolution == "filled_from_incoming" {
			sawFill = true
		}
	}
	if !sawTimestamp || !sawFill {
		t.Fatalf("diffs %+v", report.ColumnDiffs)
	}
	staged, err := sql.Open(sqlite.DriverName, out)
	if err != nil {
		t.Fatal(err)
	}
	defer staged.Close()
	var stamp any
	var name string
	if err := staged.QueryRow(`SELECT timestamp, filename FROM messages WHERE id='synthetic-m1'`).Scan(&stamp, &name); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fmt.Sprint(stamp), "2026-01-02") || name != "synthetic-display-name" {
		t.Fatalf("staged timestamp %q filename %q", stamp, name)
	}

	dst2, dst2Path := newSchemaDB(t)
	insertSyntheticHistory(t, dst2)
	freezeClocks(t, dst2)
	if _, err := dst2.Exec(`UPDATE messages SET is_from_me=1 WHERE id='synthetic-m1'`); err != nil {
		t.Fatal(err)
	}
	dst2.Close()
	blocked := filepath.Join(t.TempDir(), "blocked.db")
	blockedReport, err := Overlay(context.Background(), OverlayRequest{
		Source: srcPath, Target: dst2Path, MapPath: mapping, WorkDir: filepath.Join(t.TempDir(), "block"), Out: blocked,
	})
	if err != nil || !blockedReport.Applied || blockedReport.Conflicts[0].Class != "genuine_content" || blockedReport.HistoryComplete {
		t.Fatalf("sender flag %+v %v", blockedReport, err)
	}
	flagDB, err := sql.Open(sqlite.DriverName, blocked)
	if err != nil {
		t.Fatal(err)
	}
	var fromMe any
	if err := flagDB.QueryRow(`SELECT is_from_me FROM messages WHERE id='synthetic-m1'`).Scan(&fromMe); err != nil {
		t.Fatal(err)
	}
	flagDB.Close()
	if fmt.Sprint(fromMe) == "1" || fmt.Sprint(fromMe) == "true" {
		t.Fatalf("source is_from_me %v", fromMe)
	}

	dst3, dst3Path := newSchemaDB(t)
	insertSyntheticHistory(t, dst3)
	freezeClocks(t, dst3)
	if _, err := dst3.Exec(`UPDATE messages SET content='synthetic edited' WHERE id='synthetic-m1'`); err != nil {
		t.Fatal(err)
	}
	if _, err := dst3.Exec(`UPDATE message_edits SET previous_content='synthetic hello', new_content='synthetic edited' WHERE original_message_id='synthetic-m1'`); err != nil {
		t.Fatal(err)
	}
	dst3.Close()
	srcDB, err := sql.Open(sqlite.DriverName, srcPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srcDB.Exec(`UPDATE message_edits SET previous_content='synthetic hello', new_content='synthetic edited' WHERE original_message_id='synthetic-m1'`); err != nil {
		t.Fatal(err)
	}
	srcDB.Close()
	editOut := filepath.Join(t.TempDir(), "edit.db")
	editReport, err := Overlay(context.Background(), OverlayRequest{
		Source: srcPath, Target: dst3Path, MapPath: mapping, WorkDir: filepath.Join(t.TempDir(), "edit"), Out: editOut,
	})
	if err != nil || !editReport.Applied || len(editReport.Conflicts) != 1 || editReport.Conflicts[0].Class != "edit_divergence" {
		t.Fatalf("edit %+v %v", editReport, err)
	}
	editJSON, _ := json.Marshal(editReport)
	if strings.Contains(string(editJSON), "synthetic edited") || strings.Contains(string(editJSON), "synthetic hello") {
		t.Fatal("edit report included message text")
	}
	keptEdit, err := rowsInAudit(editReport.AuditPath, "synthetic edited")
	if err != nil || !keptEdit {
		t.Fatalf("edit audit %v", err)
	}
	editDB, err := sql.Open(sqlite.DriverName, editOut)
	if err != nil {
		t.Fatal(err)
	}
	var editContent string
	if err := editDB.QueryRow(`SELECT content FROM messages WHERE id='synthetic-m1'`).Scan(&editContent); err != nil {
		t.Fatal(err)
	}
	editDB.Close()
	if editContent != "synthetic hello" {
		t.Fatalf("edit primary %q", editContent)
	}
}

func TestCoverageAcceptsEmptyAndMetadataArchives(t *testing.T) {
	db, path := newSchemaDB(t)
	insertSyntheticHistory(t, db)
	db.Close()
	write := func(name, body string) string {
		t.Helper()
		file := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return file
	}
	empty := write("empty.json", `{"syncType":3,"conversations":[{"ID":"15550002000@s.whatsapp.net","participant":[]},{"ID":"15550003000@s.whatsapp.net"}],"recentStickers":[],"pastParticipants":[],"callLogRecords":[]}`)
	emptyReport, err := Coverage(path, []string{empty})
	if err != nil || emptyReport.HistoryComplete || emptyReport.OldBridgeAssessed {
		t.Fatalf("empty %+v %v", emptyReport, err)
	}
	if emptyReport.Archives[0].Kind != "messages" || !emptyReport.Archives[0].MessageAudit || emptyReport.Archives[0].Conversations != 2 || emptyReport.Archives[0].EmptyConversations != 2 || emptyReport.Archives[0].Messages != 0 {
		t.Fatalf("empty archive %+v", emptyReport.Archives[0])
	}
	mixed := write("mixed.json", `{"syncType":2,"chunkOrder":1,"progress":1,"conversations":[{"ID":"15550002000@s.whatsapp.net","endOfHistoryTransferType":0},{"ID":"15550002000@s.whatsapp.net","messages":[{"message":{"key":{"remoteJid":"15550002000@s.whatsapp.net","id":"synthetic-absent"}}},{"message":{"key":{"remoteJid":"15550002000@s.whatsapp.net","id":"synthetic-m1"}}}]}],"phoneNumberToLidMappings":[]}`)
	mixedReport, err := Coverage(path, []string{mixed})
	if err != nil || mixedReport.HistoryComplete || mixedReport.Archives[0].EmptyConversations != 1 || mixedReport.Archives[0].Present != 1 || mixedReport.Archives[0].Absent != 1 {
		t.Fatalf("mixed %+v %v", mixedReport, err)
	}
	if mixedReport.Archives[0].AbsentIDs[0].ID != "synthetic-absent" {
		t.Fatalf("absent %+v", mixedReport.Archives[0].AbsentIDs)
	}
	for _, item := range []struct {
		name string
		body string
	}{
		{"status.json", `{"syncType":1,"statusV3Messages":[],"chunkOrder":1,"phoneNumberToLidMappings":[]}`},
		{"push.json", `{"syncType":4,"chunkOrder":1,"pushnames":[{"id":"synthetic-push"}]}`},
		{"progress.json", `{"syncType":2,"chunkOrder":1,"progress":1}`},
	} {
		report, err := Coverage(path, []string{write(item.name, item.body)})
		if err != nil || report.HistoryComplete || report.Archives[0].Kind != "metadata_only" || report.Archives[0].MessageAudit || report.Archives[0].Messages != 0 {
			t.Fatalf("%s %+v %v", item.name, report, err)
		}
	}
	if _, err := Coverage(path, []string{write("unknown.json", `{"syncType":3,"conversations":[],"notAField":true}`)}); err == nil {
		t.Fatal("unknown top-level key was accepted")
	}
}

func TestMediaReferencesFailClosed(t *testing.T) {
	db, path := newSchemaDB(t)
	insertSyntheticHistory(t, db)
	if _, err := db.Exec(`UPDATE messages SET media_type='image', filename='synthetic-display', file_sha256=X'010203', media_key=X'0a0b0c' WHERE id='synthetic-m1'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO messages (id, chat_jid, device_id, sender, content, timestamp, filename) VALUES ('synthetic-dup', '15550002000@s.whatsapp.net', '15550001000@s.whatsapp.net', '15550002000@s.whatsapp.net', 'other', '2026-02-02 00:00:00', 'synthetic-display')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO messages (id, chat_jid, device_id, sender, content, timestamp, filename) VALUES ('synthetic-path', '15550002000@s.whatsapp.net', '15550001000@s.whatsapp.net', '15550002000@s.whatsapp.net', 'path', '2026-02-03 00:00:00', 'synthetic/path.pdf')`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	archive := filepath.Join(t.TempDir(), "status.json")
	if err := os.WriteFile(archive, []byte(`{"syncType":1,"statusV3Messages":[],"chunkOrder":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := MediaReferences(MediaRefRequest{DB: path, Archives: []string{archive}})
	if err != nil {
		t.Fatal(err)
	}
	if report.SubsetProven || report.WholeTreeCopyAllowed || report.WholeTreeFits || report.PurgeAllowed || report.HistoryComplete || report.OldBridgeAssessed || report.LocalPathStored || report.BytesSelected != nil {
		t.Fatalf("flags %+v", report)
	}
	if report.AmbiguousDisplayNameGroups != 1 || report.PathShapedFilenames != 1 || report.RowsWithFilename < 2 || report.MetadataOnlyArchives != 1 || report.ArchiveLocalPaths != 0 {
		t.Fatalf("counts %+v", report)
	}
	if report.Inventory.MeasuredByTool || report.Inventory.Files != 490698 || report.Inventory.LogicalGB != "129.21" || report.Inventory.AllocatedGB != "130.48" || report.Inventory.BoxFreeGB != "49.21" {
		t.Fatalf("inventory %+v", report.Inventory)
	}
	if len(report.NextAdapterFields) < 6 {
		t.Fatalf("evidence %+v", report.NextAdapterFields)
	}
	encoded, _ := json.Marshal(report)
	for _, hidden := range []string{"synthetic-display", "synthetic/path.pdf", "010203", "0a0b0c", "synthetic hello"} {
		if strings.Contains(string(encoded), hidden) {
			t.Fatalf("report contains %s", hidden)
		}
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "synthetic-display"), []byte("bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := MediaReferences(MediaRefRequest{DB: path, MediaRoot: root}); err == nil || !strings.Contains(err.Error(), "display") {
		t.Fatalf("media root err %v", err)
	}
	var out, errBuf bytes.Buffer
	if code := runOffline(&out, &errBuf, []string{"media-refs", "--db", path}); code != 0 || !strings.Contains(out.String(), `"subset_proven": false`) || strings.Contains(out.String(), `"history_complete": true`) {
		t.Fatalf("cli %d %s %s", code, out.String(), errBuf.String())
	}
}

func TestOverlayKeepsOrphanReactionAndSourcePrimaryOnTargetBase(t *testing.T) {
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
	if _, err := dst.Exec(`INSERT INTO message_reactions (message_id, chat_jid, device_id, reactor_jid, emoji, reaction_timestamp) VALUES ('synthetic-missing', '15550002000@s.whatsapp.net', '15550001000@s.whatsapp.net', '15550002000@s.whatsapp.net', 'synthetic-emoji', '2026-03-03 00:01:00')`); err != nil {
		t.Fatal(err)
	}
	dst.Close()
	mapping := writeMap(t, AccountMapping{
		Label: "synthetic-a", Phone: "15550001000",
		SourceStorageJID: "15550001000@s.whatsapp.net", TargetStorageJID: "15550001000@s.whatsapp.net",
	})
	out := filepath.Join(t.TempDir(), "union.db")
	report, err := Overlay(context.Background(), OverlayRequest{
		Source: srcPath, Target: dstPath, MapPath: mapping, WorkDir: t.TempDir(), Out: out, Base: "target",
	})
	if err != nil || !report.Applied || report.AliasInferred || report.HistoryComplete {
		t.Fatalf("union %+v %v", report, err)
	}
	encoded, _ := json.Marshal(report)
	if strings.Contains(string(encoded), "box wording") || strings.Contains(string(encoded), "synthetic-emoji") || strings.Contains(string(encoded), "synthetic hello") {
		t.Fatal("public report included a payload")
	}
	var orphan OrphanRef
	for _, item := range report.Orphans {
		if item.MessageID == "synthetic-missing" {
			orphan = item
		}
	}
	if orphan.Table != "message_reactions" || orphan.ParentInvented || !orphan.RetainedInStagedDB {
		t.Fatalf("orphan %+v", report.Orphans)
	}
	staged, err := sql.Open(sqlite.DriverName, out)
	if err != nil {
		t.Fatal(err)
	}
	var content string
	var reactions int
	var invented int
	if err := staged.QueryRow(`SELECT content FROM messages WHERE id='synthetic-m1'`).Scan(&content); err != nil {
		t.Fatal(err)
	}
	if err := staged.QueryRow(`SELECT COUNT(*) FROM message_reactions WHERE message_id='synthetic-missing'`).Scan(&reactions); err != nil {
		t.Fatal(err)
	}
	if err := staged.QueryRow(`SELECT COUNT(*) FROM messages WHERE id='synthetic-missing'`).Scan(&invented); err != nil {
		t.Fatal(err)
	}
	staged.Close()
	if content != "synthetic hello" || reactions != 1 || invented != 0 {
		t.Fatalf("content %q reactions %d invented %d", content, reactions, invented)
	}
	kept, err := rowsInAudit(report.AuditPath, "box wording")
	if err != nil || !kept {
		t.Fatalf("audit %v", err)
	}
}

func TestChildUniqueKeysKeepBothChatsWithoutRelinking(t *testing.T) {
	db, _ := newSchemaDB(t)
	constraints := uniqueColumns(t, db, "message_reactions")
	if !sameStringSet(constraints, []string{"message_id", "reactor_jid", "device_id"}) {
		t.Fatalf("reaction unique key %v", constraints)
	}
	if len(foreignKeyColumns(t, db, "message_reactions").from) != 0 {
		t.Fatal("reaction foreign keys changed")
	}
	editKey := uniqueColumns(t, db, "message_edits")
	if !sameStringSet(editKey, []string{"original_message_id", "edit_event_id", "device_id"}) {
		t.Fatalf("edit unique key %v", editKey)
	}
	editFK := foreignKeyColumns(t, db, "message_edits")
	if !sameStringSet(editFK.from, []string{"original_message_id", "chat_jid", "device_id"}) || editFK.refTable != "messages" || !sameStringSet(editFK.to, []string{"id", "chat_jid", "device_id"}) {
		t.Fatalf("edit fk %+v", editFK)
	}
	if !sameStringSet(uniqueColumns(t, db, "poll_definitions"), []string{"device_id", "chat_jid", "poll_message_id"}) {
		t.Fatal("poll key changed")
	}
	if !sameStringSet(uniqueColumns(t, db, "messages"), []string{"id", "chat_jid", "device_id"}) {
		t.Fatal("message key changed")
	}
	if len(foreignKeyColumns(t, db, "messages").from) != 0 || len(foreignKeyColumns(t, db, "poll_definitions").from) != 0 {
		t.Fatal("message or poll foreign keys changed")
	}
	if !sameStringSet(uniqueColumns(t, db, "chats"), []string{"jid", "device_id"}) || len(foreignKeyColumns(t, db, "chats").from) != 0 {
		t.Fatal("chat key changed")
	}
	db.Close()

	src, srcPath := newSchemaDB(t)
	if _, err := src.Exec(`INSERT INTO devices (device_id, display_name, jid, ad_jid) VALUES ('slot-src', 'synthetic', '999001@lid', '15550001000:4@s.whatsapp.net')`); err != nil {
		t.Fatal(err)
	}
	if _, err := src.Exec(`INSERT INTO chats (jid, device_id, name, last_message_time) VALUES ('15550002000@s.whatsapp.net', '999001@lid', 'synthetic peer', '2026-01-02 03:04:05'), ('15550003000@s.whatsapp.net', '999001@lid', 'second chat', '2026-04-04 00:00:00')`); err != nil {
		t.Fatal(err)
	}
	if _, err := src.Exec(`INSERT INTO messages (id, chat_jid, device_id, sender, content, timestamp, is_from_me) VALUES ('synthetic-m1', '15550003000@s.whatsapp.net', '999001@lid', '15550003000@s.whatsapp.net', 'other occurrence', '2026-04-04 00:00:00', 0), ('synthetic-m2', '15550002000@s.whatsapp.net', '999001@lid', '15550002000@s.whatsapp.net', 'same chat', '2026-04-05 00:00:00', 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := src.Exec(`INSERT INTO message_reactions (message_id, chat_jid, device_id, reactor_jid, emoji, reaction_timestamp) VALUES ('synthetic-m1', '15550003000@s.whatsapp.net', '999001@lid', '15550002000@s.whatsapp.net', 'other-chat-emoji', '2026-04-04 00:01:00'), ('synthetic-m2', '15550002000@s.whatsapp.net', '999001@lid', '15550002000@s.whatsapp.net', 'source-emoji', '2026-04-05 00:01:00')`); err != nil {
		t.Fatal(err)
	}
	if _, err := src.Exec(`INSERT INTO message_edits (original_message_id, edit_event_id, chat_jid, device_id, edited_at, previous_content, new_content) VALUES ('synthetic-m1', 'synthetic-e1', '15550003000@s.whatsapp.net', '999001@lid', '2026-04-04 00:02:00', 'left', 'right'), ('synthetic-missing-parent', 'synthetic-e-orphan', '15550002000@s.whatsapp.net', '999001@lid', '2026-04-04 00:03:00', 'orphan-left', 'orphan-right')`); err != nil {
		t.Fatal(err)
	}
	if _, err := src.Exec(`INSERT INTO poll_definitions (device_id, chat_jid, poll_message_id, question) VALUES ('999001@lid', '15550003000@s.whatsapp.net', 'synthetic-m1', 'other poll'), ('999001@lid', '15550002000@s.whatsapp.net', 'synthetic-m2', 'source question')`); err != nil {
		t.Fatal(err)
	}
	src.Close()

	dst, dstPath := newSchemaDB(t)
	insertSyntheticHistory(t, dst)
	freezeClocks(t, dst)
	if _, err := dst.Exec(`INSERT INTO devices (device_id, display_name, jid, ad_jid) VALUES ('synthetic-slot-b', 'synthetic b', '15550004000@s.whatsapp.net', '15550004000:1@s.whatsapp.net')`); err != nil {
		t.Fatal(err)
	}
	if _, err := dst.Exec(`INSERT INTO chats (jid, device_id, name, last_message_time) VALUES ('15550002000@s.whatsapp.net', '15550004000@s.whatsapp.net', 'kept account', '2026-04-04 00:00:00')`); err != nil {
		t.Fatal(err)
	}
	if _, err := dst.Exec(`INSERT INTO messages (id, chat_jid, device_id, sender, content, timestamp, is_from_me, created_at, updated_at) VALUES ('synthetic-kept', '15550002000@s.whatsapp.net', '15550004000@s.whatsapp.net', '15550002000@s.whatsapp.net', 'kept body', '2026-04-04 00:00:00', 0, '2026-01-02 03:04:05', '2026-01-02 03:04:05')`); err != nil {
		t.Fatal(err)
	}
	if _, err := dst.Exec(`INSERT INTO message_reactions (message_id, chat_jid, device_id, reactor_jid, emoji, reaction_timestamp, created_at, updated_at) VALUES ('synthetic-kept', '15550002000@s.whatsapp.net', '15550004000@s.whatsapp.net', '15550002000@s.whatsapp.net', 'kept-account', '2026-04-04 00:01:00', '2026-01-02 03:04:05', '2026-01-02 03:04:05')`); err != nil {
		t.Fatal(err)
	}
	if _, err := dst.Exec(`INSERT INTO messages (id, chat_jid, device_id, sender, content, timestamp, is_from_me, created_at, updated_at) VALUES ('synthetic-m2', '15550002000@s.whatsapp.net', '15550001000@s.whatsapp.net', '15550002000@s.whatsapp.net', 'same chat', '2026-04-05 00:00:00', 0, '2026-01-02 03:04:05', '2026-01-02 03:04:05')`); err != nil {
		t.Fatal(err)
	}
	if _, err := dst.Exec(`INSERT INTO message_reactions (message_id, chat_jid, device_id, reactor_jid, emoji, reaction_timestamp, created_at, updated_at) VALUES ('synthetic-m2', '15550002000@s.whatsapp.net', '15550001000@s.whatsapp.net', '15550002000@s.whatsapp.net', 'base-emoji', '2026-04-05 00:01:00', '2026-01-02 03:04:05', '2026-01-02 03:04:05')`); err != nil {
		t.Fatal(err)
	}
	if _, err := dst.Exec(`INSERT INTO poll_definitions (device_id, chat_jid, poll_message_id, question, created_at, updated_at) VALUES ('15550001000@s.whatsapp.net', '15550002000@s.whatsapp.net', 'synthetic-m2', 'base question', '2026-01-02 03:04:05', '2026-01-02 03:04:05')`); err != nil {
		t.Fatal(err)
	}
	dst.Close()
	srcSum, dstSum := fileSum(t, srcPath), fileSum(t, dstPath)
	mapping := writeMap(t, AccountMapping{
		Label: "synthetic-a", Phone: "15550001000", LID: "999001",
		SourceStorageJID: "999001@lid", TargetStorageJID: "15550001000@s.whatsapp.net",
	})
	out := filepath.Join(t.TempDir(), "union.db")
	report, err := Overlay(context.Background(), OverlayRequest{
		Source: srcPath, Target: dstPath, MapPath: mapping, WorkDir: t.TempDir(), Out: out, Base: "target",
	})
	if err != nil || !report.Applied || report.HistoryComplete || report.AliasInferred {
		t.Fatalf("overlay %+v %v", report, err)
	}
	if fileSum(t, srcPath) != srcSum || fileSum(t, dstPath) != dstSum {
		t.Fatal("originals changed")
	}
	encoded, _ := json.Marshal(report)
	for _, hidden := range []string{"source-emoji", "other-chat-emoji", "other-chat body", "orphan-right", "source question", "kept body"} {
		if strings.Contains(string(encoded), hidden) {
			t.Fatalf("public report contains %s", hidden)
		}
	}
	var reactionHits, editHits int
	for _, collision := range report.KeyCollisions {
		if collision.Relinked {
			t.Fatalf("relinked %+v", collision)
		}
		if collision.Table == "message_reactions" && collision.MessageID == "synthetic-m1" {
			reactionHits++
			if !sameStringSet(collision.KeyColumns, []string{"message_id", "reactor_jid", "device_id"}) {
				t.Fatalf("collision key %v", collision.KeyColumns)
			}
		}
		if collision.Table == "message_edits" && collision.OtherChatJID == "15550003000@s.whatsapp.net" {
			editHits++
		}
	}
	if reactionHits != 1 || editHits != 1 {
		t.Fatalf("collisions %+v", report.KeyCollisions)
	}
	staged, err := sql.Open(sqlite.DriverName, out)
	if err != nil {
		t.Fatal(err)
	}
	defer staged.Close()
	var emoji, chat string
	var reactionCount, editCount, pollCount, messageCount, orphanEdits, invented, kept int
	var question, keptEmoji string
	if err := staged.QueryRow(`SELECT COUNT(*), emoji, chat_jid FROM message_reactions WHERE message_id='synthetic-m1' AND reactor_jid='15550002000@s.whatsapp.net' AND device_id='15550001000@s.whatsapp.net'`).Scan(&reactionCount, &emoji, &chat); err != nil {
		t.Fatal(err)
	}
	if reactionCount != 1 || emoji != "ok" || chat != "15550002000@s.whatsapp.net" {
		t.Fatalf("reaction count %d emoji %q chat %q", reactionCount, emoji, chat)
	}
	var sameChatEmoji string
	if err := staged.QueryRow(`SELECT emoji FROM message_reactions WHERE message_id='synthetic-m2' AND device_id='15550001000@s.whatsapp.net'`).Scan(&sameChatEmoji); err != nil || sameChatEmoji != "source-emoji" {
		t.Fatalf("same-chat emoji %q %v", sameChatEmoji, err)
	}
	if err := staged.QueryRow(`SELECT COUNT(*) FROM message_edits WHERE original_message_id='synthetic-m1' AND edit_event_id='synthetic-e1' AND device_id='15550001000@s.whatsapp.net' AND chat_jid='15550002000@s.whatsapp.net'`).Scan(&editCount); err != nil || editCount != 1 {
		t.Fatalf("edit %d %v", editCount, err)
	}
	if err := staged.QueryRow(`SELECT COUNT(*) FROM message_edits WHERE original_message_id='synthetic-missing-parent'`).Scan(&orphanEdits); err != nil || orphanEdits != 0 {
		t.Fatalf("orphan edit %d %v", orphanEdits, err)
	}
	if err := staged.QueryRow(`SELECT COUNT(*) FROM messages WHERE id='synthetic-missing-parent'`).Scan(&invented); err != nil || invented != 0 {
		t.Fatalf("invented parent %d %v", invented, err)
	}
	if err := staged.QueryRow(`SELECT COUNT(*) FROM poll_definitions WHERE poll_message_id='synthetic-m1' AND device_id='15550001000@s.whatsapp.net'`).Scan(&pollCount); err != nil || pollCount != 2 {
		t.Fatalf("polls %d %v", pollCount, err)
	}
	if err := staged.QueryRow(`SELECT question FROM poll_definitions WHERE poll_message_id='synthetic-m2' AND chat_jid='15550002000@s.whatsapp.net'`).Scan(&question); err != nil || question != "source question" {
		t.Fatalf("question %q %v", question, err)
	}
	if err := staged.QueryRow(`SELECT COUNT(*) FROM messages WHERE id='synthetic-m1' AND device_id='15550001000@s.whatsapp.net'`).Scan(&messageCount); err != nil || messageCount != 2 {
		t.Fatalf("messages %d %v", messageCount, err)
	}
	if err := staged.QueryRow(`SELECT emoji FROM message_reactions WHERE message_id='synthetic-kept'`).Scan(&keptEmoji); err != nil || keptEmoji != "kept-account" {
		t.Fatalf("preserved reaction %q %v", keptEmoji, err)
	}
	if err := staged.QueryRow(`SELECT COUNT(*) FROM messages WHERE id='synthetic-kept'`).Scan(&kept); err != nil || kept != 1 {
		t.Fatalf("preserved message %d %v", kept, err)
	}
	for _, needle := range []string{"other-chat-emoji", "right", "orphan-right"} {
		ok, err := rowsInAudit(report.AuditPath, needle)
		if err != nil || !ok {
			t.Fatalf("audit missing %s (%v)", needle, err)
		}
	}
}

type foreignKey struct {
	from     []string
	to       []string
	refTable string
}

func uniqueColumns(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
	rows, err := db.Query(fmt.Sprintf(`PRAGMA index_list(%s)`, table))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var uniqueName string
	for rows.Next() {
		var seq, unique, partial int
		var name, origin string
		if err := rows.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
			t.Fatal(err)
		}
		if unique == 1 && origin == "pk" {
			uniqueName = name
		}
	}
	if uniqueName == "" {
		t.Fatalf("%s has no primary key", table)
	}
	info, err := db.Query(fmt.Sprintf(`PRAGMA index_info(%s)`, uniqueName))
	if err != nil {
		t.Fatal(err)
	}
	defer info.Close()
	var cols []string
	for info.Next() {
		var seqno, cid int
		var col string
		if err := info.Scan(&seqno, &cid, &col); err != nil {
			t.Fatal(err)
		}
		cols = append(cols, col)
	}
	return cols
}

func foreignKeyColumns(t *testing.T, db *sql.DB, table string) foreignKey {
	t.Helper()
	rows, err := db.Query(fmt.Sprintf(`PRAGMA foreign_key_list(%s)`, table))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var fk foreignKey
	for rows.Next() {
		var id, seq int
		var ref, from, to, onUpdate, onDelete, match string
		if err := rows.Scan(&id, &seq, &ref, &from, &to, &onUpdate, &onDelete, &match); err != nil {
			t.Fatal(err)
		}
		fk.refTable = ref
		fk.from = append(fk.from, from)
		fk.to = append(fk.to, to)
	}
	return fk
}

func TestImportArchiveInsertsAbsentMessagesOnly(t *testing.T) {
	db, path := newSchemaDB(t)
	insertSyntheticHistory(t, db)
	db.Close()
	write := func(name, body string) string {
		t.Helper()
		file := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return file
	}
	present := write("history-1-15550001000@s.whatsapp.net-1-RECENT.json", `{"syncType":2,"conversations":[{"ID":"15550002000@s.whatsapp.net","messages":[{"message":{"key":{"remoteJid":"15550002000@s.whatsapp.net","fromMe":false,"id":"synthetic-m1"},"message":{"conversation":"synthetic archive body"},"messageTimestamp":1700000000}},{"message":{"key":{"remoteJid":"15550002000@s.whatsapp.net","fromMe":false,"id":"synthetic-absent"},"message":{"conversation":"absent body"},"messageTimestamp":1700000002}}]},{"ID":"15550009999@s.whatsapp.net","messages":[{"message":{"key":{"remoteJid":"15550009999@s.whatsapp.net","fromMe":false,"id":"synthetic-m1"},"message":{"conversation":"other chat body"},"messageTimestamp":1700000001}}]},{"ID":"120363000@g.us","messages":[{"message":{"key":{"remoteJid":"120363000@g.us","id":"synthetic-group"},"message":{"conversation":"group body"},"messageTimestamp":1700000003}}]}]}`)
	meta := write("history-1-15550001000@s.whatsapp.net-2-PUSH_NAME.json", `{"syncType":4,"chunkOrder":1,"pushnames":[{"id":"synthetic"}]}`)
	before := fileSum(t, path)
	wrong := write("history-1-15550009999@s.whatsapp.net-1-RECENT.json", `{"syncType":2,"conversations":[{"ID":"15550002000@s.whatsapp.net","messages":[{"message":{"key":{"remoteJid":"15550002000@s.whatsapp.net","id":"synthetic-nope"},"message":{"conversation":"nope"},"messageTimestamp":1700000004}}]}]}`)
	if _, err := ImportArchives(ImportRequest{DB: path, Archives: []string{wrong}}); err == nil {
		t.Fatal("unscoped archive was imported")
	}
	if fileSum(t, path) != before {
		t.Fatal("refused import modified the database")
	}
	report, err := ImportArchives(ImportRequest{DB: path, Archives: []string{present, meta}})
	if err != nil {
		t.Fatal(err)
	}
	if report.HistoryComplete || report.OldBridgeAssessed || report.MetadataOnlyIsProof || report.Account != "15550001000@s.whatsapp.net" {
		t.Fatalf("import %+v", report)
	}
	if report.Imported != 2 || report.AlreadyPresent != 1 || report.DistinctOtherChat != 1 || report.SkippedMetadataOnly != 1 || report.SenderUnresolved != 1 || report.TextExtracted != 2 {
		t.Fatalf("counts %+v", report)
	}
	encoded, _ := json.Marshal(report)
	if strings.Contains(string(encoded), "absent body") || strings.Contains(string(encoded), "other chat body") || strings.Contains(string(encoded), "group body") {
		t.Fatal("import report included message text")
	}
	staged, err := sql.Open(sqlite.DriverName, path)
	if err != nil {
		t.Fatal(err)
	}
	var absent, original, other string
	var group int
	if err := staged.QueryRow(`SELECT content FROM messages WHERE id='synthetic-absent'`).Scan(&absent); err != nil || absent != "absent body" {
		t.Fatalf("absent %q %v", absent, err)
	}
	if err := staged.QueryRow(`SELECT content FROM messages WHERE id='synthetic-m1' AND chat_jid='15550002000@s.whatsapp.net'`).Scan(&original); err != nil || original != "synthetic hello" {
		t.Fatalf("original %q %v", original, err)
	}
	if err := staged.QueryRow(`SELECT content FROM messages WHERE id='synthetic-m1' AND chat_jid='15550009999@s.whatsapp.net'`).Scan(&other); err != nil || other != "other chat body" {
		t.Fatalf("other %q %v", other, err)
	}
	if err := staged.QueryRow(`SELECT COUNT(*) FROM messages WHERE id='synthetic-group'`).Scan(&group); err != nil || group != 0 {
		t.Fatalf("group %d %v", group, err)
	}
	staged.Close()
	kept, err := rowsInAudit(report.AuditPath, "group body")
	if err != nil || !kept {
		t.Fatalf("group audit %v", err)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
