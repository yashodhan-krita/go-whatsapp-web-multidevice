package dbreuse

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

// ArchiveMessage is one history-sync id. The message body is not retained.
type ArchiveMessage struct {
	ChatJID string `json:"chat_jid"`
	ID      string `json:"id"`
}

// ArchiveCoverage is the overlap between one JSON archive and a snapshot.
type ArchiveCoverage struct {
	Path               string           `json:"path"`
	Bytes              int64            `json:"bytes"`
	SHA256             string           `json:"sha256"`
	Kind               string           `json:"kind"`
	MessageAudit       bool             `json:"message_audit"`
	Conversations      int              `json:"conversations"`
	EmptyConversations int              `json:"empty_conversations"`
	TopLevel           []string         `json:"top_level"`
	Messages           int              `json:"messages"`
	Present            int              `json:"present"`
	Absent             int              `json:"absent"`
	OtherChat          int              `json:"other_chat"`
	AbsentIDs          []ArchiveMessage `json:"absent_ids"`
	OtherChats         []ArchiveMessage `json:"other_chats"`
	Notes              []string         `json:"notes"`
}

// CoverageReport lists archive ids that are already in the snapshot.
// HistoryComplete is always false, including when every id is present.
type CoverageReport struct {
	Database              string            `json:"database"`
	Archives              []ArchiveCoverage `json:"archives"`
	HistoryComplete       bool              `json:"history_complete"`
	HistoryCompleteReason string            `json:"history_complete_reason"`
	OldBridgeAssessed     bool              `json:"old_bridge_assessed"`
	Notes                 []string          `json:"notes"`
}

// Coverage compares GOWA history-sync JSON files with a chatstorage snapshot.
// It does not insert rows. Unrecognized files fail closed.
func Coverage(dbPath string, archives []string) (*CoverageReport, error) {
	report := &CoverageReport{
		Database:              dbPath,
		Archives:              []ArchiveCoverage{},
		HistoryComplete:       false,
		OldBridgeAssessed:     false,
		HistoryCompleteReason: historyCompleteNote + " An archive id that is already stored does not prove media, reactions, or older bridge rows, and it does not authorize a purge.",
		Notes: []string{
			"This is not an importer. Absent ids stay only in the archive file.",
			"No old-bridge row format is read. A SQLite file or gzip stream is rejected. That gap is unverified coverage, not permission to purge.",
			"A history-sync file with no conversations field is metadata-only when every top-level key is a known history-sync field. Zero absent ids in that file is not message coverage.",
			"A conversation without a messages field is empty. It is counted and is not a malformed archive.",
			historyCompleteNote,
		},
	}
	if len(archives) == 0 {
		return report, fmt.Errorf("coverage requires at least one --archive")
	}
	checked, err := Check(dbPath)
	if err != nil {
		return report, err
	}
	if checked == nil || !checked.Reusable {
		reason := "schema was not accepted"
		if checked != nil && len(checked.Reasons) > 0 {
			reason = strings.Join(checked.Reasons, "; ")
		}
		return report, fmt.Errorf("coverage refused the snapshot: %s", reason)
	}
	db, err := openSQLite(dbPath, true)
	if err != nil {
		return report, err
	}
	defer db.Close()
	byPair, byID, err := loadMessageLocations(db)
	if err != nil {
		return report, err
	}
	for _, path := range archives {
		one, err := coverArchive(path, byPair, byID)
		if one != nil {
			report.Archives = append(report.Archives, *one)
		}
		if err != nil {
			report.HistoryComplete = false
			return report, err
		}
	}
	report.HistoryComplete = false
	return report, nil
}

func loadMessageLocations(db *sql.DB) (map[string]struct{}, map[string][]string, error) {
	rows, err := db.Query(`SELECT id, chat_jid FROM messages`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	byPair := map[string]struct{}{}
	seenChat := map[string]map[string]struct{}{}
	for rows.Next() {
		var id, chat string
		if err := rows.Scan(&id, &chat); err != nil {
			return nil, nil, err
		}
		byPair[chat+"\x00"+id] = struct{}{}
		set := seenChat[id]
		if set == nil {
			set = map[string]struct{}{}
			seenChat[id] = set
		}
		set[chat] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	byID := map[string][]string{}
	for id, chats := range seenChat {
		for chat := range chats {
			byID[id] = append(byID[id], chat)
		}
		sort.Strings(byID[id])
	}
	return byPair, byID, nil
}

func coverArchive(path string, byPair map[string]struct{}, byID map[string][]string) (*ArchiveCoverage, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	sum, err := hashFile(path)
	if err != nil {
		return nil, err
	}
	parsed, err := readHistoryArchive(path)
	out := &ArchiveCoverage{
		Path: path, Bytes: info.Size(), SHA256: sum,
		AbsentIDs: []ArchiveMessage{}, OtherChats: []ArchiveMessage{}, Notes: []string{}, TopLevel: []string{},
	}
	if err != nil {
		return out, err
	}
	out.Kind = parsed.Kind
	out.MessageAudit = parsed.MessageAudit
	out.Conversations = parsed.Conversations
	out.EmptyConversations = parsed.EmptyConversations
	out.TopLevel = parsed.TopLevel
	if !parsed.MessageAudit {
		out.Notes = append(out.Notes, "metadata-only history sync: conversation messages were not audited, so zero absent ids is not coverage and does not authorize a purge")
	}
	if parsed.StatusMessagesPresent {
		out.Notes = append(out.Notes, "statusV3Messages is present and was not audited as chat history")
	}
	if parsed.EmptyConversations > 0 {
		out.Notes = append(out.Notes, "conversations without messages are counted separately and are not message coverage")
	}
	for _, message := range parsed.Messages {
		out.Messages++
		if _, ok := byPair[message.ChatJID+"\x00"+message.ID]; ok {
			out.Present++
			continue
		}
		if chats := byID[message.ID]; len(chats) > 0 {
			out.OtherChat++
			out.OtherChats = append(out.OtherChats, message)
			continue
		}
		out.Absent++
		out.AbsentIDs = append(out.AbsentIDs, message)
	}
	sort.Slice(out.AbsentIDs, func(i, j int) bool {
		if out.AbsentIDs[i].ChatJID == out.AbsentIDs[j].ChatJID {
			return out.AbsentIDs[i].ID < out.AbsentIDs[j].ID
		}
		return out.AbsentIDs[i].ChatJID < out.AbsentIDs[j].ChatJID
	})
	sort.Slice(out.OtherChats, func(i, j int) bool {
		if out.OtherChats[i].ChatJID == out.OtherChats[j].ChatJID {
			return out.OtherChats[i].ID < out.OtherChats[j].ID
		}
		return out.OtherChats[i].ChatJID < out.OtherChats[j].ChatJID
	})
	return out, nil
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

type historyArchive struct {
	Kind                  string
	MessageAudit          bool
	Conversations         int
	EmptyConversations    int
	TopLevel              []string
	Messages              []ArchiveMessage
	Payloads              []archivePayload
	StatusMessagesPresent bool
}

// archivePayload keeps one raw history message for the private audit.
// Coverage reports do not include it.
type archivePayload struct {
	ChatJID string
	ID      string
	Raw     json.RawMessage
}

func knownHistoryKey(key string) bool {
	switch key {
	case "syncType", "conversations", "statusV3Messages", "chunkOrder", "progress",
		"pushnames", "globalSettings", "threadIDUserSecret", "threadDsTimeframeOffset",
		"recentStickers", "pastParticipants", "callLogRecords", "aiWaitListState",
		"phoneNumberToLidMappings", "companionMetaNonce", "shareableChatIdentifierEncryptionKey",
		"accounts", "nctSalt", "inlineContacts", "inlineContactsProvided", "favoriteStickers",
		"omittedConversations", "omittedConversationsProvided":
		return true
	default:
		return false
	}
}

func readHistoryArchive(path string) (*historyArchive, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	head := make([]byte, 16)
	n, readErr := io.ReadFull(f, head)
	if readErr != nil && readErr != io.ErrUnexpectedEOF && readErr != io.EOF {
		return nil, readErr
	}
	if n >= 2 && head[0] == 0x1f && head[1] == 0x8b {
		return nil, fmt.Errorf("%s is gzip; refusing to guess an archive format", path)
	}
	if n >= 16 && string(head[:16]) == "SQLite format 3\x00" {
		return nil, fmt.Errorf("%s is a SQLite database, not a history JSON archive", path)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(f)
	dec.UseNumber()
	token, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("%s is not a GOWA history-sync object: %w", path, err)
	}
	delim, ok := token.(json.Delim)
	if !ok || delim != '{' {
		return nil, fmt.Errorf("%s is not a GOWA history-sync object", path)
	}
	parsed := &historyArchive{Kind: "metadata_only", TopLevel: []string{}, Messages: []ArchiveMessage{}}
	sawConversations := false
	for dec.More() {
		keyToken, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, fmt.Errorf("%s has a non-string key", path)
		}
		if !knownHistoryKey(key) {
			return nil, fmt.Errorf("%s has unrecognized key %q", path, key)
		}
		if key == "statusV3Messages" {
			parsed.StatusMessagesPresent = true
		}
		parsed.TopLevel = append(parsed.TopLevel, key)
		if key != "conversations" {
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return nil, fmt.Errorf("%s: %w", path, err)
			}
			continue
		}
		if sawConversations {
			return nil, fmt.Errorf("%s repeats conversations", path)
		}
		sawConversations = true
		if err := decodeConversations(path, dec, parsed); err != nil {
			return nil, err
		}
	}
	end, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if delim, ok := end.(json.Delim); !ok || delim != '}' {
		return nil, fmt.Errorf("%s is not a GOWA history-sync object", path)
	}
	if len(parsed.TopLevel) == 0 {
		return nil, fmt.Errorf("%s is not a GOWA history-sync object", path)
	}
	sort.Strings(parsed.TopLevel)
	if sawConversations {
		parsed.Kind = "messages"
		parsed.MessageAudit = true
	}
	return parsed, nil
}

func decodeConversations(path string, dec *json.Decoder, parsed *historyArchive) error {
	token, err := dec.Token()
	if err != nil {
		return err
	}
	if delim, ok := token.(json.Delim); !ok || delim != '[' {
		return fmt.Errorf("%s conversations is not an array", path)
	}
	for dec.More() {
		var conv map[string]json.RawMessage
		if err := dec.Decode(&conv); err != nil {
			return fmt.Errorf("%s conversation: %w", path, err)
		}
		parsed.Conversations++
		chat, err := conversationID(conv)
		if err != nil {
			return fmt.Errorf("%s conversation: %w", path, err)
		}
		rawMessages, hasMessages := conv["messages"]
		if !hasMessages || string(rawMessages) == "null" {
			parsed.EmptyConversations++
			continue
		}
		if len(rawMessages) == 0 || rawMessages[0] != '[' {
			return fmt.Errorf("%s conversation %s messages is not an array", path, chat)
		}
		var items []struct {
			Message struct {
				Key struct {
					RemoteJID string `json:"remoteJid"`
					ID        string `json:"id"`
				} `json:"key"`
			} `json:"message"`
		}
		var rawItems []json.RawMessage
		if err := json.Unmarshal(rawMessages, &items); err != nil {
			return fmt.Errorf("%s conversation %s: %w", path, chat, err)
		}
		if err := json.Unmarshal(rawMessages, &rawItems); err != nil {
			return fmt.Errorf("%s conversation %s: %w", path, chat, err)
		}
		if len(items) != len(rawItems) {
			return fmt.Errorf("%s conversation %s message list could not be retained", path, chat)
		}
		if len(items) == 0 {
			parsed.EmptyConversations++
			continue
		}
		for i, item := range items {
			id := item.Message.Key.ID
			remote := item.Message.Key.RemoteJID
			if id == "" {
				return fmt.Errorf("%s has a message without key.id", path)
			}
			messageChat := chat
			if messageChat == "" {
				messageChat = remote
			}
			if messageChat == "" {
				return fmt.Errorf("%s message %s has no chat jid", path, id)
			}
			if remote != "" && chat != "" && remote != chat {
				return fmt.Errorf("%s message %s chat id %s disagrees with remoteJid %s", path, id, chat, remote)
			}
			parsed.Messages = append(parsed.Messages, ArchiveMessage{ChatJID: messageChat, ID: id})
			parsed.Payloads = append(parsed.Payloads, archivePayload{ChatJID: messageChat, ID: id, Raw: append(json.RawMessage(nil), rawItems[i]...)})
		}
	}
	end, err := dec.Token()
	if err != nil {
		return err
	}
	if delim, ok := end.(json.Delim); !ok || delim != ']' {
		return fmt.Errorf("%s conversations did not end", path)
	}
	return nil
}

func conversationID(conv map[string]json.RawMessage) (string, error) {
	primary, hasPrimary := conv["ID"]
	alt, hasAlt := conv["id"]
	if !hasPrimary && !hasAlt {
		return "", nil
	}
	read := func(raw json.RawMessage) (string, error) {
		var id string
		if err := json.Unmarshal(raw, &id); err != nil {
			return "", fmt.Errorf("chat id is not a string")
		}
		return id, nil
	}
	if hasPrimary && hasAlt {
		left, err := read(primary)
		if err != nil {
			return "", err
		}
		right, err := read(alt)
		if err != nil {
			return "", err
		}
		if left != right {
			return "", fmt.Errorf("ID and id disagree")
		}
		return left, nil
	}
	if hasPrimary {
		return read(primary)
	}
	return read(alt)
}
