package dbreuse

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// ImportRequest copies absent history-sync messages into a staged chatstorage file.
// The raw message JSON is appended to the private audit. Metadata-only files are
// not treated as proof, and history_complete stays false.
type ImportRequest struct {
	DB       string
	Account  string
	Audit    string
	Archives []string
}

// ImportReport is safe to print. Message bodies stay in the audit file.
type ImportReport struct {
	Database                string   `json:"database"`
	AuditPath               string   `json:"audit_path"`
	Account                 string   `json:"account"`
	Imported                int      `json:"imported"`
	AlreadyPresent          int      `json:"already_present"`
	DistinctOtherChat       int      `json:"distinct_other_chat"`
	SkippedMetadataOnly     int      `json:"skipped_metadata_only"`
	SkippedEmpty            int      `json:"skipped_empty_conversations"`
	SenderUnresolved        int      `json:"sender_unresolved"`
	TimestampUnresolved     int      `json:"timestamp_unresolved"`
	TextExtracted           int      `json:"text_extracted"`
	TextRetainedOnlyInAudit int      `json:"text_retained_only_in_audit"`
	ChatsAdded              int      `json:"chats_added"`
	HistoryComplete         bool     `json:"history_complete"`
	HistoryCompleteReason   string   `json:"history_complete_reason"`
	OldBridgeAssessed       bool     `json:"old_bridge_assessed"`
	MetadataOnlyIsProof     bool     `json:"metadata_only_is_proof"`
	Notes                   []string `json:"notes"`
}

// ImportArchives inserts history JSON messages that are absent for the
// account's (chat, id) pair. It does not overwrite a row that is already staged.
func ImportArchives(req ImportRequest) (*ImportReport, error) {
	report := &ImportReport{
		Database:              req.DB,
		HistoryComplete:       false,
		OldBridgeAssessed:     false,
		MetadataOnlyIsProof:   false,
		HistoryCompleteReason: historyCompleteNote + " Imported archive ids are not complete history. A legacy dry run of 908 keys, excluding broadcasts and newsletters, is not full legacy coverage. Old-bridge SQLite still needs its own export and account map. This does not authorize a purge.",
		Notes: []string{
			"Only a missing (account, chat, message id) row is inserted. An id that already exists under another chat is inserted for this chat and is not merged.",
			"The raw history message is stored in the private audit. This report does not include it.",
			"Metadata-only history files add no messages and are not proof of coverage.",
			"No LID mapping is invented. Sender is taken from fromMe, participant, or the chat JID.",
			historyCompleteNote,
		},
	}
	if req.DB == "" || len(req.Archives) == 0 {
		return report, fmt.Errorf("import-archive requires --db and at least one --archive")
	}
	checked, err := Check(req.DB)
	if err != nil {
		return report, err
	}
	if checked == nil || !checked.Reusable {
		reason := "schema was not accepted"
		if checked != nil && len(checked.Reasons) > 0 {
			reason = strings.Join(checked.Reasons, "; ")
		}
		return report, fmt.Errorf("import-archive refused the snapshot: %s", reason)
	}
	account, err := resolveImportAccount(req.Account, req.Archives, checked.Registry)
	if err != nil {
		return report, err
	}
	report.Account = account
	auditPath := req.Audit
	if auditPath == "" {
		auditPath = auditPathFor(req.DB)
	}
	report.AuditPath = auditPath
	if err := ensureAudit(auditPath); err != nil {
		return report, err
	}
	db, err := openSQLite(req.DB, false)
	if err != nil {
		return report, err
	}
	defer db.Close()
	if _, err := db.Exec(`PRAGMA foreign_keys=ON`); err != nil {
		return report, err
	}
	before, err := fingerprintTables(db, preservedTables)
	if err != nil {
		return report, err
	}
	tx, err := db.Begin()
	if err != nil {
		return report, err
	}
	for _, path := range req.Archives {
		parsed, err := readHistoryArchive(path)
		if err != nil {
			tx.Rollback()
			return report, err
		}
		if !parsed.MessageAudit {
			report.SkippedMetadataOnly++
			continue
		}
		report.SkippedEmpty += parsed.EmptyConversations
		sum := ""
		if infoSum, sumErr := hashFile(path); sumErr == nil {
			sum = infoSum
		}
		for _, payload := range parsed.Payloads {
			decision, projectErr := projectArchiveMessage(payload, account)
			if projectErr != nil {
				tx.Rollback()
				return report, projectErr
			}
			exists, err := messagePairExists(tx, account, decision.chat, decision.id)
			if err != nil {
				tx.Rollback()
				return report, err
			}
			if exists {
				report.AlreadyPresent++
				if err := appendArchiveAudit(auditPath, path, sum, account, decision.chat, decision.id, "already_present", false, string(payload.Raw)); err != nil {
					tx.Rollback()
					return report, err
				}
				continue
			}
			other, err := messageIDOtherChat(tx, account, decision.chat, decision.id)
			if err != nil {
				tx.Rollback()
				return report, err
			}
			if decision.skip != "" {
				switch decision.skip {
				case "sender_unresolved":
					report.SenderUnresolved++
				case "timestamp_unresolved":
					report.TimestampUnresolved++
				}
				if err := appendArchiveAudit(auditPath, path, sum, account, decision.chat, decision.id, decision.skip, false, string(payload.Raw)); err != nil {
					tx.Rollback()
					return report, err
				}
				continue
			}
			added, err := ensureChat(tx, account, decision.chat, decision.timestamp)
			if err != nil {
				tx.Rollback()
				return report, err
			}
			if added {
				report.ChatsAdded++
			}
			if _, err := tx.Exec(`INSERT INTO messages (id, chat_jid, device_id, sender, content, timestamp, is_from_me) VALUES (?, ?, ?, ?, ?, ?, ?)`,
				decision.id, decision.chat, account, decision.sender, decision.content, decision.timestamp, decision.fromMe); err != nil {
				tx.Rollback()
				return report, err
			}
			report.Imported++
			if other {
				report.DistinctOtherChat++
			}
			if decision.textExtracted {
				report.TextExtracted++
			} else {
				report.TextRetainedOnlyInAudit++
			}
			if err := appendArchiveAudit(auditPath, path, sum, account, decision.chat, decision.id, "imported", true, string(payload.Raw)); err != nil {
				tx.Rollback()
				return report, err
			}
		}
	}
	after, err := fingerprintTables(tx, preservedTables)
	if err != nil {
		tx.Rollback()
		return report, err
	}
	if before != after {
		tx.Rollback()
		return report, fmt.Errorf("registry, schedule, or Chatwoot rows changed")
	}
	if err := tx.Commit(); err != nil {
		return report, err
	}
	report.HistoryComplete = false
	report.OldBridgeAssessed = false
	report.MetadataOnlyIsProof = false
	return report, nil
}

type projectedMessage struct {
	chat          string
	id            string
	sender        string
	content       string
	timestamp     string
	fromMe        int
	textExtracted bool
	skip          string
}

func projectArchiveMessage(payload archivePayload, account string) (projectedMessage, error) {
	out := projectedMessage{chat: payload.ChatJID, id: payload.ID}
	var body struct {
		Message struct {
			Key struct {
				RemoteJID   string `json:"remoteJid"`
				FromMe      *bool  `json:"fromMe"`
				ID          string `json:"id"`
				Participant string `json:"participant"`
			} `json:"key"`
			Message          map[string]json.RawMessage `json:"message"`
			MessageTimestamp json.RawMessage            `json:"messageTimestamp"`
		} `json:"message"`
	}
	if err := json.Unmarshal(payload.Raw, &body); err != nil {
		return out, fmt.Errorf("archive message %s: %w", payload.ID, err)
	}
	seconds, ok := unixSeconds(body.Message.MessageTimestamp)
	if !ok {
		out.skip = "timestamp_unresolved"
		return out, nil
	}
	out.timestamp = time.Unix(seconds, 0).UTC().Format("2006-01-02 15:04:05")
	fromMe := false
	if body.Message.Key.FromMe != nil {
		fromMe = *body.Message.Key.FromMe
	}
	if fromMe {
		out.fromMe = 1
		out.sender = account
	} else if body.Message.Key.Participant != "" {
		out.sender = body.Message.Key.Participant
	} else if out.chat != "" && !strings.Contains(out.chat, "@g.us") && !strings.Contains(out.chat, "@broadcast") && !strings.Contains(out.chat, "@newsletter") {
		out.sender = out.chat
	} else {
		out.skip = "sender_unresolved"
		return out, nil
	}
	if text, ok := archiveText(body.Message.Message); ok {
		out.content = text
		out.textExtracted = true
	}
	return out, nil
}

func unixSeconds(raw json.RawMessage) (int64, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, false
	}
	text := strings.Trim(string(raw), `"`)
	seconds, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0, false
	}
	return seconds, true
}

func archiveText(message map[string]json.RawMessage) (string, bool) {
	if message == nil {
		return "", false
	}
	if raw, ok := message["conversation"]; ok {
		var text string
		if err := json.Unmarshal(raw, &text); err == nil {
			return text, true
		}
	}
	if raw, ok := message["extendedTextMessage"]; ok {
		var extended struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(raw, &extended); err == nil && extended.Text != "" {
			return extended.Text, true
		}
	}
	return "", false
}

func messagePairExists(q queryer, device, chat, id string) (bool, error) {
	var n int
	err := q.QueryRow(`SELECT COUNT(*) FROM messages WHERE device_id=? AND chat_jid=? AND id=?`, device, chat, id).Scan(&n)
	return n > 0, err
}

func messageIDOtherChat(q queryer, device, chat, id string) (bool, error) {
	var n int
	err := q.QueryRow(`SELECT COUNT(*) FROM messages WHERE device_id=? AND id=? AND chat_jid<>?`, device, id, chat).Scan(&n)
	return n > 0, err
}

func ensureChat(tx *sql.Tx, device, chat, stamp string) (bool, error) {
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM chats WHERE device_id=? AND jid=?`, device, chat).Scan(&n); err != nil {
		return false, err
	}
	if n > 0 {
		return false, nil
	}
	_, err := tx.Exec(`INSERT INTO chats (jid, device_id, name, last_message_time) VALUES (?, ?, '', ?)`, chat, device, stamp)
	return err == nil, err
}

func ensureAudit(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	return writeMigrationAudit(path, nil, nil)
}

func resolveImportAccount(explicit string, archives []string, slots []RegistrySlot) (string, error) {
	var fromFiles []string
	for _, path := range archives {
		jids := jidsInFilename(path)
		if len(jids) == 0 {
			continue
		}
		var matched []string
		for _, jid := range jids {
			slot, ok := matchRegistryJID(jid, slots)
			if !ok {
				return "", fmt.Errorf("archive filename JID %s is not one registry storage id", jid)
			}
			matched = append(matched, slot)
		}
		matched = uniqueStrings(matched)
		if len(matched) != 1 {
			return "", fmt.Errorf("archive filename matches more than one registry account")
		}
		fromFiles = append(fromFiles, matched[0])
	}
	fromFiles = uniqueStrings(fromFiles)
	explicit = strings.TrimSpace(explicit)
	if explicit != "" {
		if _, ok := matchRegistryJID(explicit, slots); !ok {
			return "", fmt.Errorf("account %s is not a registry storage id", explicit)
		}
		if len(fromFiles) > 0 && (len(fromFiles) != 1 || fromFiles[0] != explicit) {
			return "", fmt.Errorf("archive filename JID does not match --account %s", explicit)
		}
		return explicit, nil
	}
	if len(fromFiles) == 1 {
		return fromFiles[0], nil
	}
	if len(fromFiles) == 0 {
		return "", fmt.Errorf("archive filename has no registry JID; pass --account with the storage JID")
	}
	return "", fmt.Errorf("archive filenames match more than one registry account")
}

func jidsInFilename(path string) []string {
	name := path
	if slash := strings.LastIndexAny(path, `/\`); slash >= 0 {
		name = path[slash+1:]
	}
	var out []string
	rest := name
	for {
		at := strings.Index(rest, "@")
		if at < 0 {
			break
		}
		userStart := strings.LastIndex(rest[:at], "-")
		if userStart < 0 {
			userStart = 0
		} else {
			userStart++
		}
		end := at + 1
		for end < len(rest) && isJIDServer(rest[end]) {
			end++
		}
		if userStart < at && end > at+1 {
			out = append(out, rest[userStart:end])
		}
		if end >= len(rest) {
			break
		}
		rest = rest[end:]
	}
	return out
}

func isJIDServer(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '.'
}

func matchRegistryJID(jid string, slots []RegistrySlot) (string, bool) {
	var found []string
	for _, slot := range slots {
		if slot.StorageID == "" {
			continue
		}
		if jid == slot.StorageID || jid == slot.ADJID || nonADJID(jid) == slot.StorageID || (slot.ADJID != "" && nonADJID(slot.ADJID) == slot.StorageID && nonADJID(jid) == slot.StorageID) {
			found = append(found, slot.StorageID)
		}
	}
	found = uniqueStrings(found)
	if len(found) != 1 {
		return "", false
	}
	return found[0], true
}

func nonADJID(jid string) string {
	at := strings.LastIndex(jid, "@")
	if at <= 0 {
		return jid
	}
	user, server := jid[:at], jid[at:]
	if colon := strings.IndexByte(user, ':'); colon > 0 && digitsOnly(user[colon+1:]) {
		user = user[:colon]
	}
	return user + server
}

func digitsOnly(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func uniqueStrings(values []string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}
