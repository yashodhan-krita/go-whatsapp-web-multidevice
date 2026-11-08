package dbreuse

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ErrRejected means the report explains a fail-closed result and no staged
// database was published.
var ErrRejected = errors.New("dbreuse: rejected")

var (
	labelPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	phonePattern = regexp.MustCompile(`^[0-9]{6,15}$`)
	lidPattern   = regexp.MustCompile(`^[0-9]{1,20}$`)
)

// AccountMapping is the operator's explicit source-to-target storage JID pair.
// Slot UUIDs are not accepted. The registry on each file must confirm Phone.
type AccountMapping struct {
	Label            string `json:"label"`
	Phone            string `json:"phone"`
	LID              string `json:"lid,omitempty"`
	SourceStorageJID string `json:"source_storage_jid"`
	TargetStorageJID string `json:"target_storage_jid"`
}

type accountFile struct {
	Accounts []AccountMapping `json:"accounts"`
}

// OverlayRequest copies target-only history onto a new snapshot of source.
type OverlayRequest struct {
	Source              string
	Target              string
	MapPath             string
	WorkDir             string
	Out                 string
	AllowLiveValidation bool
	// Base is "source" or "target". Empty means source, which keeps the Mac
	// registry and cannot represent a box-only account. "target" keeps the box
	// registry and session identity and copies mapped incoming history onto it.
	Base string
}

// RowConflict identifies a row whose payload differs. Bodies are not included.
type RowConflict struct {
	Table    string `json:"table"`
	DeviceID string `json:"device_id"`
	ChatJID  string `json:"chat_jid,omitempty"`
	ID       string `json:"id"`
	Class    string `json:"class,omitempty"`
}

// AmbiguousMessage is one message id stored under more than one chat.
type AmbiguousMessage struct {
	DeviceID string   `json:"device_id"`
	ID       string   `json:"id"`
	ChatJIDs []string `json:"chat_jids"`
}

// OrphanRef is a child row whose parent message is absent. The parent is not invented.
type OrphanRef struct {
	Table              string `json:"table"`
	DeviceID           string `json:"device_id"`
	ChatJID            string `json:"chat_jid"`
	MessageID          string `json:"message_id"`
	Origin             string `json:"origin"`
	RetainedInStagedDB bool   `json:"retained_in_staged_db"`
	ParentInvented     bool   `json:"parent_invented"`
}

// ChatDifference is target chat metadata that was not written over the source row.
type ChatDifference struct {
	DeviceID    string `json:"device_id"`
	ChatJID     string `json:"chat_jid"`
	Field       string `json:"field"`
	SourceValue string `json:"source_value"`
	TargetValue string `json:"target_value"`
}

// OverlayReport is the staged copy result. Applied is false when nothing was published.
type OverlayReport struct {
	Applied                bool               `json:"applied"`
	Output                 string             `json:"output,omitempty"`
	SourceSnapshot         string             `json:"source_snapshot,omitempty"`
	TargetSnapshot         string             `json:"target_snapshot,omitempty"`
	InsertedMessages       []MessageRef       `json:"inserted_messages"`
	InsertedChats          int                `json:"inserted_chats"`
	InsertedReactions      int                `json:"inserted_reactions"`
	InsertedEdits          int                `json:"inserted_edits"`
	InsertedPolls          int                `json:"inserted_polls"`
	AlreadyPresent         int                `json:"already_present"`
	Conflicts              []RowConflict      `json:"conflicts"`
	Ambiguous              []AmbiguousMessage `json:"ambiguous"`
	UnmappedDeviceIDs      []string           `json:"unmapped_device_ids"`
	PreservedBaseDeviceIDs []string           `json:"preserved_base_device_ids"`
	Base                   string             `json:"base"`
	ColumnDiffs            []ColumnDiff       `json:"column_diffs"`
	EnrichmentFills        int                `json:"enrichment_fills"`
	Policy                 string             `json:"policy"`
	AuditPath              string             `json:"audit_path,omitempty"`
	DivergentRetained      int                `json:"divergent_retained"`
	Orphans                []OrphanRef        `json:"orphans"`
	AliasInferred          bool               `json:"alias_inferred"`
	ChatDifferences        []ChatDifference   `json:"chat_differences"`
	CutoverReady           bool               `json:"cutover_ready"`
	HistoryComplete        bool               `json:"history_complete"`
	HistoryCompleteReason  string             `json:"history_complete_reason"`
	Reasons                []string           `json:"reasons"`
	Notes                  []string           `json:"notes"`
}

type verifiedAccount struct {
	mapping AccountMapping
}

type planned struct {
	messages      []sqlRow
	chats         []sqlRow
	reactions     []sqlRow
	edits         []sqlRow
	polls         []sqlRow
	fills         []columnFill
	updates       []rowUpdate
	auditMessages []auditMessage
	auditOrphans  []auditOrphan
}

type rowUpdate struct {
	table string
	keys  []string
	row   sqlRow
}

// Overlay writes a new database. Source and target paths are not modified.
// Registry, schedule, and Chatwoot tables are copied from the source snapshot
// and then left unchanged. There is no in-place merge command in GOWA.
func Overlay(ctx context.Context, req OverlayRequest) (*OverlayReport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	report := newOverlayReport()
	if req.Out == "" || req.WorkDir == "" || req.MapPath == "" {
		return report, fmt.Errorf("overlay requires --source, --target, --map, --work-dir, and --out")
	}
	if _, err := os.Lstat(req.Out); err == nil {
		return report, fmt.Errorf("overlay output already exists: %s", req.Out)
	} else if !os.IsNotExist(err) {
		return report, err
	}
	for _, path := range []string{req.Source, req.Target} {
		_, shm, err := siblingFile(path, "-shm")
		if err != nil {
			return report, err
		}
		if shm && !req.AllowLiveValidation {
			return report, fmt.Errorf("%s has -shm; freeze source GOWA, the periodic merge writer, and target GOWA, or pass allow-live-validation for a non-cutover overlay", path)
		}
		if shm {
			report.Notes = append(report.Notes, path+" had -shm; this overlay is a validation copy and can race a later commit")
		}
	}
	accounts, err := loadAccountMap(req.MapPath)
	if err != nil {
		report.Reasons = append(report.Reasons, err.Error())
		return report, errors.Join(ErrRejected, err)
	}
	if err := os.MkdirAll(req.WorkDir, 0o700); err != nil {
		return report, err
	}
	sourceSnap := filepath.Join(req.WorkDir, "source-snapshot.db")
	targetSnap := filepath.Join(req.WorkDir, "target-snapshot.db")
	for _, path := range []string{sourceSnap, targetSnap} {
		os.Remove(path)
		os.Remove(path + "-wal")
		os.Remove(path + "-shm")
	}
	if err := snapshot(ctx, req.Source, sourceSnap); err != nil {
		return report, err
	}
	if err := snapshot(ctx, req.Target, targetSnap); err != nil {
		return report, err
	}
	report.SourceSnapshot = sourceSnap
	report.TargetSnapshot = targetSnap

	srcCheck, err := Check(sourceSnap)
	if err != nil {
		report.Reasons = append(report.Reasons, err.Error())
		return report, err
	}
	dstCheck, err := Check(targetSnap)
	if err != nil {
		report.Reasons = append(report.Reasons, err.Error())
		return report, err
	}
	if !srcCheck.Reusable {
		report.Reasons = append(report.Reasons, "source schema was not accepted: "+strings.Join(srcCheck.Reasons, "; "))
	}
	if !dstCheck.Reusable {
		report.Reasons = append(report.Reasons, "target schema was not accepted: "+strings.Join(dstCheck.Reasons, "; "))
	}
	verified, reason := verifyAccounts(accounts, srcCheck.Registry, dstCheck.Registry)
	report.Reasons = append(report.Reasons, reason...)
	if len(report.Reasons) > 0 {
		return report, ErrRejected
	}

	srcDB, err := openSQLite(sourceSnap, true)
	if err != nil {
		return report, err
	}
	defer srcDB.Close()
	dstDB, err := openSQLite(targetSnap, true)
	if err != nil {
		return report, err
	}
	defer dstDB.Close()

	baseName := req.Base
	if baseName == "" {
		baseName = "source"
	}
	if baseName != "source" && baseName != "target" {
		return report, fmt.Errorf("overlay --base must be source or target")
	}
	report.Base = baseName
	baseDB, incomingDB := srcDB, dstDB
	baseSnap := sourceSnap
	if baseName == "target" {
		baseDB, incomingDB = dstDB, srcDB
		baseSnap = targetSnap
		report.Notes = append(report.Notes, "The staged file copies the target registry, schedules, and Chatwoot rows. Mapped incoming history is added onto that registry. Base accounts that are not in the map stay in place and are not invented on the incoming side.")
	} else {
		report.Notes = append(report.Notes, "Base is the source registry. An account that exists only on the target cannot be mapped and blocks this direction.")
	}
	report.Notes = append(report.Notes, "Policy "+sourceAuthoritativePolicy+": for the same account, chat, and message id, the source row is the staged primary. The other full row is written only to the private audit file. Ids under more than one chat stay separate. No LID alias is inferred.")
	plan, err := buildOverlayPlan(report, baseDB, incomingDB, verified, baseName)
	if err != nil {
		report.Reasons = append(report.Reasons, err.Error())
		return report, err
	}
	if rejected(report) {
		return report, ErrRejected
	}
	report.AuditPath = auditPathFor(req.Out)
	report.DivergentRetained = len(plan.auditMessages) / 2
	if err := writeMigrationAudit(report.AuditPath, plan.auditMessages, plan.auditOrphans); err != nil {
		return report, err
	}
	if err := publishOverlay(ctx, baseSnap, req.Out, plan); err != nil {
		os.Remove(req.Out)
		os.Remove(req.Out + "-wal")
		os.Remove(req.Out + "-shm")
		os.Remove(report.AuditPath)
		report.Reasons = append(report.Reasons, err.Error())
		return report, err
	}
	report.Applied = true
	report.Output = req.Out
	report.CutoverReady = false
	report.HistoryComplete = false
	return report, nil
}

func newOverlayReport() *OverlayReport {
	return &OverlayReport{
		InsertedMessages:       []MessageRef{},
		Conflicts:              []RowConflict{},
		Ambiguous:              []AmbiguousMessage{},
		UnmappedDeviceIDs:      []string{},
		PreservedBaseDeviceIDs: []string{},
		ColumnDiffs:            []ColumnDiff{},
		Orphans:                []OrphanRef{},
		Policy:                 sourceAuthoritativePolicy,
		AliasInferred:          false,
		ChatDifferences:        []ChatDifference{},
		Reasons:                []string{},
		CutoverReady:           false,
		HistoryComplete:        false,
		HistoryCompleteReason:  historyCompleteNote,
		Notes: []string{
			"The output is a new file. Source and target are not modified.",
			"devices, scheduled_sends, and Chatwoot tables are copied from the overlay base and are not edited.",
			"Message device_id is rewritten to the base storage JID. chat_jid and sender are not rewritten.",
			"Reconcile still does not merge. This staged copy is the only overlay, and it is not a cutover by itself.",
			historyCompleteNote,
		},
	}
}

func loadAccountMap(path string) ([]AccountMapping, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Size() > 1<<20 {
		return nil, fmt.Errorf("account map %s is larger than 1MiB", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var file accountFile
	if err := dec.Decode(&file); err != nil {
		return nil, fmt.Errorf("account map: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("account map has trailing data")
	}
	if len(file.Accounts) == 0 {
		return nil, fmt.Errorf("account map has no accounts")
	}
	return file.Accounts, nil
}

func verifyAccounts(accounts []AccountMapping, source, target []RegistrySlot) ([]verifiedAccount, []string) {
	var reasons []string
	var verified []verifiedAccount
	seenSource := map[string]struct{}{}
	seenTarget := map[string]struct{}{}
	seenPhone := map[string]struct{}{}
	for _, account := range accounts {
		if !labelPattern.MatchString(account.Label) {
			reasons = append(reasons, "account label "+account.Label+" is empty or not a short token")
			continue
		}
		if !phonePattern.MatchString(account.Phone) {
			reasons = append(reasons, "account "+account.Label+" phone is not a digit string")
			continue
		}
		if account.LID != "" && !lidPattern.MatchString(account.LID) {
			reasons = append(reasons, "account "+account.Label+" lid is not a digit string")
			continue
		}
		if _, ok := seenPhone[account.Phone]; ok {
			reasons = append(reasons, "phone "+account.Phone+" is mapped more than once")
			continue
		}
		seenPhone[account.Phone] = struct{}{}
		if _, ok := seenSource[account.SourceStorageJID]; ok {
			reasons = append(reasons, "source storage JID "+account.SourceStorageJID+" is mapped more than once")
			continue
		}
		if _, ok := seenTarget[account.TargetStorageJID]; ok {
			reasons = append(reasons, "target storage JID "+account.TargetStorageJID+" is mapped more than once")
			continue
		}
		seenSource[account.SourceStorageJID] = struct{}{}
		seenTarget[account.TargetStorageJID] = struct{}{}
		if reason := verifySide("source", account.Label, account.Phone, account.LID, account.SourceStorageJID, source); reason != "" {
			reasons = append(reasons, reason)
			continue
		}
		if reason := verifySide("target", account.Label, account.Phone, account.LID, account.TargetStorageJID, target); reason != "" {
			reasons = append(reasons, reason)
			continue
		}
		if account.LID != "" && jidUser(account.SourceStorageJID, "lid") != account.LID && jidUser(account.TargetStorageJID, "lid") != account.LID {
			reasons = append(reasons, "account "+account.Label+" declares lid "+account.LID+" but neither storage JID is that lid")
			continue
		}
		verified = append(verified, verifiedAccount{mapping: account})
	}
	return verified, reasons
}

func verifySide(side, label, phone, lid, storage string, slots []RegistrySlot) string {
	if !strings.Contains(storage, "@") {
		return side + " storage id " + storage + " for " + label + " is not a JID; slot UUIDs are not history identity"
	}
	user, server, device, err := splitJID(storage)
	if err != nil || device != 0 {
		return side + " storage JID " + storage + " for " + label + " must be a non-AD phone or LID JID"
	}
	if server != "s.whatsapp.net" && server != "lid" {
		return side + " storage JID " + storage + " for " + label + " has an unsupported server"
	}
	var matched []RegistrySlot
	for _, slot := range slots {
		if slot.StorageID == storage {
			matched = append(matched, slot)
		}
	}
	if len(matched) != 1 {
		return fmt.Sprintf("%s registry has %d rows for storage JID %s", side, len(matched), storage)
	}
	slot := matched[0]
	if strings.TrimSpace(slot.JID) == "" || slot.JID != slot.StorageID {
		return side + " storage id " + storage + " falls back to slot " + slot.DeviceID + "; refusing a slot UUID"
	}
	if reason := confirmIdentity(slot, phone, lid); reason != "" {
		return side + " " + label + ": " + reason
	}
	if user != "" && server == "s.whatsapp.net" && user != phone {
		return side + " " + label + ": storage JID phone does not match the declared phone"
	}
	phones := slotsForPhone(slots, phone)
	if len(phones) != 1 || phones[0].StorageID != storage {
		return fmt.Sprintf("%s phone %s matches %d registry rows; refusing an ambiguous account", side, phone, len(phones))
	}
	return ""
}

func confirmIdentity(slot RegistrySlot, phone, lid string) string {
	user, server, device, err := splitJID(slot.JID)
	if err != nil {
		return "registry jid " + slot.JID + " is not usable"
	}
	if device != 0 {
		return "registry jid must be the storage JID without a device suffix"
	}
	switch server {
	case "s.whatsapp.net":
		if user != phone {
			return "registry jid " + slot.JID + " does not match declared phone " + phone
		}
	case "lid":
		if lid == "" || user != lid {
			return "registry jid " + slot.JID + " does not match declared lid"
		}
	default:
		return "registry jid server " + server + " is not a phone or LID"
	}
	if strings.TrimSpace(slot.ADJID) == "" {
		return "ad_jid is empty, so the phone cannot be confirmed independently of the storage JID"
	}
	adUser, adServer, adDevice, err := splitJID(slot.ADJID)
	if err != nil || adServer != "s.whatsapp.net" || adDevice == 0 || adUser != phone {
		return "ad_jid " + slot.ADJID + " does not confirm phone " + phone
	}
	return ""
}

func jidUser(raw, server string) string {
	user, got, device, err := splitJID(raw)
	if err != nil || got != server || device != 0 {
		return ""
	}
	return user
}

func slotsForPhone(slots []RegistrySlot, phone string) []RegistrySlot {
	var matched []RegistrySlot
	for _, slot := range slots {
		if phoneOf(slot) == phone {
			matched = append(matched, slot)
		}
	}
	return matched
}

func phoneOf(slot RegistrySlot) string {
	if slot.ADJID != "" {
		user, server, _, err := splitJID(slot.ADJID)
		if err == nil && server == "s.whatsapp.net" {
			return user
		}
	}
	user, server, device, err := splitJID(slot.JID)
	if err == nil && server == "s.whatsapp.net" && device == 0 {
		return user
	}
	return ""
}

func splitJID(raw string) (user, server string, device int, err error) {
	raw = strings.TrimSpace(raw)
	at := strings.LastIndex(raw, "@")
	if at <= 0 || at == len(raw)-1 || strings.Count(raw, "@") != 1 {
		return "", "", 0, fmt.Errorf("jid %q", raw)
	}
	server = raw[at+1:]
	left := raw[:at]
	device = 0
	if colon := strings.LastIndex(left, ":"); colon >= 0 {
		user = left[:colon]
		n, convErr := strconv.Atoi(left[colon+1:])
		if convErr != nil || n <= 0 || user == "" {
			return "", "", 0, fmt.Errorf("jid %q", raw)
		}
		device = n
	} else {
		user = left
	}
	if user == "" || strings.ContainsAny(user, ":/@ ") {
		return "", "", 0, fmt.Errorf("jid %q", raw)
	}
	return user, server, device, nil
}

func buildOverlayPlan(report *OverlayReport, srcDB, dstDB queryer, accounts []verifiedAccount, baseName string) (*planned, error) {
	baseIDs := map[string]struct{}{}
	targetOf := map[string]string{}
	for _, account := range accounts {
		incoming, baseID := account.mapping.TargetStorageJID, account.mapping.SourceStorageJID
		if baseName == "target" {
			incoming, baseID = account.mapping.SourceStorageJID, account.mapping.TargetStorageJID
		}
		targetOf[incoming] = baseID
		baseIDs[baseID] = struct{}{}
	}
	srcMessages, err := readHistory(srcDB, "messages")
	if err != nil {
		return nil, err
	}
	dstMessages, err := readHistory(dstDB, "messages")
	if err != nil {
		return nil, err
	}
	if err := sameHistoryColumns(srcMessages, dstMessages, "messages"); err != nil {
		return nil, err
	}
	byKey := map[string]sqlRow{}
	for _, row := range srcMessages {
		key, err := rowKey(row, []string{"device_id", "chat_jid", "id"})
		if err != nil {
			return nil, err
		}
		if _, ok := byKey[key]; ok {
			return nil, fmt.Errorf("duplicate message key %s", key)
		}
		byKey[key] = row
	}
	plan := &planned{}
	idChats := map[string]map[string]struct{}{}
	addChat := func(device, id, chat string) {
		key := device + "\x00" + id
		set := idChats[key]
		if set == nil {
			set = map[string]struct{}{}
			idChats[key] = set
		}
		set[chat] = struct{}{}
	}
	preserved := map[string]struct{}{}
	for _, row := range srcMessages {
		device, err := rowString(row, "device_id")
		if err != nil {
			return nil, err
		}
		if _, ok := baseIDs[device]; !ok {
			preserved[device] = struct{}{}
			continue
		}
		id, err := rowString(row, "id")
		if err != nil {
			return nil, err
		}
		chat, err := rowString(row, "chat_jid")
		if err != nil {
			return nil, err
		}
		addChat(device, id, chat)
	}
	baseEditRows, err := readHistory(srcDB, "message_edits")
	if err != nil {
		return nil, err
	}
	incomingEditRows, err := readHistory(dstDB, "message_edits")
	if err != nil {
		return nil, err
	}
	baseEditIndex, err := indexEdits(baseEditRows)
	if err != nil {
		return nil, err
	}
	incomingEditIndex, err := indexEdits(incomingEditRows)
	if err != nil {
		return nil, err
	}
	unmapped := map[string]struct{}{}
	for _, row := range dstMessages {
		device, err := rowString(row, "device_id")
		if err != nil {
			return nil, err
		}
		sourceDevice, ok := targetOf[device]
		if !ok {
			unmapped[device] = struct{}{}
			continue
		}
		rewritten, err := withString(row, "device_id", sourceDevice)
		if err != nil {
			return nil, err
		}
		id, _ := rowString(rewritten, "id")
		chat, _ := rowString(rewritten, "chat_jid")
		addChat(sourceDevice, id, chat)
		key, err := rowKey(rewritten, []string{"device_id", "chat_jid", "id"})
		if err != nil {
			return nil, err
		}
		existing, found := byKey[key]
		if !found {
			plan.messages = append(plan.messages, rewritten)
			ref, err := messageRef(rewritten)
			if err != nil {
				return nil, err
			}
			report.InsertedMessages = append(report.InsertedMessages, ref)
			byKey[key] = rewritten
			continue
		}
		same, err := samePayload(existing, rewritten)
		if err != nil {
			return nil, err
		}
		if same {
			report.AlreadyPresent++
			continue
		}
		baseContent, _ := rowString(existing, "content")
		incomingContent, _ := rowString(rewritten, "content")
		baseDevice, _ := rowString(existing, "device_id")
		linked := append([][2]string{}, baseEditIndex[baseDevice+"\x00"+chat+"\x00"+id]...)
		linked = append(linked, incomingEditIndex[device+"\x00"+chat+"\x00"+id]...)
		diffs, fills, conflict, err := classifyMessage(existing, rewritten, baseContent, incomingContent, linked)
		if err != nil {
			return nil, err
		}
		report.ColumnDiffs = append(report.ColumnDiffs, diffs...)
		if conflict != nil {
			report.Conflicts = append(report.Conflicts, *conflict)
			retainBoth(plan, existing, row, baseName, sourceDevice, chat, id)
			if baseName == "target" {
				plan.updates = append(plan.updates, rowUpdate{table: "messages", keys: []string{"id", "chat_jid", "device_id"}, row: rewritten})
			}
			continue
		}
		plan.fills = append(plan.fills, fills...)
		report.AlreadyPresent++
	}
	for key, chats := range idChats {
		if len(chats) < 2 {
			continue
		}
		parts := strings.SplitN(key, "\x00", 2)
		var names []string
		for chat := range chats {
			names = append(names, chat)
		}
		sort.Strings(names)
		report.Ambiguous = append(report.Ambiguous, AmbiguousMessage{DeviceID: parts[0], ID: parts[1], ChatJIDs: names})
	}
	sort.Slice(report.Ambiguous, func(i, j int) bool {
		if report.Ambiguous[i].DeviceID == report.Ambiguous[j].DeviceID {
			return report.Ambiguous[i].ID < report.Ambiguous[j].ID
		}
		return report.Ambiguous[i].DeviceID < report.Ambiguous[j].DeviceID
	})

	chats, err := planChats(report, plan, srcDB, dstDB, targetOf, unmapped)
	if err != nil {
		return nil, err
	}
	if err := planChildren(report, plan, srcDB, dstDB, targetOf, unmapped, byKey, baseName); err != nil {
		return nil, err
	}
	for id := range unmapped {
		report.UnmappedDeviceIDs = append(report.UnmappedDeviceIDs, id)
	}
	sort.Strings(report.UnmappedDeviceIDs)
	for id := range preserved {
		report.PreservedBaseDeviceIDs = append(report.PreservedBaseDeviceIDs, id)
	}
	sort.Strings(report.PreservedBaseDeviceIDs)
	report.EnrichmentFills = len(plan.fills)
	if len(report.PreservedBaseDeviceIDs) > 0 {
		report.Notes = append(report.Notes, "Base device ids that are not in the account map stay on the staged copy. They are not invented on the incoming side.")
	}
	if len(report.UnmappedDeviceIDs) > 0 {
		report.Reasons = append(report.Reasons, "incoming history has unmapped device ids that are not in the account map: "+strings.Join(report.UnmappedDeviceIDs, ", "))
	}
	if len(report.Ambiguous) > 0 {
		report.Notes = append(report.Notes, "Message ids that appear under more than one chat are kept as separate rows. No LID or phone alias was inferred.")
	}
	if len(report.Conflicts) > 0 {
		report.Notes = append(report.Notes, "Divergent payloads were not discarded. The source row is the staged primary and both original rows are in the private audit.")
	}
	for _, row := range plan.messages {
		device, err := rowString(row, "device_id")
		if err != nil {
			return nil, err
		}
		chat, err := rowString(row, "chat_jid")
		if err != nil {
			return nil, err
		}
		if _, ok := chats[device+"\x00"+chat]; !ok {
			report.Reasons = append(report.Reasons, "message "+chat+" has no chat row; refusing to invent one")
		}
	}
	sortRefs(report.InsertedMessages)
	return plan, nil
}

func planChats(report *OverlayReport, plan *planned, srcDB, dstDB queryer, targetOf map[string]string, unmapped map[string]struct{}) (map[string]struct{}, error) {
	srcRows, err := readHistory(srcDB, "chats")
	if err != nil {
		return nil, err
	}
	dstRows, err := readHistory(dstDB, "chats")
	if err != nil {
		return nil, err
	}
	if err := sameHistoryColumns(srcRows, dstRows, "chats"); err != nil {
		return nil, err
	}
	existing := map[string]sqlRow{}
	have := map[string]struct{}{}
	for _, row := range srcRows {
		key, err := rowKey(row, []string{"device_id", "jid"})
		if err != nil {
			return nil, err
		}
		existing[key] = row
		have[key] = struct{}{}
	}
	for _, row := range dstRows {
		device, err := rowString(row, "device_id")
		if err != nil {
			return nil, err
		}
		sourceDevice, ok := targetOf[device]
		if !ok {
			unmapped[device] = struct{}{}
			continue
		}
		rewritten, err := withString(row, "device_id", sourceDevice)
		if err != nil {
			return nil, err
		}
		key, err := rowKey(rewritten, []string{"device_id", "jid"})
		if err != nil {
			return nil, err
		}
		current, found := existing[key]
		if !found {
			plan.chats = append(plan.chats, rewritten)
			existing[key] = rewritten
			have[key] = struct{}{}
			continue
		}
		chat, err := rowString(rewritten, "jid")
		if err != nil {
			return nil, err
		}
		for _, field := range []string{"name", "archived", "ephemeral_expiration", "last_message_time"} {
			left, err := canonField(current, field)
			if err != nil {
				return nil, err
			}
			right, err := canonField(rewritten, field)
			if err != nil {
				return nil, err
			}
			if left != right {
				report.ChatDifferences = append(report.ChatDifferences, ChatDifference{
					DeviceID: sourceDevice, ChatJID: chat, Field: field, SourceValue: left, TargetValue: right,
				})
			}
		}
	}
	report.InsertedChats = len(plan.chats)
	return have, nil
}

func planChildren(report *OverlayReport, plan *planned, srcDB, dstDB queryer, targetOf map[string]string, unmapped map[string]struct{}, messages map[string]sqlRow, baseName string) error {
	specs := []struct {
		table       string
		key         []string
		idCol       string
		assign      *[]sqlRow
		counter     *int
		allowOrphan bool
	}{
		{"message_reactions", []string{"message_id", "reactor_jid", "device_id"}, "message_id", &plan.reactions, &report.InsertedReactions, true},
		{"message_edits", []string{"original_message_id", "edit_event_id", "device_id"}, "edit_event_id", &plan.edits, &report.InsertedEdits, false},
		{"poll_definitions", []string{"device_id", "chat_jid", "poll_message_id"}, "poll_message_id", &plan.polls, &report.InsertedPolls, true},
	}
	for _, spec := range specs {
		srcRows, err := readHistory(srcDB, spec.table)
		if err != nil {
			return err
		}
		dstRows, err := readHistory(dstDB, spec.table)
		if err != nil {
			return err
		}
		if err := sameHistoryColumns(srcRows, dstRows, spec.table); err != nil {
			return err
		}
		existing := map[string]sqlRow{}
		for _, row := range srcRows {
			key, err := rowKey(row, spec.key)
			if err != nil {
				return err
			}
			if _, ok := existing[key]; ok {
				return fmt.Errorf("duplicate %s key", spec.table)
			}
			existing[key] = row
			if _, err := recordOrphan(report, plan, spec.table, parentColumn(spec.table), row, messages, baseOriginName(baseName), true); err != nil {
				return err
			}
		}
		for _, row := range dstRows {
			device, err := rowString(row, "device_id")
			if err != nil {
				return err
			}
			sourceDevice, ok := targetOf[device]
			if !ok {
				unmapped[device] = struct{}{}
				continue
			}
			rewritten, err := withString(row, "device_id", sourceDevice)
			if err != nil {
				return err
			}
			key, err := rowKey(rewritten, spec.key)
			if err != nil {
				return err
			}
			chat, _ := rowString(rewritten, "chat_jid")
			id, _ := rowString(rewritten, spec.idCol)
			messageID := id
			if spec.table == "message_reactions" || spec.table == "message_edits" {
				col := "message_id"
				if spec.table == "message_edits" {
					col = "original_message_id"
				}
				messageID, err = rowString(rewritten, col)
				if err != nil {
					return err
				}
			}
			parentKey := sourceDevice + "\x00" + chat + "\x00" + messageID
			if _, ok := messages[parentKey]; !ok {
				if _, err := recordOrphan(report, plan, spec.table, parentColumn(spec.table), row, nil, incomingOriginName(baseName), false); err != nil {
					return err
				}
				if spec.allowOrphan {
					*spec.assign = append(*spec.assign, rewritten)
					report.Orphans[len(report.Orphans)-1].RetainedInStagedDB = true
					plan.auditOrphans[len(plan.auditOrphans)-1].inStaged = true
				}
				continue
			}
			current, found := existing[key]
			if !found {
				*spec.assign = append(*spec.assign, rewritten)
				existing[key] = rewritten
				continue
			}
			same, err := samePayload(current, rewritten)
			if err != nil {
				return err
			}
			if !same {
				provenanceOnly, err := onlyProvenanceDiff(current, rewritten)
				if err != nil {
					return err
				}
				if provenanceOnly {
					continue
				}
				report.Conflicts = append(report.Conflicts, RowConflict{Table: spec.table, DeviceID: sourceDevice, ChatJID: chat, ID: id, Class: "genuine_content"})
				retainBoth(plan, current, row, baseName, sourceDevice, chat, id)
				if baseName == "target" {
					plan.updates = append(plan.updates, rowUpdate{table: spec.table, keys: spec.key, row: rewritten})
				}
			}
		}
		*spec.counter = len(*spec.assign)
	}
	return nil
}

func readHistory(db queryer, table string) ([]sqlRow, error) {
	cols, err := columnNamesQuery(db, table)
	if err != nil {
		return nil, err
	}
	return loadRowsQuery(db, table, cols)
}

func sameHistoryColumns(src, dst []sqlRow, table string) error {
	if len(src) == 0 || len(dst) == 0 {
		return nil
	}
	if !sameStrings(src[0].cols, dst[0].cols) {
		return fmt.Errorf("%s columns differ; refusing to drop fields", table)
	}
	return nil
}

func samePayload(left, right sqlRow) (bool, error) {
	a, err := rowHash(left, "device_id")
	if err != nil {
		return false, err
	}
	b, err := rowHash(right, "device_id")
	if err != nil {
		return false, err
	}
	return a == b, nil
}

func canonField(row sqlRow, name string) (string, error) {
	val, ok := rowField(row, name)
	if !ok {
		return "", fmt.Errorf("column %s is missing", name)
	}
	return canonValue(val)
}

func messageRef(row sqlRow) (MessageRef, error) {
	var ref MessageRef
	var err error
	if ref.ID, err = rowString(row, "id"); err != nil {
		return ref, err
	}
	if ref.ChatJID, err = rowString(row, "chat_jid"); err != nil {
		return ref, err
	}
	if ref.DeviceID, err = rowString(row, "device_id"); err != nil {
		return ref, err
	}
	if ref.Timestamp, err = rowString(row, "timestamp"); err != nil {
		return ref, err
	}
	return ref, nil
}

func rejected(report *OverlayReport) bool {
	return len(report.Reasons) > 0 || len(report.UnmappedDeviceIDs) > 0
}

func baseOriginName(baseName string) string {
	if baseName == "target" {
		return "target"
	}
	return "source"
}

func incomingOriginName(baseName string) string {
	if baseName == "target" {
		return "source"
	}
	return "target"
}

func parentColumn(table string) string {
	switch table {
	case "message_edits":
		return "original_message_id"
	case "poll_definitions":
		return "poll_message_id"
	default:
		return "message_id"
	}
}

func recordOrphan(report *OverlayReport, plan *planned, table, idCol string, row sqlRow, messages map[string]sqlRow, origin string, inStaged bool) (bool, error) {
	device, err := rowString(row, "device_id")
	if err != nil {
		return false, err
	}
	chat, err := rowString(row, "chat_jid")
	if err != nil {
		return false, err
	}
	messageID, err := rowString(row, idCol)
	if err != nil {
		return false, err
	}
	if messages != nil {
		if _, ok := messages[device+"\x00"+chat+"\x00"+messageID]; ok {
			return false, nil
		}
	}
	report.Orphans = append(report.Orphans, OrphanRef{
		Table: table, DeviceID: device, ChatJID: chat, MessageID: messageID,
		Origin: origin, RetainedInStagedDB: inStaged, ParentInvented: false,
	})
	plan.auditOrphans = append(plan.auditOrphans, auditOrphan{
		table: table, device: device, chat: chat, id: messageID, origin: origin, row: row, inStaged: inStaged,
	})
	return true, nil
}

func updateRow(tx *sql.Tx, update rowUpdate) error {
	table, err := quoteIdent(update.table)
	if err != nil {
		return err
	}
	keys := map[string]struct{}{}
	for _, key := range update.keys {
		keys[key] = struct{}{}
	}
	var sets []string
	var args []any
	for i, col := range update.row.cols {
		if _, ok := keys[col]; ok {
			continue
		}
		quoted, err := quoteIdent(col)
		if err != nil {
			return err
		}
		sets = append(sets, quoted+"=?")
		args = append(args, update.row.vals[i])
	}
	var where []string
	for _, key := range update.keys {
		quoted, err := quoteIdent(key)
		if err != nil {
			return err
		}
		value, ok := rowField(update.row, key)
		if !ok {
			return fmt.Errorf("update %s is missing %s", update.table, key)
		}
		where = append(where, quoted+"=?")
		args = append(args, value)
	}
	if len(sets) == 0 {
		return nil
	}
	res, err := tx.Exec(`UPDATE `+table+` SET `+strings.Join(sets, ", ")+` WHERE `+strings.Join(where, " AND "), args...)
	if err != nil {
		return fmt.Errorf("update %s: %w", update.table, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("update %s matched %d rows", update.table, n)
	}
	return nil
}

func retainBoth(plan *planned, baseRow, incoming sqlRow, baseName, device, chat, id string) {
	baseOrigin, incomingOrigin := "source", "target"
	if baseName == "target" {
		baseOrigin, incomingOrigin = "target", "source"
	}
	add := func(row sqlRow, origin string) {
		role := "retained_alternate"
		if origin == "source" {
			role = "primary"
		}
		plan.auditMessages = append(plan.auditMessages, auditMessage{
			device: device, chat: chat, id: id, origin: origin, role: role, row: row,
		})
	}
	add(baseRow, baseOrigin)
	add(incoming, incomingOrigin)
}

func publishOverlay(ctx context.Context, sourceSnap, out string, plan *planned) error {
	if err := snapshot(ctx, sourceSnap, out); err != nil {
		return err
	}
	db, err := openSQLite(out, false)
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.Exec(`PRAGMA foreign_keys=ON`); err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	before, err := fingerprintTables(tx, preservedTables)
	if err != nil {
		tx.Rollback()
		return err
	}
	batches := []struct {
		table string
		rows  []sqlRow
	}{
		{"chats", plan.chats},
		{"messages", plan.messages},
		{"message_reactions", plan.reactions},
		{"message_edits", plan.edits},
		{"poll_definitions", plan.polls},
	}
	for _, batch := range batches {
		for _, row := range batch.rows {
			if err := insertRow(tx, batch.table, row); err != nil {
				tx.Rollback()
				return fmt.Errorf("insert %s: %w", batch.table, err)
			}
		}
	}
	for _, update := range plan.updates {
		if err := updateRow(tx, update); err != nil {
			tx.Rollback()
			return err
		}
	}
	for _, fill := range plan.fills {
		if !enrichmentColumn(fill.column) {
			tx.Rollback()
			return fmt.Errorf("refusing to fill %s", fill.column)
		}
		quoted, err := quoteIdent(fill.column)
		if err != nil {
			tx.Rollback()
			return err
		}
		res, err := tx.Exec(`UPDATE messages SET `+quoted+` = ? WHERE id = ? AND chat_jid = ? AND device_id = ?`, fill.value, fill.id, fill.chat, fill.device)
		if err != nil {
			tx.Rollback()
			return fmt.Errorf("fill %s: %w", fill.column, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			tx.Rollback()
			return err
		}
		if n != 1 {
			tx.Rollback()
			return fmt.Errorf("fill %s matched %d rows", fill.column, n)
		}
	}
	after, err := fingerprintTables(tx, preservedTables)
	if err != nil {
		tx.Rollback()
		return err
	}
	if before != after {
		tx.Rollback()
		return fmt.Errorf("registry, schedule, or Chatwoot rows changed")
	}
	return tx.Commit()
}

var preservedTables = []string{
	"schema_info",
	"devices",
	"scheduled_sends",
	"chatwoot_device_configs",
	"chatwoot_message_links",
	"chatwoot_forward_queue",
}
