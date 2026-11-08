package dbreuse

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// AuditVerifyRequest checks a staged overlay against its source, target, and
// private audit. The inputs are opened read-only.
type AuditVerifyRequest struct {
	Staged  string
	Source  string
	Target  string
	MapPath string
	Audit   string
	Base    string
}

// AuditIdentity is a message key named in a mismatch. It has no message body.
type AuditIdentity struct {
	Side      string `json:"side"`
	DeviceID  string `json:"device_id"`
	ChatJID   string `json:"chat_jid"`
	MessageID string `json:"message_id"`
	Problem   string `json:"problem"`
}

// AuditVerifyReport is safe to print. Counts and file hashes do not include
// message text, media keys, or filenames.
type AuditVerifyReport struct {
	StagedSHA256           string          `json:"staged_sha256"`
	SourceSHA256           string          `json:"source_sha256"`
	TargetSHA256           string          `json:"target_sha256"`
	AuditSHA256            string          `json:"audit_sha256"`
	Base                   string          `json:"base"`
	StagedMessages         int             `json:"staged_messages"`
	SourceMessages         int             `json:"source_messages"`
	TargetMessages         int             `json:"target_messages"`
	SourceRetained         int             `json:"source_retained"`
	SourceMissing          int             `json:"source_missing"`
	SourcePayloadMismatch  int             `json:"source_payload_mismatch"`
	TargetIdentical        int             `json:"target_identical"`
	TargetRetainedInAudit  int             `json:"target_retained_in_audit"`
	TargetMissing          int             `json:"target_missing"`
	PreservedTargetDevices int             `json:"preserved_target_devices"`
	RegistryMatchesBase    bool            `json:"registry_matches_base"`
	AuditSchemaOK          bool            `json:"audit_schema_ok"`
	MessageVersions        int             `json:"message_versions"`
	TargetAlternates       int             `json:"target_alternates"`
	KeyCollisions          int             `json:"key_collisions"`
	RelinkedCollisions     int             `json:"relinked_collisions"`
	OrphanRows             int             `json:"orphan_rows"`
	InventedParents        int             `json:"invented_parents"`
	ArchiveRows            int             `json:"archive_rows"`
	Mismatches             []AuditIdentity `json:"mismatches"`
	Verified               bool            `json:"verified"`
	HistoryComplete        bool            `json:"history_complete"`
	CutoverReady           bool            `json:"cutover_ready"`
	OldBridgeAssessed      bool            `json:"old_bridge_assessed"`
	Reasons                []string        `json:"reasons"`
	Notes                  []string        `json:"notes"`
}

// AuditVerify checks that mapped source rows survived in the staged file and
// that every target row is either still there or retained in the audit.
func AuditVerify(_ context.Context, req AuditVerifyRequest) (*AuditVerifyReport, error) {
	report := &AuditVerifyReport{
		Base:              req.Base,
		Mismatches:        []AuditIdentity{},
		Reasons:           []string{},
		HistoryComplete:   false,
		CutoverReady:      false,
		OldBridgeAssessed: false,
		Notes: []string{
			"Verification compares message identity and payload. The printed report has no message text, media key, or filename.",
			"A target row that differs from the staged row must match a retained_alternate in the private audit.",
			"history_complete and cutover_ready stay false. This check does not authorize a purge.",
		},
	}
	if req.Base == "" {
		req.Base = "target"
		report.Base = req.Base
	}
	if req.Base != "source" && req.Base != "target" {
		return report, fmt.Errorf("audit-verify --base must be source or target")
	}
	if req.Staged == "" || req.Source == "" || req.Target == "" || req.MapPath == "" {
		return report, fmt.Errorf("audit-verify requires --staged, --source, --target, and --map")
	}
	auditPath := req.Audit
	if auditPath == "" {
		auditPath = auditPathFor(req.Staged)
	}
	for _, item := range []struct {
		path string
		dest *string
	}{
		{req.Staged, &report.StagedSHA256},
		{req.Source, &report.SourceSHA256},
		{req.Target, &report.TargetSHA256},
		{auditPath, &report.AuditSHA256},
	} {
		sum, err := hashFile(item.path)
		if err != nil {
			return report, err
		}
		*item.dest = sum
	}
	stagedCheck, err := Check(req.Staged)
	if err != nil {
		return report, err
	}
	sourceCheck, err := Check(req.Source)
	if err != nil {
		return report, err
	}
	targetCheck, err := Check(req.Target)
	if err != nil {
		return report, err
	}
	if stagedCheck == nil || !stagedCheck.Reusable || sourceCheck == nil || !sourceCheck.Reusable || targetCheck == nil || !targetCheck.Reusable {
		report.Reasons = append(report.Reasons, "staged, source, and target must all be reusable chatstorage snapshots")
		return report, fmt.Errorf("audit verify failed")
	}
	accounts, err := loadAccountMap(req.MapPath)
	if err != nil {
		return report, err
	}
	verified, reasons := verifyAccounts(accounts, sourceCheck.Registry, targetCheck.Registry)
	if len(reasons) > 0 || len(verified) != len(accounts) {
		report.Reasons = append(report.Reasons, reasons...)
		return report, fmt.Errorf("audit verify failed")
	}
	sourceToBase := map[string]string{}
	targetToStaged := map[string]string{}
	for _, account := range verified {
		sourceID := account.mapping.SourceStorageJID
		targetID := account.mapping.TargetStorageJID
		if req.Base == "target" {
			sourceToBase[sourceID] = targetID
			targetToStaged[targetID] = targetID
		} else {
			sourceToBase[sourceID] = sourceID
			targetToStaged[targetID] = sourceID
		}
	}
	stagedDB, err := openSQLite(req.Staged, true)
	if err != nil {
		return report, err
	}
	defer stagedDB.Close()
	sourceDB, err := openSQLite(req.Source, true)
	if err != nil {
		return report, err
	}
	defer sourceDB.Close()
	targetDB, err := openSQLite(req.Target, true)
	if err != nil {
		return report, err
	}
	defer targetDB.Close()
	auditDB, err := openSQLite(auditPath, true)
	if err != nil {
		return report, err
	}
	defer auditDB.Close()
	if err := checkAuditSchema(auditDB, report); err != nil {
		report.Reasons = append(report.Reasons, err.Error())
		return report, fmt.Errorf("audit verify failed")
	}
	baseDB := targetDB
	if req.Base == "source" {
		baseDB = sourceDB
	}
	registryOK, err := sameTableFingerprint(stagedDB, baseDB, []string{"devices"})
	if err != nil {
		return report, err
	}
	report.RegistryMatchesBase = registryOK
	if !registryOK {
		report.Reasons = append(report.Reasons, "staged devices registry does not match the "+req.Base+" database")
	}
	stagedRows, err := readHistory(stagedDB, "messages")
	if err != nil {
		return report, err
	}
	sourceRows, err := readHistory(sourceDB, "messages")
	if err != nil {
		return report, err
	}
	targetRows, err := readHistory(targetDB, "messages")
	if err != nil {
		return report, err
	}
	report.StagedMessages = len(stagedRows)
	report.SourceMessages = len(sourceRows)
	report.TargetMessages = len(targetRows)
	stagedByKey := map[string]sqlRow{}
	for _, row := range stagedRows {
		key, err := messageIdentity(row, "")
		if err != nil {
			return report, err
		}
		stagedByKey[key] = row
	}
	alternates, err := loadAuditAlternates(auditDB)
	if err != nil {
		return report, err
	}
	preserved := map[string]struct{}{}
	for _, row := range sourceRows {
		device, chat, id, err := messageParts(row)
		if err != nil {
			return report, err
		}
		baseDevice, ok := sourceToBase[device]
		if !ok {
			report.SourceMissing++
			addMismatch(report, "source", device, chat, id, "unmapped_device")
			continue
		}
		staged, ok := stagedByKey[baseDevice+"\x00"+chat+"\x00"+id]
		if !ok {
			report.SourceMissing++
			addMismatch(report, "source", baseDevice, chat, id, "missing")
			continue
		}
		same, err := samePayload(row, staged)
		if err != nil {
			return report, err
		}
		if !same {
			report.SourcePayloadMismatch++
			addMismatch(report, "source", baseDevice, chat, id, "payload_mismatch")
			continue
		}
		report.SourceRetained++
	}
	for _, row := range targetRows {
		device, chat, id, err := messageParts(row)
		if err != nil {
			return report, err
		}
		stagedDevice := device
		if mapped, ok := targetToStaged[device]; ok {
			stagedDevice = mapped
		} else if req.Base == "target" {
			preserved[device] = struct{}{}
		}
		staged, ok := stagedByKey[stagedDevice+"\x00"+chat+"\x00"+id]
		if !ok {
			report.TargetMissing++
			addMismatch(report, "target", device, chat, id, "missing")
			continue
		}
		same, err := samePayload(row, staged)
		if err != nil {
			return report, err
		}
		if same {
			report.TargetIdentical++
			continue
		}
		if alt, ok := alternates[stagedDevice+"\x00"+chat+"\x00"+id]; ok {
			match, err := auditJSONMatchesRow(alt, row)
			if err != nil {
				return report, err
			}
			if match {
				report.TargetRetainedInAudit++
				continue
			}
		}
		report.TargetMissing++
		addMismatch(report, "target", device, chat, id, "not_in_audit")
	}
	report.PreservedTargetDevices = len(preserved)
	if err := auditCounters(auditDB, report); err != nil {
		return report, err
	}
	if report.RelinkedCollisions > 0 {
		report.Reasons = append(report.Reasons, "audit records a relinked child-key collision")
	}
	if report.InventedParents > 0 {
		report.Reasons = append(report.Reasons, "audit records an invented parent message")
	}
	report.Verified = len(report.Reasons) == 0 && report.SourceMissing == 0 && report.SourcePayloadMismatch == 0 && report.TargetMissing == 0 && report.AuditSchemaOK && report.RegistryMatchesBase
	if !report.Verified {
		return report, fmt.Errorf("audit verify failed")
	}
	return report, nil
}

func addMismatch(report *AuditVerifyReport, side, device, chat, id, problem string) {
	if len(report.Mismatches) >= 20 {
		return
	}
	report.Mismatches = append(report.Mismatches, AuditIdentity{Side: side, DeviceID: device, ChatJID: chat, MessageID: id, Problem: problem})
}

func messageParts(row sqlRow) (string, string, string, error) {
	device, err := rowString(row, "device_id")
	if err != nil {
		return "", "", "", err
	}
	chat, err := rowString(row, "chat_jid")
	if err != nil {
		return "", "", "", err
	}
	id, err := rowString(row, "id")
	if err != nil {
		return "", "", "", err
	}
	return device, chat, id, nil
}

func messageIdentity(row sqlRow, deviceOverride string) (string, error) {
	device, chat, id, err := messageParts(row)
	if err != nil {
		return "", err
	}
	if deviceOverride != "" {
		device = deviceOverride
	}
	return device + "\x00" + chat + "\x00" + id, nil
}

func sameTableFingerprint(left, right *sql.DB, tables []string) (bool, error) {
	a, err := fingerprintTables(left, tables)
	if err != nil {
		return false, err
	}
	b, err := fingerprintTables(right, tables)
	if err != nil {
		return false, err
	}
	return a == b, nil
}

func checkAuditSchema(db *sql.DB, report *AuditVerifyReport) error {
	expected := map[string][]string{
		"audit_policy":     {"key", "value"},
		"message_versions": {"device_id", "chat_jid", "message_id", "origin", "role", "row_json"},
		"orphan_rows":      {"table_name", "device_id", "chat_jid", "message_id", "origin", "retained_in_staged_db", "parent_invented", "row_json"},
		"key_collisions":   {"table_name", "device_id", "kept_chat_jid", "alternate_chat_jid", "message_id", "relinked", "kept_row_json", "alternate_row_json"},
		"archive_messages": {"archive_path", "archive_sha256", "device_id", "chat_jid", "message_id", "imported", "reason", "raw_json"},
	}
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		return err
	}
	defer rows.Close()
	seen := map[string]struct{}{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		seen[name] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for name, cols := range expected {
		if _, ok := seen[name]; !ok {
			return fmt.Errorf("audit schema is missing %s", name)
		}
		got, err := columnNamesQuery(db, name)
		if err != nil {
			return err
		}
		if !sameStringSet(got, cols) {
			return fmt.Errorf("audit table %s columns differ", name)
		}
	}
	var policy, statement, complete string
	if err := db.QueryRow(`SELECT value FROM audit_policy WHERE key='policy'`).Scan(&policy); err != nil {
		return fmt.Errorf("audit policy is missing")
	}
	if err := db.QueryRow(`SELECT value FROM audit_policy WHERE key='statement'`).Scan(&statement); err != nil {
		return err
	}
	if err := db.QueryRow(`SELECT value FROM audit_policy WHERE key='history_complete'`).Scan(&complete); err != nil {
		return err
	}
	if policy != sourceAuthoritativePolicy || statement != auditPolicyText || complete != "false" {
		return fmt.Errorf("audit policy does not match the staged history policy")
	}
	report.AuditSchemaOK = true
	return nil
}

func loadAuditAlternates(db *sql.DB) (map[string]string, error) {
	rows, err := db.Query(`SELECT device_id, chat_jid, message_id, row_json FROM message_versions WHERE origin='target' AND role='retained_alternate'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var device, chat, id, raw string
		if err := rows.Scan(&device, &chat, &id, &raw); err != nil {
			return nil, err
		}
		out[device+"\x00"+chat+"\x00"+id] = raw
	}
	return out, rows.Err()
}

func auditCounters(db *sql.DB, report *AuditVerifyReport) error {
	queries := []struct {
		sql  string
		dest *int
	}{
		{`SELECT COUNT(*) FROM message_versions`, &report.MessageVersions},
		{`SELECT COUNT(*) FROM message_versions WHERE origin='target' AND role='retained_alternate'`, &report.TargetAlternates},
		{`SELECT COUNT(*) FROM key_collisions`, &report.KeyCollisions},
		{`SELECT COUNT(*) FROM key_collisions WHERE relinked!=0`, &report.RelinkedCollisions},
		{`SELECT COUNT(*) FROM orphan_rows`, &report.OrphanRows},
		{`SELECT COUNT(*) FROM orphan_rows WHERE parent_invented!=0`, &report.InventedParents},
		{`SELECT COUNT(*) FROM archive_messages`, &report.ArchiveRows},
	}
	for _, query := range queries {
		if err := db.QueryRow(query.sql).Scan(query.dest); err != nil {
			return err
		}
	}
	return nil
}

func auditJSONMatchesRow(raw string, row sqlRow) (bool, error) {
	encoded, err := privateRowJSON(row)
	if err != nil {
		return false, err
	}
	left, err := canonicalJSON(raw)
	if err != nil {
		return false, err
	}
	right, err := canonicalJSON(encoded)
	if err != nil {
		return false, err
	}
	return left == right, nil
}

func canonicalJSON(raw string) (string, error) {
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return "", err
	}
	normalized, err := json.Marshal(normalizeJSON(value))
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(normalized)
	return hex.EncodeToString(sum[:]), nil
}

func normalizeJSON(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		out := make(map[string]any, len(keys))
		for _, key := range keys {
			out[key] = normalizeJSON(typed[key])
		}
		return out
	case []any:
		for i := range typed {
			typed[i] = normalizeJSON(typed[i])
		}
		return typed
	case float64:
		if typed == float64(int64(typed)) {
			return strconvFormat(typed)
		}
		return typed
	default:
		return value
	}
}

func strconvFormat(value float64) string {
	return strings.TrimSuffix(strings.TrimSuffix(fmt.Sprintf("%.0f", value), ".0"), "")
}
