package dbreuse

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// MessageRef identifies one stored message without its body or media keys.
type MessageRef struct {
	DeviceID  string `json:"device_id"`
	ChatJID   string `json:"chat_jid"`
	ID        string `json:"id"`
	Timestamp string `json:"timestamp"`
}

// ChatSummary counts one chat on each side.
type ChatSummary struct {
	DeviceID    string `json:"device_id"`
	ChatJID     string `json:"chat_jid"`
	SourceCount int    `json:"source_count"`
	TargetCount int    `json:"target_count"`
	SourceMin   string `json:"source_min,omitempty"`
	SourceMax   string `json:"source_max,omitempty"`
	TargetMin   string `json:"target_min,omitempty"`
	TargetMax   string `json:"target_max,omitempty"`
}

// ReconcileReport compares two chatstorage files. Neither file is modified.
// Target-only rows stay in the target file; they are not copied.
type ReconcileReport struct {
	SourceSnapshot        string        `json:"source_snapshot"`
	TargetSnapshot        string        `json:"target_snapshot"`
	HistoryComplete       bool          `json:"history_complete"`
	HistoryCompleteReason string        `json:"history_complete_reason"`
	SourceOnly            []MessageRef  `json:"source_only"`
	TargetOnly            []MessageRef  `json:"target_only"`
	Changed               []MessageRef  `json:"changed"`
	SameCount             int           `json:"same_count"`
	Chats                 []ChatSummary `json:"chats"`
	Notes                 []string      `json:"notes"`
}

type storedMessage struct {
	ref  MessageRef
	hash string
}

// Reconcile snapshots both databases into workDir and compares message keys.
func Reconcile(ctx context.Context, source, target, workDir string) (*ReconcileReport, error) {
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		return nil, err
	}
	sourceSnap := filepath.Join(workDir, "source-snapshot.db")
	targetSnap := filepath.Join(workDir, "target-snapshot.db")
	for _, path := range []string{sourceSnap, targetSnap} {
		os.Remove(path)
		os.Remove(path + "-wal")
		os.Remove(path + "-shm")
	}
	if err := Snapshot(ctx, source, sourceSnap); err != nil {
		return nil, err
	}
	if err := Snapshot(ctx, target, targetSnap); err != nil {
		return nil, err
	}
	report := &ReconcileReport{
		SourceSnapshot:        sourceSnap,
		TargetSnapshot:        targetSnap,
		SourceOnly:            []MessageRef{},
		TargetOnly:            []MessageRef{},
		Changed:               []MessageRef{},
		Chats:                 []ChatSummary{},
		HistoryComplete:       false,
		HistoryCompleteReason: historyCompleteNote + " Target-only rows remain in the target snapshot and are not merged.",
		Notes: []string{
			"Adopting the source snapshot as the runtime chatstorage.db leaves target-only messages in the target snapshot only.",
			"This comparison uses the stored device_id, chat_jid, and message id. It does not rewrite JIDs.",
		},
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

	srcRows, err := loadMessages(srcDB)
	if err != nil {
		return report, err
	}
	dstRows, err := loadMessages(dstDB)
	if err != nil {
		return report, err
	}
	chats := map[string]*ChatSummary{}
	for key, row := range srcRows {
		chat := chatBucket(chats, row.ref)
		chat.SourceCount++
		chat.SourceMin = earlier(chat.SourceMin, row.ref.Timestamp)
		chat.SourceMax = later(chat.SourceMax, row.ref.Timestamp)
		other, ok := dstRows[key]
		if !ok {
			report.SourceOnly = append(report.SourceOnly, row.ref)
			continue
		}
		if other.hash != row.hash {
			report.Changed = append(report.Changed, row.ref)
			continue
		}
		report.SameCount++
	}
	for key, row := range dstRows {
		chat := chatBucket(chats, row.ref)
		chat.TargetCount++
		chat.TargetMin = earlier(chat.TargetMin, row.ref.Timestamp)
		chat.TargetMax = later(chat.TargetMax, row.ref.Timestamp)
		if _, ok := srcRows[key]; !ok {
			report.TargetOnly = append(report.TargetOnly, row.ref)
		}
	}
	sortRefs(report.SourceOnly)
	sortRefs(report.TargetOnly)
	sortRefs(report.Changed)
	for _, chat := range chats {
		report.Chats = append(report.Chats, *chat)
	}
	sort.Slice(report.Chats, func(i, j int) bool {
		if report.Chats[i].DeviceID == report.Chats[j].DeviceID {
			return report.Chats[i].ChatJID < report.Chats[j].ChatJID
		}
		return report.Chats[i].DeviceID < report.Chats[j].DeviceID
	})
	return report, nil
}

func loadMessages(db *sql.DB) (map[string]storedMessage, error) {
	rows, err := db.Query(`
		SELECT id, chat_jid, device_id, COALESCE(sender, ''), COALESCE(content, ''),
			COALESCE(timestamp, ''), COALESCE(is_from_me, 0), COALESCE(media_type, ''),
			COALESCE(hex(file_sha256), '')
		FROM messages`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]storedMessage{}
	for rows.Next() {
		var ref MessageRef
		var sender, content, fromMe, mediaType, fileHash string
		if err := rows.Scan(&ref.ID, &ref.ChatJID, &ref.DeviceID, &sender, &content, &ref.Timestamp, &fromMe, &mediaType, &fileHash); err != nil {
			return nil, err
		}
		sum := sha256.Sum256([]byte(strings.Join([]string{sender, content, fromMe, mediaType, fileHash}, "\x00")))
		out[ref.DeviceID+"\x00"+ref.ChatJID+"\x00"+ref.ID] = storedMessage{ref: ref, hash: hex.EncodeToString(sum[:])}
	}
	return out, rows.Err()
}

func chatBucket(chats map[string]*ChatSummary, ref MessageRef) *ChatSummary {
	key := ref.DeviceID + "\x00" + ref.ChatJID
	chat, ok := chats[key]
	if !ok {
		chat = &ChatSummary{DeviceID: ref.DeviceID, ChatJID: ref.ChatJID}
		chats[key] = chat
	}
	return chat
}

func earlier(current, next string) string {
	if current == "" || (next != "" && next < current) {
		return next
	}
	return current
}

func later(current, next string) string {
	if next > current {
		return next
	}
	return current
}

func sortRefs(refs []MessageRef) {
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].DeviceID != refs[j].DeviceID {
			return refs[i].DeviceID < refs[j].DeviceID
		}
		if refs[i].ChatJID != refs[j].ChatJID {
			return refs[i].ChatJID < refs[j].ChatJID
		}
		return refs[i].ID < refs[j].ID
	})
}
