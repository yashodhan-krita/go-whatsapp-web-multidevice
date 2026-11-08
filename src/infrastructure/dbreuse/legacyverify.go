package dbreuse

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// LegacyVerifyRequest re-reads the declared legacy files and compares each
// original with the staged database and the private audit. It does not write.
type LegacyVerifyRequest struct {
	Staged  string
	MapPath string
	Audit   string
}

// LegacyVerifyAccount is one declared file. Counts and column classes do not
// include message text, filenames, or secrets.
type LegacyVerifyAccount struct {
	Label               string                   `json:"label"`
	SourceSHA256        string                   `json:"source_sha256"`
	TargetStorageJID    string                   `json:"target_storage_jid"`
	Messages            int                      `json:"messages"`
	StagedPayloadMatch  int                      `json:"staged_payload_match"`
	AuditExact          int                      `json:"audit_exact"`
	AuditOnly           int                      `json:"audit_only"`
	Missing             int                      `json:"missing"`
	Tampered            int                      `json:"tampered"`
	SenderUnresolved    int                      `json:"sender_unresolved"`
	TimestampUnresolved int                      `json:"timestamp_unresolved"`
	FlagUnresolved      int                      `json:"flag_unresolved"`
	ColumnClasses       []ColumnClassCount       `json:"column_classes"`
	Unsupported         []LegacyUnsupportedCheck `json:"unsupported"`
}

// LegacyUnsupportedCheck is a non-message table. Webhook values are not read.
type LegacyUnsupportedCheck struct {
	Table      string `json:"table"`
	Rows       int    `json:"rows"`
	AuditExact int    `json:"audit_exact"`
	Missing    int    `json:"missing"`
	Tampered   int    `json:"tampered"`
	Copied     bool   `json:"copied"`
	Reason     string `json:"reason"`
}

// LegacyVerifyReport is safe to print. Verified means every retained legacy
// original is an exact private-audit payload, or a message whose staged row
// matches and whose extra columns are empty. It does not mean history is complete.
type LegacyVerifyReport struct {
	StagedSHA256       string                `json:"staged_sha256"`
	AuditSHA256        string                `json:"audit_sha256"`
	AuditPath          string                `json:"audit_path"`
	FilesRead          int                   `json:"files_read"`
	Messages           int                   `json:"messages"`
	StagedPayloadMatch int                   `json:"staged_payload_match"`
	AuditExact         int                   `json:"audit_exact"`
	AuditOnly          int                   `json:"audit_only"`
	Missing            int                   `json:"missing"`
	Tampered           int                   `json:"tampered"`
	Accounts           []LegacyVerifyAccount `json:"accounts"`
	Limitations        []string              `json:"limitations"`
	Mismatches         []AuditIdentity       `json:"mismatches"`
	AuditSchemaOK      bool                  `json:"audit_schema_ok"`
	Verified           bool                  `json:"verified"`
	SourcesUnchanged   bool                  `json:"sources_unchanged"`
	MediaBytesCopied   bool                  `json:"media_bytes_copied"`
	OldBridgeAssessed  bool                  `json:"old_bridge_assessed"`
	HistoryComplete    bool                  `json:"history_complete"`
	CutoverReady       bool                  `json:"cutover_ready"`
	Notes              []string              `json:"notes"`
	Reasons            []string              `json:"reasons"`
}

// LegacyVerify proves each legacy message, poll option, poll vote, and
// nickname is still the exact source payload. An audit identity whose JSON
// differs is tampered. A missing JSON is not accepted merely because the
// staged message id exists, unless that staged row matches and every legacy
// column GOWA does not store is empty.
func LegacyVerify(_ context.Context, req LegacyVerifyRequest) (*LegacyVerifyReport, error) {
	report := &LegacyVerifyReport{
		Accounts:          []LegacyVerifyAccount{},
		Limitations:       []string{},
		Mismatches:        []AuditIdentity{},
		Reasons:           []string{},
		SourcesUnchanged:  true,
		MediaBytesCopied:  false,
		OldBridgeAssessed: false,
		HistoryComplete:   false,
		CutoverReady:      false,
		Notes: []string{
			"Each legacy message, poll option, poll vote, and nickname is read again from the declared file. The file hash must match the map.",
			"An audit row counts only when its row_json is the exact source payload. The same identity with different JSON is tampered.",
			"A staged message id is ordinary coverage only when the GOWA columns match the legacy projection. Extra legacy columns still have to be exact in the audit unless they are empty.",
			"Audit-only coverage is an exact audit payload whose staged projection differs or was not inserted. A conflict keeps the GOWA primary.",
			"Webhook tables are counted and not read. This check does not invent a sender and does not read archive JSON.",
			"history_complete, cutover_ready, and old_bridge_assessed stay false. This does not authorize a purge.",
		},
	}
	if req.Staged == "" || req.MapPath == "" {
		return report, fmt.Errorf("legacy-verify requires --db and --map")
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
		return report, fmt.Errorf("legacy-verify refused the staged snapshot")
	}
	for _, account := range accounts {
		if !exactStorageID(account.TargetStorageJID, checked.Registry) {
			return report, fmt.Errorf("target storage JID %s is not exactly one registry storage id", account.TargetStorageJID)
		}
	}
	stagedSum, err := hashFile(req.Staged)
	if err != nil {
		return report, err
	}
	report.StagedSHA256 = stagedSum
	auditPath := req.Audit
	if auditPath == "" {
		auditPath = auditPathFor(req.Staged)
	}
	report.AuditPath = auditPath
	auditSum, err := hashFile(auditPath)
	if err != nil {
		return report, err
	}
	report.AuditSHA256 = auditSum
	staged, err := openSQLite(req.Staged, true)
	if err != nil {
		return report, err
	}
	defer staged.Close()
	audit, err := openSQLite(auditPath, true)
	if err != nil {
		return report, err
	}
	defer audit.Close()
	if err := requireLegacyAuditRows(audit); err != nil {
		report.AuditSchemaOK = false
		report.Reasons = append(report.Reasons, err.Error())
		report.Verified = false
		return report, fmt.Errorf("legacy verify failed")
	}
	report.AuditSchemaOK = true
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
	for _, account := range accounts {
		before, err := hashFile(account.SourcePath)
		if err != nil {
			return report, err
		}
		if !strings.EqualFold(before, account.SourceSHA256) {
			report.Verified = false
			return report, fmt.Errorf("legacy file hash does not match the declared provenance")
		}
		src, err := openSQLite(account.SourcePath, true)
		if err != nil {
			return report, err
		}
		item, err := verifyLegacyAccount(src, audit, account, stagedByKey, report)
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
		report.Messages += item.Messages
		report.StagedPayloadMatch += item.StagedPayloadMatch
		report.AuditExact += item.AuditExact
		report.AuditOnly += item.AuditOnly
		report.Missing += item.Missing
		report.Tampered += item.Tampered
		for _, unsupported := range item.Unsupported {
			report.Missing += unsupported.Missing
			report.Tampered += unsupported.Tampered
			report.AuditExact += unsupported.AuditExact
		}
		if item.SenderUnresolved > 0 {
			report.Limitations = append(report.Limitations, fmt.Sprintf("%s has %d messages with no supported sender. They are audit-only when the raw row is exact. No sender was invented.", account.Label, item.SenderUnresolved))
		}
		report.Accounts = append(report.Accounts, item)
	}
	stagedAfter, err := hashFile(req.Staged)
	if err != nil {
		return report, err
	}
	auditAfter, err := hashFile(auditPath)
	if err != nil {
		return report, err
	}
	if stagedAfter != stagedSum || auditAfter != auditSum {
		report.SourcesUnchanged = false
		return report, fmt.Errorf("legacy-verify changed the staged database or the private audit")
	}
	if report.Missing > 0 {
		report.Reasons = append(report.Reasons, "a legacy original is not an exact audit payload and is not fully present in the staged row")
	}
	if report.Tampered > 0 {
		report.Reasons = append(report.Reasons, "a legacy_rows identity exists but its payload is not the source row")
	}
	report.Verified = len(report.Reasons) == 0 && report.Missing == 0 && report.Tampered == 0 && report.AuditSchemaOK && report.SourcesUnchanged
	report.HistoryComplete = false
	report.CutoverReady = false
	report.OldBridgeAssessed = false
	if !report.Verified {
		return report, fmt.Errorf("legacy verify failed")
	}
	return report, nil
}

func verifyLegacyAccount(src, audit *sql.DB, account LegacyAccount, staged map[string]sqlRow, report *LegacyVerifyReport) (LegacyVerifyAccount, error) {
	item := LegacyVerifyAccount{
		Label: account.Label, SourceSHA256: account.SourceSHA256, TargetStorageJID: account.TargetStorageJID,
		ColumnClasses: []ColumnClassCount{}, Unsupported: []LegacyUnsupportedCheck{},
	}
	if err := requireLegacyShape(src); err != nil {
		return item, err
	}
	stored, err := loadLegacyAuditPayloads(audit, account.SourceSHA256, account.TargetStorageJID)
	if err != nil {
		return item, err
	}
	messages, err := loadLegacyRows(src, "messages", legacyMessageColumns)
	if err != nil {
		return item, err
	}
	item.Messages = len(messages)
	classes := map[string]int{}
	for _, row := range messages {
		chat := rowText(row, "chat_jid")
		id := rowText(row, "id")
		storedStamp, _, stampOK := legacyStoredTimestamp(fieldOrNil(row, "timestamp"))
		sender, senderOK := legacySender(rowText(row, "sender"), chat, fieldOrNil(row, "is_from_me"), account.TargetStorageJID)
		fromMe, flagOK := legacyBit(fieldOrNil(row, "is_from_me"))
		if !stampOK {
			item.TimestampUnresolved++
		}
		if !flagOK {
			item.FlagUnresolved++
		}
		if !senderOK {
			item.SenderUnresolved++
		}
		key := account.TargetStorageJID + "\x00" + chat + "\x00" + id
		stagedRow, present := staged[key]
		match := false
		if present && stampOK && flagOK && senderOK {
			diffs, worst, err := classifyStagedRow(projectLegacy(row, sender, storedStamp, fromMe), projectStaged(stagedRow), false)
			if err != nil {
				return item, err
			}
			for _, diff := range diffs {
				classes[diff.column+"\x00"+diff.class]++
			}
			match = worst == "identical" || worst == "normalized"
		}
		if match {
			item.StagedPayloadMatch++
		}
		exact, found, err := exactLegacyPayload(stored["messages\x00"+chat+"\x00"+id], row)
		if err != nil {
			return item, err
		}
		switch {
		case exact:
			item.AuditExact++
			if !match {
				item.AuditOnly++
			}
		case found:
			item.Tampered++
			addLegacyMismatch(report, "messages", account.TargetStorageJID, chat, id, "tampered")
		default:
			if match && legacyExtrasEmpty(row) {
				break
			}
			item.Missing++
			addLegacyMismatch(report, "messages", account.TargetStorageJID, chat, id, "missing")
		}
	}
	item.ColumnClasses = columnClassCounts(classes)
	retained := []struct {
		table, reason string
		cols          []string
	}{
		{"contact_nicknames", "not a message; exact raw row required in the audit", legacyNicknameColumns},
		{"poll_options", "no GOWA question column; exact raw row required in the audit", legacyOptionColumns},
		{"poll_current_votes", "no GOWA question column; exact raw row required in the audit", legacyVoteColumns},
	}
	for _, spec := range retained {
		check, err := verifyRetainedTable(src, spec.table, spec.cols, spec.reason, stored, account, report)
		if err != nil {
			return item, err
		}
		item.Unsupported = append(item.Unsupported, check)
	}
	webhooks := []struct {
		table string
		cols  []string
	}{
		{"webhook_configs", legacyWebhookConfigs},
		{"webhook_logs", legacyWebhookLogs},
		{"webhook_triggers", legacyWebhookTriggers},
	}
	for _, spec := range webhooks {
		count, err := countExactTable(src, spec.table, spec.cols)
		if err != nil {
			return item, err
		}
		item.Unsupported = append(item.Unsupported, LegacyUnsupportedCheck{
			Table: spec.table, Rows: count, Copied: false,
			Reason: "not message history; row values were not read or copied",
		})
	}
	return item, nil
}

func verifyRetainedTable(src *sql.DB, table string, cols []string, reason string, stored map[string][]string, account LegacyAccount, report *LegacyVerifyReport) (LegacyUnsupportedCheck, error) {
	check := LegacyUnsupportedCheck{Table: table, Reason: reason, Copied: true}
	count, err := countExactTable(src, table, cols)
	if err != nil {
		return check, err
	}
	check.Rows = count
	if count == 0 {
		return check, nil
	}
	rows, err := loadRowsQuery(src, table, cols)
	if err != nil {
		return check, err
	}
	for _, row := range rows {
		chat, id := legacySideKey(table, row)
		exact, found, err := exactLegacyPayload(stored[table+"\x00"+chat+"\x00"+id], row)
		if err != nil {
			return check, err
		}
		switch {
		case exact:
			check.AuditExact++
		case found:
			check.Tampered++
			addLegacyMismatch(report, table, account.TargetStorageJID, chat, id, "tampered")
		default:
			check.Missing++
			addLegacyMismatch(report, table, account.TargetStorageJID, chat, id, "missing")
		}
	}
	return check, nil
}

func loadLegacyAuditPayloads(db *sql.DB, sha, account string) (map[string][]string, error) {
	rows, err := db.Query(`SELECT table_name, chat_jid, message_id, row_json FROM legacy_rows WHERE source_sha256=? AND account_jid=?`, sha, account)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var table, chat, id, raw string
		if err := rows.Scan(&table, &chat, &id, &raw); err != nil {
			return nil, err
		}
		key := table + "\x00" + chat + "\x00" + id
		out[key] = append(out[key], raw)
	}
	return out, rows.Err()
}

func exactLegacyPayload(raws []string, row sqlRow) (exact bool, found bool, err error) {
	if len(raws) == 0 {
		return false, false, nil
	}
	for _, raw := range raws {
		match, err := auditJSONMatchesRow(raw, row)
		if err != nil {
			return false, true, err
		}
		if match {
			return true, true, nil
		}
	}
	return false, true, nil
}

func legacyExtrasEmpty(row sqlRow) bool {
	projected := map[string]struct{}{}
	for _, name := range legacyProjected {
		projected[name] = struct{}{}
	}
	for _, name := range legacyMessageColumns {
		if name == "id" || name == "chat_jid" {
			continue
		}
		if _, ok := projected[name]; ok {
			continue
		}
		if !emptyValue(fieldOrNil(row, name)) {
			return false
		}
	}
	return true
}

func requireLegacyAuditRows(db *sql.DB) error {
	got, err := columnNamesQuery(db, "legacy_rows")
	if err != nil {
		return err
	}
	expected := []string{"source_sha256", "account_jid", "table_name", "chat_jid", "message_id", "reason", "imported", "row_json"}
	if len(got) == 0 {
		return fmt.Errorf("audit schema is missing legacy_rows")
	}
	if !sameStringSet(got, expected) {
		return fmt.Errorf("audit table legacy_rows columns differ")
	}
	return nil
}

func addLegacyMismatch(report *LegacyVerifyReport, table, device, chat, id, problem string) {
	if len(report.Mismatches) >= 20 {
		return
	}
	report.Mismatches = append(report.Mismatches, AuditIdentity{
		Side: table, DeviceID: device, ChatJID: chat, MessageID: id, Problem: problem,
	})
}
