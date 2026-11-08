package dbreuse

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// LegacyAccount declares one legacy SQLite file and the registry storage JID
// it belongs to. The file has no device identity. The hash is the caller's
// provenance for that exact file.
type LegacyAccount struct {
	Label            string `json:"label"`
	SourcePath       string `json:"source_path"`
	SourceSHA256     string `json:"source_sha256"`
	TargetStorageJID string `json:"target_storage_jid"`
}

// LegacyRequest reads declared legacy files. Apply appends missing messages
// into the staged GOWA database and writes the private audit. It does not
// modify the legacy files.
type LegacyRequest struct {
	Staged  string
	MapPath string
	Audit   string
	Apply   bool
}

// UnsupportedClass counts a legacy table that is not copied into GOWA history.
type UnsupportedClass struct {
	Table           string `json:"table"`
	Rows            int    `json:"rows"`
	RetainedInAudit bool   `json:"retained_in_audit"`
	Reason          string `json:"reason"`
}

// LegacyAccountReport is the coverage of one declared file. It has counts and
// column classes, not message bodies, filenames, or secrets.
type LegacyAccountReport struct {
	Label                 string             `json:"label"`
	SourceSHA256          string             `json:"source_sha256"`
	TargetStorageJID      string             `json:"target_storage_jid"`
	Messages              int                `json:"messages"`
	Chats                 int                `json:"chats"`
	TimestampMin          string             `json:"timestamp_min,omitempty"`
	TimestampMax          string             `json:"timestamp_max,omitempty"`
	Present               int                `json:"present"`
	Absent                int                `json:"absent"`
	AlreadySame           int                `json:"already_same"`
	Conflict              int                `json:"conflict"`
	Inserted              int                `json:"inserted"`
	ChatsInserted         int                `json:"chats_inserted"`
	SenderUnresolved      int                `json:"sender_unresolved"`
	TimestampUnresolved   int                `json:"timestamp_unresolved"`
	FlagUnresolved        int                `json:"flag_unresolved"`
	ZoneUnspecified       int                `json:"zone_unspecified"`
	BroadcastOrNewsletter int                `json:"broadcast_or_newsletter"`
	MediaFilename         int                `json:"media_filename"`
	MediaURL              int                `json:"media_url"`
	MediaDirectPath       int                `json:"media_direct_path"`
	MediaKey              int                `json:"media_key"`
	ColumnClasses         []ColumnClassCount `json:"column_classes"`
	Unsupported           []UnsupportedClass `json:"unsupported"`
}

// LegacyReport is safe to print.
type LegacyReport struct {
	StagedSHA256      string                `json:"staged_sha256"`
	AuditPath         string                `json:"audit_path,omitempty"`
	Applied           bool                  `json:"applied"`
	FilesRead         int                   `json:"files_read"`
	Accounts          []LegacyAccountReport `json:"accounts"`
	Limitations       []string              `json:"limitations"`
	MediaBytesCopied  bool                  `json:"media_bytes_copied"`
	SourcesUnchanged  bool                  `json:"sources_unchanged"`
	OldBridgeAssessed bool                  `json:"old_bridge_assessed"`
	HistoryComplete   bool                  `json:"history_complete"`
	CutoverReady      bool                  `json:"cutover_ready"`
	Notes             []string              `json:"notes"`
}

var (
	legacyMessageColumns = []string{
		"id", "chat_jid", "sender", "sender_name", "content", "timestamp", "is_from_me",
		"media_type", "filename", "url", "media_key", "file_sha256", "file_enc_sha256", "file_length",
		"direct_path", "quoted_message_id", "quoted_sender_name", "quoted_text_preview",
		"reply_to_message_id", "edit_count", "is_edited", "is_forwarded", "forwarded_from",
		"is_system_message", "system_message_type",
	}
	legacyChatColumns     = []string{"jid", "name", "last_message_time"}
	legacyNicknameColumns = []string{"jid", "nickname", "created_at", "updated_at"}
	legacyVoteColumns     = []string{"poll_message_id", "chat_jid", "voter_jid", "selected_options", "vote_timestamp_ms", "updated_at"}
	legacyOptionColumns   = []string{"message_id", "chat_jid", "option_index", "option_name"}
	legacyWebhookConfigs  = []string{"id", "name", "webhook_url", "secret_token", "enabled", "created_at", "updated_at"}
	legacyWebhookLogs     = []string{"id", "webhook_config_id", "message_id", "chat_jid", "trigger_type", "trigger_value", "payload", "response_status", "response_body", "attempt_count", "delivered_at", "created_at"}
	legacyWebhookTriggers = []string{"id", "webhook_config_id", "trigger_type", "trigger_value", "match_type", "enabled"}
	legacyProjected       = []string{"content", "sender", "timestamp", "is_from_me", "media_type", "filename", "url", "direct_path", "media_key", "file_sha256", "file_enc_sha256", "file_length"}
)

// LegacyCoverage reads the declared files and compares them to the staged
// database. It does not write.
func LegacyCoverage(_ context.Context, req LegacyRequest) (*LegacyReport, error) {
	req.Apply = false
	return runLegacy(req)
}

// LegacyImport appends legacy messages that are absent for the declared
// account, chat, and id. An existing GOWA row is left in place. The full
// legacy row is stored in the private audit.
func LegacyImport(_ context.Context, req LegacyRequest) (*LegacyReport, error) {
	req.Apply = true
	return runLegacy(req)
}

func runLegacy(req LegacyRequest) (*LegacyReport, error) {
	report := &LegacyReport{
		Accounts:          []LegacyAccountReport{},
		Limitations:       []string{},
		MediaBytesCopied:  false,
		SourcesUnchanged:  true,
		OldBridgeAssessed: false,
		HistoryComplete:   false,
		CutoverReady:      false,
		Notes: []string{
			"The legacy file has no device id. The account is only the storage JID declared in the map, and only when the file hash matches.",
			"Message identity is account, chat, and id. A broadcast or newsletter chat is kept. No alias is guessed.",
			"An existing GOWA row stays the primary. The full legacy row, including columns GOWA does not have, is stored in the private audit.",
			"A group, broadcast, or newsletter message with an empty sender is not inserted. No sender is invented.",
			"Media filename, url, direct_path, key, and hashes are copied onto a newly inserted row and kept in the audit. Media bytes are not copied.",
			"Webhook tables are counted and not read. Poll options, votes, and nicknames are not turned into GOWA fields they do not have.",
			"These declared files are not full history. history_complete and cutover_ready stay false. This does not authorize a purge.",
		},
	}
	if req.Staged == "" || req.MapPath == "" {
		return report, fmt.Errorf("legacy coverage requires --db and --map")
	}
	accounts, err := loadLegacyMap(req.MapPath)
	if err != nil {
		return report, err
	}
	checked, err := Check(req.Staged)
	if err != nil {
		return report, err
	}
	if checked == nil || !checked.Reusable {
		return report, fmt.Errorf("legacy import refused the staged snapshot")
	}
	seenAccount := map[string]struct{}{}
	for _, account := range accounts {
		if _, ok := seenAccount[account.TargetStorageJID]; ok {
			return report, fmt.Errorf("legacy map declares %s more than once", account.TargetStorageJID)
		}
		seenAccount[account.TargetStorageJID] = struct{}{}
		if !exactStorageID(account.TargetStorageJID, checked.Registry) {
			return report, fmt.Errorf("target storage JID %s is not exactly one registry storage id", account.TargetStorageJID)
		}
	}
	sum, err := hashFile(req.Staged)
	if err != nil {
		return report, err
	}
	report.StagedSHA256 = sum
	staged, err := openSQLite(req.Staged, !req.Apply)
	if err != nil {
		return report, err
	}
	defer staged.Close()
	stagedRows, err := readHistory(staged, "messages")
	if err != nil {
		return report, err
	}
	stagedByKey := map[string]sqlRow{}
	for _, row := range stagedRows {
		key, err := messageIdentity(row, "")
		if err != nil {
			return report, err
		}
		stagedByKey[key] = row
	}
	chatHave := map[string]struct{}{}
	chats, err := readHistory(staged, "chats")
	if err != nil {
		return report, err
	}
	for _, row := range chats {
		jid, err := rowString(row, "jid")
		if err != nil {
			return report, err
		}
		device, err := rowString(row, "device_id")
		if err != nil {
			return report, err
		}
		chatHave[device+"\x00"+jid] = struct{}{}
	}
	var inserts []plannedLegacyInsert
	var newChats []plannedChat
	var auditRows []legacyAuditRow
	for _, account := range accounts {
		before, err := hashFile(account.SourcePath)
		if err != nil {
			return report, err
		}
		if !strings.EqualFold(before, account.SourceSHA256) {
			return report, fmt.Errorf("legacy file hash does not match the declared provenance")
		}
		src, err := openSQLite(account.SourcePath, true)
		if err != nil {
			return report, err
		}
		item, extra, err := assessLegacyAccount(src, account, stagedByKey)
		src.Close()
		if err != nil {
			return report, err
		}
		after, err := hashFile(account.SourcePath)
		if err != nil {
			return report, err
		}
		if after != before {
			report.SourcesUnchanged = false
			return report, fmt.Errorf("legacy source changed while it was read")
		}
		report.FilesRead++
		if item.SenderUnresolved > 0 {
			report.Limitations = append(report.Limitations, fmt.Sprintf("%s has %d messages with no supported sender. They were not inserted and no sender was invented. The raw rows are retained in the private audit.", account.Label, item.SenderUnresolved))
		}
		if !req.Apply {
			for i := range item.Unsupported {
				if item.Unsupported[i].RetainedInAudit {
					item.Unsupported[i].RetainedInAudit = false
				}
			}
		}
		item.Inserted = len(extra.inserts)
		for _, chat := range extra.chats {
			if _, ok := chatHave[account.TargetStorageJID+"\x00"+chat.jid]; ok {
				continue
			}
			item.ChatsInserted++
			if req.Apply {
				newChats = append(newChats, plannedChat{account: account.TargetStorageJID, jid: chat.jid, name: chat.name, stamp: chat.stamp})
				chatHave[account.TargetStorageJID+"\x00"+chat.jid] = struct{}{}
			}
		}
		if req.Apply {
			inserts = append(inserts, extra.inserts...)
			auditRows = append(auditRows, extra.audit...)
		}
		report.Accounts = append(report.Accounts, item)
	}
	if !req.Apply {
		return report, nil
	}
	if err := applyLegacy(staged, inserts, newChats); err != nil {
		return report, err
	}
	auditPath := req.Audit
	if auditPath == "" {
		auditPath = auditPathFor(req.Staged)
	}
	report.AuditPath = auditPath
	if err := writeLegacyAudit(auditPath, auditRows); err != nil {
		return report, err
	}
	report.Applied = true
	sum, err = hashFile(req.Staged)
	if err != nil {
		return report, err
	}
	report.StagedSHA256 = sum
	return report, nil
}

type plannedChat struct {
	account, jid, name, stamp string
}

type legacyChatPlan struct {
	jid, name, stamp string
}

type legacyExtra struct {
	inserts []plannedLegacyInsert
	chats   []legacyChatPlan
	audit   []legacyAuditRow
}

type plannedLegacyInsert struct {
	account string
	row     sqlRow
	sender  string
	stamp   string
	fromMe  int
}

type legacyAuditRow struct {
	sha, account, table, chat, id, reason string
	imported                              bool
	row                                   sqlRow
}

func assessLegacyAccount(src *sql.DB, account LegacyAccount, staged map[string]sqlRow) (LegacyAccountReport, legacyExtra, error) {
	item := LegacyAccountReport{
		Label: account.Label, SourceSHA256: account.SourceSHA256, TargetStorageJID: account.TargetStorageJID,
		ColumnClasses: []ColumnClassCount{}, Unsupported: []UnsupportedClass{},
	}
	var extra legacyExtra
	if err := requireLegacyShape(src); err != nil {
		return item, extra, err
	}
	messages, err := loadLegacyRows(src, "messages", legacyMessageColumns)
	if err != nil {
		return item, extra, err
	}
	chats, err := loadLegacyRows(src, "chats", legacyChatColumns)
	if err != nil {
		return item, extra, err
	}
	item.Messages = len(messages)
	item.Chats = len(chats)
	classes := map[string]int{}
	chatTime := map[string]string{}
	chatName := map[string]string{}
	for _, chat := range chats {
		jid := rowText(chat, "jid")
		name := rowText(chat, "name")
		chatName[jid] = name
		if stored, class, ok := legacyStoredTimestamp(fieldOrNil(chat, "last_message_time")); ok {
			chatTime[jid] = stored
			if class == "zone_unspecified" {
				item.ZoneUnspecified++
			}
		}
	}
	messageMax := map[string]string{}
	for _, row := range messages {
		chat := rowText(row, "chat_jid")
		id := rowText(row, "id")
		if nonEmptyField(row, "filename") {
			item.MediaFilename++
		}
		if nonEmptyField(row, "url") {
			item.MediaURL++
		}
		if nonEmptyField(row, "direct_path") {
			item.MediaDirectPath++
		}
		if nonEmptyField(row, "media_key") {
			item.MediaKey++
		}
		server := chatServer(chat)
		if server == "broadcast" || server == "newsletter" {
			item.BroadcastOrNewsletter++
		}
		stored, class, stampOK := legacyStoredTimestamp(fieldOrNil(row, "timestamp"))
		if stampOK {
			if item.TimestampMin == "" || stored < item.TimestampMin {
				item.TimestampMin = stored
			}
			if stored > item.TimestampMax {
				item.TimestampMax = stored
			}
			if class == "zone_unspecified" {
				item.ZoneUnspecified++
			}
			if prev, ok := messageMax[chat]; !ok || stored > prev {
				messageMax[chat] = stored
			}
		}
		sender, senderOK := legacySender(rowText(row, "sender"), chat, fieldOrNil(row, "is_from_me"), account.TargetStorageJID)
		fromMe, flagOK := legacyBit(fieldOrNil(row, "is_from_me"))
		key := account.TargetStorageJID + "\x00" + chat + "\x00" + id
		_, present := staged[key]
		if present {
			item.Present++
		} else {
			item.Absent++
		}
		reason := ""
		switch {
		case id == "" || chat == "":
			reason = "identity_missing"
		case !stampOK:
			item.TimestampUnresolved++
			reason = "timestamp_unresolved"
		case !flagOK:
			item.FlagUnresolved++
			reason = "flag_unresolved"
		case !senderOK:
			item.SenderUnresolved++
			reason = "sender_unresolved"
		case present:
			diffs, worst, err := classifyStagedRow(projectLegacy(row, sender, stored, fromMe), projectStaged(staged[key]), false)
			if err != nil {
				return item, extra, err
			}
			for _, diff := range diffs {
				classes[diff.column+"\x00"+diff.class]++
			}
			if worst == "identical" || worst == "normalized" {
				item.AlreadySame++
				reason = "already_present"
			} else {
				item.Conflict++
				reason = "conflict"
			}
		default:
			reason = "imported"
			extra.inserts = append(extra.inserts, plannedLegacyInsert{
				account: account.TargetStorageJID, row: row, sender: sender, stamp: stored, fromMe: fromMe,
			})
		}
		extra.audit = append(extra.audit, legacyAuditRow{
			sha: account.SourceSHA256, account: account.TargetStorageJID, table: "messages",
			chat: chat, id: id, reason: reason, imported: reason == "imported", row: row,
		})
	}
	for jid, name := range chatName {
		stamp := chatTime[jid]
		if stamp == "" {
			stamp = messageMax[jid]
		}
		if stamp == "" {
			continue
		}
		extra.chats = append(extra.chats, legacyChatPlan{jid: jid, name: name, stamp: stamp})
	}
	item.ColumnClasses = columnClassCounts(classes)
	unsupported := []struct {
		table, reason string
		cols          []string
		retain        bool
	}{
		{"contact_nicknames", "not a message; chat name is not changed", legacyNicknameColumns, true},
		{"poll_options", "no GOWA question column; poll_definitions is not invented", legacyOptionColumns, true},
		{"poll_current_votes", "no GOWA question column; poll_definitions is not invented", legacyVoteColumns, true},
		{"webhook_configs", "not message history; row values were not read or copied", legacyWebhookConfigs, false},
		{"webhook_logs", "not message history; row values were not read or copied", legacyWebhookLogs, false},
		{"webhook_triggers", "not message history; row values were not read or copied", legacyWebhookTriggers, false},
	}
	for _, spec := range unsupported {
		count, err := countExactTable(src, spec.table, spec.cols)
		if err != nil {
			return item, extra, err
		}
		entry := UnsupportedClass{Table: spec.table, Rows: count, Reason: spec.reason}
		if spec.retain && count > 0 {
			rows, err := loadRowsQuery(src, spec.table, spec.cols)
			if err != nil {
				return item, extra, err
			}
			for _, row := range rows {
				chat, id := legacySideKey(spec.table, row)
				extra.audit = append(extra.audit, legacyAuditRow{
					sha: account.SourceSHA256, account: account.TargetStorageJID, table: spec.table,
					chat: chat, id: id, reason: "unsupported", imported: false, row: row,
				})
			}
			entry.RetainedInAudit = true
		}
		item.Unsupported = append(item.Unsupported, entry)
	}
	return item, extra, nil
}

func legacySideKey(table string, row sqlRow) (string, string) {
	switch table {
	case "contact_nicknames":
		return rowText(row, "jid"), ""
	case "poll_options":
		return rowText(row, "chat_jid"), rowText(row, "message_id") + "#" + rowText(row, "option_index")
	case "poll_current_votes":
		return rowText(row, "chat_jid"), rowText(row, "poll_message_id") + "#" + rowText(row, "voter_jid")
	default:
		return rowText(row, "chat_jid"), rowText(row, "id")
	}
}

func applyLegacy(staged *sql.DB, inserts []plannedLegacyInsert, chats []plannedChat) error {
	tx, err := staged.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`PRAGMA foreign_keys=ON`); err != nil {
		tx.Rollback()
		return err
	}
	for _, chat := range chats {
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM chats WHERE jid=? AND device_id=?`, chat.jid, chat.account).Scan(&n); err != nil {
			tx.Rollback()
			return err
		}
		if n > 0 {
			continue
		}
		if _, err := tx.Exec(`INSERT INTO chats (jid, device_id, name, last_message_time) VALUES (?, ?, ?, ?)`, chat.jid, chat.account, chat.name, chat.stamp); err != nil {
			tx.Rollback()
			return err
		}
	}
	for _, message := range inserts {
		var n int
		id := rowText(message.row, "id")
		chat := rowText(message.row, "chat_jid")
		if err := tx.QueryRow(`SELECT COUNT(*) FROM messages WHERE id=? AND chat_jid=? AND device_id=?`, id, chat, message.account).Scan(&n); err != nil {
			tx.Rollback()
			return err
		}
		if n > 0 {
			continue
		}
		if _, err := tx.Exec(`INSERT INTO messages (
			id, chat_jid, device_id, sender, content, timestamp, is_from_me,
			media_type, filename, url, direct_path, media_key, file_sha256, file_enc_sha256, file_length
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, chat, message.account, message.sender, fieldOrNil(message.row, "content"), message.stamp, message.fromMe,
			fieldOrNil(message.row, "media_type"), fieldOrNil(message.row, "filename"), fieldOrNil(message.row, "url"),
			fieldOrNil(message.row, "direct_path"), fieldOrNil(message.row, "media_key"), fieldOrNil(message.row, "file_sha256"),
			fieldOrNil(message.row, "file_enc_sha256"), fieldOrNil(message.row, "file_length"),
		); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func writeLegacyAudit(path string, rows []legacyAuditRow) error {
	if err := ensureAudit(path); err != nil {
		return err
	}
	db, err := openSQLite(path, false)
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS legacy_rows (
		source_sha256 TEXT NOT NULL,
		account_jid TEXT NOT NULL,
		table_name TEXT NOT NULL,
		chat_jid TEXT NOT NULL,
		message_id TEXT NOT NULL,
		reason TEXT NOT NULL,
		imported INTEGER NOT NULL,
		row_json TEXT NOT NULL
	)`); err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	for _, row := range rows {
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM legacy_rows WHERE source_sha256=? AND account_jid=? AND table_name=? AND chat_jid=? AND message_id=?`,
			row.sha, row.account, row.table, row.chat, row.id).Scan(&n); err != nil {
			tx.Rollback()
			return err
		}
		if n > 0 {
			continue
		}
		raw, err := privateRowJSON(row.row)
		if err != nil {
			tx.Rollback()
			return err
		}
		imported := 0
		if row.imported {
			imported = 1
		}
		if _, err := tx.Exec(`INSERT INTO legacy_rows (source_sha256, account_jid, table_name, chat_jid, message_id, reason, imported, row_json) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			row.sha, row.account, row.table, row.chat, row.id, row.reason, imported, raw); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func loadLegacyRows(db *sql.DB, table string, cols []string) ([]sqlRow, error) {
	timeCols := map[string]bool{
		"timestamp": true, "last_message_time": true, "created_at": true, "updated_at": true, "delivered_at": true,
	}
	quoted := make([]string, len(cols))
	for i, col := range cols {
		name, err := quoteIdent(col)
		if err != nil {
			return nil, err
		}
		if timeCols[col] {
			name = "CAST(" + name + " AS TEXT)"
		}
		quoted[i] = name
	}
	tableName, err := quoteIdent(table)
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(fmt.Sprintf(`SELECT %s FROM %s ORDER BY rowid`, strings.Join(quoted, ", "), tableName))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []sqlRow
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		for i, val := range vals {
			vals[i] = cloneValue(val)
		}
		out = append(out, sqlRow{cols: cols, vals: vals})
	}
	return out, rows.Err()
}

func loadLegacyMap(path string) ([]LegacyAccount, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Size() > 1<<20 {
		return nil, fmt.Errorf("legacy map %s is larger than 1MiB", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var file struct {
		Accounts []LegacyAccount `json:"accounts"`
	}
	if err := dec.Decode(&file); err != nil {
		return nil, fmt.Errorf("legacy map: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("legacy map has trailing data")
	}
	if len(file.Accounts) == 0 {
		return nil, fmt.Errorf("legacy map has no accounts")
	}
	for _, account := range file.Accounts {
		if !labelPattern.MatchString(account.Label) {
			return nil, fmt.Errorf("legacy map label %q is invalid", account.Label)
		}
		if account.SourcePath == "" || account.TargetStorageJID == "" {
			return nil, fmt.Errorf("legacy map entry %s is missing a path or storage JID", account.Label)
		}
		decoded, err := hex.DecodeString(account.SourceSHA256)
		if err != nil || len(decoded) != sha256Size {
			return nil, fmt.Errorf("legacy map entry %s needs a sha256 provenance", account.Label)
		}
	}
	return file.Accounts, nil
}

const sha256Size = 32

func exactStorageID(jid string, slots []RegistrySlot) bool {
	n := 0
	for _, slot := range slots {
		if slot.StorageID == jid {
			n++
		}
	}
	return n == 1
}

func requireLegacyShape(db *sql.DB) error {
	required := map[string][]string{
		"chats":              legacyChatColumns,
		"messages":           legacyMessageColumns,
		"contact_nicknames":  legacyNicknameColumns,
		"poll_current_votes": legacyVoteColumns,
		"poll_options":       legacyOptionColumns,
	}
	for table, cols := range required {
		got, err := columnNamesQuery(db, table)
		if err != nil {
			return err
		}
		if len(got) == 0 {
			return fmt.Errorf("legacy table %s is missing", table)
		}
		if !sameStringSet(got, cols) {
			return fmt.Errorf("legacy table %s columns are not the known bridge shape", table)
		}
	}
	messageKey, err := primaryKeyColumns(db, "messages")
	if err != nil {
		return err
	}
	if !sameStringSet(messageKey, []string{"id", "chat_jid"}) {
		return fmt.Errorf("legacy messages primary key is not (id, chat_jid)")
	}
	chatKey, err := primaryKeyColumns(db, "chats")
	if err != nil {
		return err
	}
	if !sameStringSet(chatKey, []string{"jid"}) {
		return fmt.Errorf("legacy chats primary key is not (jid)")
	}
	fks, err := legacyForeignKeys(db, "messages")
	if err != nil {
		return err
	}
	if len(fks) != 1 || fks[0] != "chats.jid<-chat_jid" {
		return fmt.Errorf("legacy messages foreign key is not chat_jid to chats.jid")
	}
	return nil
}

func countExactTable(db *sql.DB, table string, cols []string) (int, error) {
	got, err := columnNamesQuery(db, table)
	if err != nil {
		return 0, err
	}
	if len(got) == 0 {
		return 0, fmt.Errorf("legacy table %s is missing", table)
	}
	if !sameStringSet(got, cols) {
		return 0, fmt.Errorf("legacy table %s columns are not the known bridge shape", table)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ` + quoteTable(table)).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

func quoteTable(table string) string {
	return `"` + strings.ReplaceAll(table, `"`, `""`) + `"`
}

func primaryKeyColumns(db *sql.DB, table string) ([]string, error) {
	rows, err := db.Query(`SELECT name FROM pragma_index_list(?) WHERE origin='pk'`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var index string
	for rows.Next() {
		if index != "" {
			return nil, fmt.Errorf("legacy table %s has more than one primary key", table)
		}
		if err := rows.Scan(&index); err != nil {
			return nil, err
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if index == "" {
		return nil, fmt.Errorf("legacy table %s has no primary key", table)
	}
	info, err := db.Query(`SELECT name FROM pragma_index_info(?) ORDER BY seqno`, index)
	if err != nil {
		return nil, err
	}
	defer info.Close()
	var cols []string
	for info.Next() {
		var name string
		if err := info.Scan(&name); err != nil {
			return nil, err
		}
		cols = append(cols, name)
	}
	return cols, info.Err()
}

func legacyForeignKeys(db *sql.DB, table string) ([]string, error) {
	rows, err := db.Query(`SELECT "table", "from", "to" FROM pragma_foreign_key_list(?)`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var parent, from, to string
		if err := rows.Scan(&parent, &from, &to); err != nil {
			return nil, err
		}
		out = append(out, parent+"."+to+"<-"+from)
	}
	return out, rows.Err()
}

func legacySender(sender, chat string, fromMe any, account string) (string, bool) {
	bit, ok := legacyBit(fromMe)
	if ok && bit == 1 {
		return account, true
	}
	if strings.TrimSpace(sender) != "" {
		return sender, true
	}
	if directChat(chat) {
		return chat, true
	}
	return "", false
}

func legacyBit(value any) (int, bool) {
	if value == nil {
		return 0, false
	}
	bit, ok := bitValue(value)
	return bit, ok
}

func projectLegacy(row sqlRow, sender, stamp string, fromMe int) sqlRow {
	vals := make([]any, len(legacyProjected))
	for i, name := range legacyProjected {
		switch name {
		case "sender":
			vals[i] = sender
		case "timestamp":
			vals[i] = stamp
		case "is_from_me":
			vals[i] = fromMe
		default:
			vals[i] = fieldOrNil(row, name)
		}
	}
	return sqlRow{cols: append([]string{}, legacyProjected...), vals: vals}
}

func projectStaged(row sqlRow) sqlRow {
	vals := make([]any, len(legacyProjected))
	for i, name := range legacyProjected {
		vals[i] = fieldOrNil(row, name)
	}
	return sqlRow{cols: append([]string{}, legacyProjected...), vals: vals}
}

func fieldOrNil(row sqlRow, name string) any {
	value, ok := rowField(row, name)
	if !ok {
		return nil
	}
	return value
}

func rowText(row sqlRow, name string) string {
	value := fieldOrNil(row, name)
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case []byte:
		return string(typed)
	case int:
		return strconv.Itoa(typed)
	case int64:
		return strconv.FormatInt(typed, 10)
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	default:
		text, err := rowString(row, name)
		if err != nil {
			return ""
		}
		return text
	}
}

func nonEmptyField(row sqlRow, name string) bool {
	return !emptyValue(fieldOrNil(row, name))
}
