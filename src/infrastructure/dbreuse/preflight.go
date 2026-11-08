package dbreuse

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

// Schema versions this check understands.
//
// 49 is the v9.7.0 tag. 50 is this fork (AppVersion still v9.7.0, plus the
// Chatwoot webhook-secret column). 54 is upstream v9.8.0 and v9.9.0.
// Versions 51–53 are the official scheduled-send upgrades in between.
// v9.9.0 applies whatever remains of that list at startup; none of those
// statements rewrite chat or message rows.
const (
	schemaV970Tag       = 49
	schemaThisFork      = 50
	schemaOfficialV990  = 54
	historyCompleteNote = "These tools never set history_complete true. A reusable chatstorage file is not a complete WhatsApp history. Rows that were never stored, and history-*.json archives, are not proved present by snapshot, preflight, reconcile, overlay, or coverage."
)

// Preflight is the reuse decision for one chatstorage database.
type Preflight struct {
	Path                   string         `json:"path"`
	SchemaVersion          int            `json:"schema_version"`
	Reusable               bool           `json:"reusable"`
	Mode                   string         `json:"mode"`
	Reasons                []string       `json:"reasons"`
	Warnings               []string       `json:"warnings"`
	Notes                  []string       `json:"notes"`
	HistoryComplete        bool           `json:"history_complete"`
	HistoryCompleteReason  string         `json:"history_complete_reason"`
	Registry               []RegistrySlot `json:"registry"`
	OrphanHistoryDeviceIDs []string       `json:"orphan_history_device_ids"`
	WALBytes               int64          `json:"wal_bytes"`
	WALPresent             bool           `json:"wal_present"`
	SHMPresent             bool           `json:"shm_present"`
	MessageCount           int            `json:"message_count"`
	ChatCount              int            `json:"chat_count"`
}

// RegistrySlot is the device row the running process uses to find history.
type RegistrySlot struct {
	DeviceID  string `json:"device_id"`
	JID       string `json:"jid"`
	ADJID     string `json:"ad_jid"`
	StorageID string `json:"storage_id"`
}

// Check decides whether official v9.9.0 can open this chatstorage file and
// whether its registry storage ids cover the history rows.
func Check(path string) (*Preflight, error) {
	report := &Preflight{
		Path:                   path,
		Mode:                   "refused",
		Reasons:                []string{},
		Warnings:               []string{},
		Registry:               []RegistrySlot{},
		OrphanHistoryDeviceIDs: []string{},
		HistoryComplete:        false,
		HistoryCompleteReason:  historyCompleteNote,
		Notes: []string{
			"chatstorage.db is the history and registry file. whatsapp.db is the session and is not opened here.",
			"History queries use devices.jid when it is set, otherwise devices.device_id.",
			"scheduled_sends.device_id is the registry slot id, not the history storage id.",
			"The schema version does not identify the binary. This fork's tree is not the installed Mac revision.",
			historyCompleteNote,
		},
	}
	walSize, walExists, err := siblingFile(path, "-wal")
	if err != nil {
		return report, err
	}
	report.WALBytes = walSize
	report.WALPresent = walExists
	_, shmExists, err := siblingFile(path, "-shm")
	if err != nil {
		return report, err
	}
	report.SHMPresent = shmExists
	if walExists {
		report.Warnings = append(report.Warnings, "a -wal file is present; copy the database with db-reuse snapshot so the latest commits are included in one file")
	}
	if shmExists {
		report.Warnings = append(report.Warnings, "a -shm file is present; a GOWA process may have this database open")
	}

	db, err := openSQLite(path, true)
	if err != nil {
		report.Reasons = append(report.Reasons, err.Error())
		return report, err
	}
	defer db.Close()

	version, err := schemaVersion(db)
	if err != nil {
		report.Reasons = append(report.Reasons, err.Error())
		return report, nil
	}
	report.SchemaVersion = version
	if err := checkSchema(db, version); err != nil {
		report.Reasons = append(report.Reasons, err.Error())
	} else if version == schemaOfficialV990 {
		report.Mode = "unchanged"
	} else if version >= schemaV970Tag && version < schemaOfficialV990 {
		report.Mode = "official-v9.9.0-upgrade"
		report.Notes = append(report.Notes, "The first v9.9.0 start applies the remaining official migrations. Migrations 51-54 alter scheduled_sends only. Migration 53 marks running or already-attempted schedules so they are not replayed. Message, chat, reaction, edit, poll, and device rows are not rewritten.")
	}

	slots, err := readRegistry(db)
	if err != nil {
		report.Reasons = append(report.Reasons, err.Error())
	} else {
		report.Registry = slots
	}
	orphans, messageCount, chatCount, err := historyCoverage(db, slots)
	if err != nil {
		report.Reasons = append(report.Reasons, err.Error())
	} else {
		report.OrphanHistoryDeviceIDs = orphans
		report.MessageCount = messageCount
		report.ChatCount = chatCount
		if len(orphans) > 0 {
			report.Reasons = append(report.Reasons, "history device_id values are not registry storage ids: "+strings.Join(orphans, ", "))
		}
	}
	if err := warnScheduleSlots(db, slots, report); err != nil {
		report.Reasons = append(report.Reasons, err.Error())
	}
	report.Reusable = len(report.Reasons) == 0
	return report, nil
}

func schemaVersion(db *sql.DB) (int, error) {
	var name string
	err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='schema_info'`).Scan(&name)
	if err == sql.ErrNoRows {
		return 0, fmt.Errorf("schema_info is missing")
	}
	if err != nil {
		return 0, err
	}
	var version int
	if err := db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_info`).Scan(&version); err != nil {
		return 0, err
	}
	if version == 0 {
		return 0, fmt.Errorf("schema_info has no version")
	}
	return version, nil
}

func checkSchema(db *sql.DB, version int) error {
	if version < schemaV970Tag || version > schemaOfficialV990 {
		return fmt.Errorf("schema version %d is outside the known range 49-54", version)
	}
	tables := []string{"chats", "messages", "message_reactions", "message_edits", "poll_definitions", "devices", "chatwoot_device_configs", "scheduled_sends"}
	for _, table := range tables {
		got, err := columnNames(db, table)
		if err != nil {
			return err
		}
		want := expectedColumns(table, version)
		if want == nil {
			return fmt.Errorf("no column fingerprint for %s at schema %d", table, version)
		}
		if !sameStrings(got, want) {
			return fmt.Errorf("%s columns at schema %d are %s, want %s", table, version, strings.Join(got, ","), strings.Join(want, ","))
		}
	}
	return nil
}

func columnNames(db *sql.DB, table string) ([]string, error) {
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?) ORDER BY cid`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("table %s is missing", table)
	}
	return names, nil
}

func expectedColumns(table string, version int) []string {
	switch table {
	case "chats":
		return []string{"jid", "device_id", "name", "last_message_time", "ephemeral_expiration", "created_at", "updated_at", "archived"}
	case "messages":
		return []string{"id", "chat_jid", "device_id", "sender", "content", "timestamp", "is_from_me", "media_type", "filename", "url", "media_key", "file_sha256", "file_enc_sha256", "file_length", "created_at", "updated_at", "call_metadata", "referral_metadata", "direct_path", "context_metadata"}
	case "message_reactions":
		return []string{"message_id", "chat_jid", "device_id", "reactor_jid", "emoji", "is_from_me", "reaction_timestamp", "created_at", "updated_at"}
	case "message_edits":
		return []string{"original_message_id", "edit_event_id", "chat_jid", "device_id", "editor", "previous_content", "new_content", "edited_at", "created_at"}
	case "poll_definitions":
		return []string{"device_id", "chat_jid", "poll_message_id", "question", "options_json", "selectable_option_count", "version", "created_at", "updated_at"}
	case "devices":
		return []string{"device_id", "display_name", "jid", "created_at", "updated_at", "webhook_url", "webhook_secret", "webhook_events", "webhook_insecure_skip_verify", "ad_jid"}
	case "chatwoot_device_configs":
		cols := []string{"id", "device_id", "device_jid", "chatwoot_url", "account_id", "inbox_id", "api_token", "enabled", "created_at", "updated_at"}
		if version >= schemaThisFork {
			cols = append(cols, "webhook_secret")
		}
		return cols
	case "scheduled_sends":
		cols := []string{"id", "device_id", "message_type", "payload_json", "assets_json", "phone", "summary", "scheduled_at", "next_run_at", "timezone", "recurrence", "weekdays_json", "day_of_month", "end_at", "occurrence_limit", "occurrence_count", "attempts", "status", "lease_token", "lease_until", "last_run_at", "last_message_id", "last_error", "created_at", "updated_at"}
		if version >= 51 {
			cols = append(cols, "pending_message_id")
		}
		if version >= 52 {
			cols = append(cols, "legacy_attempt_unknown")
		}
		if version >= schemaOfficialV990 {
			cols = append(cols, "pending_poll_secret")
		}
		return cols
	default:
		return nil
	}
}

func sameStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	a := append([]string(nil), got...)
	b := append([]string(nil), want...)
	sort.Strings(a)
	sort.Strings(b)
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func readRegistry(db *sql.DB) ([]RegistrySlot, error) {
	rows, err := db.Query(`SELECT device_id, COALESCE(jid, ''), COALESCE(ad_jid, '') FROM devices ORDER BY device_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var slots []RegistrySlot
	for rows.Next() {
		var slot RegistrySlot
		if err := rows.Scan(&slot.DeviceID, &slot.JID, &slot.ADJID); err != nil {
			return nil, err
		}
		slot.StorageID = slot.DeviceID
		if strings.TrimSpace(slot.JID) != "" {
			slot.StorageID = slot.JID
		}
		slots = append(slots, slot)
	}
	return slots, rows.Err()
}

func historyCoverage(db *sql.DB, slots []RegistrySlot) (orphans []string, messages, chats int, err error) {
	known := map[string]struct{}{}
	for _, slot := range slots {
		if slot.StorageID != "" {
			known[slot.StorageID] = struct{}{}
		}
	}
	queries := []string{
		`SELECT DISTINCT device_id FROM messages`,
		`SELECT DISTINCT device_id FROM chats`,
		`SELECT DISTINCT device_id FROM message_reactions`,
		`SELECT DISTINCT device_id FROM message_edits`,
		`SELECT DISTINCT device_id FROM poll_definitions`,
	}
	seen := map[string]struct{}{}
	for _, query := range queries {
		rows, err := db.Query(query)
		if err != nil {
			return nil, 0, 0, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, 0, 0, err
			}
			if _, ok := known[id]; !ok {
				seen[id] = struct{}{}
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, 0, 0, err
		}
		rows.Close()
	}
	for id := range seen {
		orphans = append(orphans, id)
	}
	sort.Strings(orphans)
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messages); err != nil {
		return nil, 0, 0, err
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM chats`).Scan(&chats); err != nil {
		return nil, 0, 0, err
	}
	return orphans, messages, chats, nil
}

func warnScheduleSlots(db *sql.DB, slots []RegistrySlot, report *Preflight) error {
	known := map[string]struct{}{}
	for _, slot := range slots {
		known[slot.DeviceID] = struct{}{}
		if slot.ADJID == "" {
			report.Warnings = append(report.Warnings, "device "+slot.DeviceID+" has no ad_jid; a later boot can backfill the first matching whatsapp.db companion")
		}
	}
	rows, err := db.Query(`SELECT DISTINCT device_id FROM scheduled_sends`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		if _, ok := known[id]; !ok {
			report.Warnings = append(report.Warnings, "scheduled_sends row uses unknown slot "+id)
		}
	}
	return rows.Err()
}
