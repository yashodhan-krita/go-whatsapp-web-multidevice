package dbreuse

import "sort"

// ColumnDiff describes one column that differs between the overlay base and
// the incoming row. Values, message bodies, and media keys are not included.
type ColumnDiff struct {
	Table             string `json:"table"`
	ID                string `json:"id"`
	ChatJID           string `json:"chat_jid"`
	DeviceID          string `json:"device_id"`
	Column            string `json:"column"`
	Class             string `json:"class"`
	Resolution        string `json:"resolution"`
	BaseEmpty         bool   `json:"base_empty"`
	IncomingEmpty     bool   `json:"incoming_empty"`
	BaseTimestamp     string `json:"base_timestamp,omitempty"`
	IncomingTimestamp string `json:"incoming_timestamp,omitempty"`
}

type columnFill struct {
	id     string
	chat   string
	device string
	column string
	value  any
}

func onlyProvenanceDiff(base, incoming sqlRow) (bool, error) {
	names := append([]string{}, base.cols...)
	sort.Strings(names)
	differed := false
	for _, name := range names {
		if name == "device_id" {
			continue
		}
		baseValue, ok := rowField(base, name)
		if !ok {
			return false, errMissing(name)
		}
		incomingValue, ok := rowField(incoming, name)
		if !ok {
			return false, errMissing(name)
		}
		equal, err := valuesEqual(baseValue, incomingValue)
		if err != nil {
			return false, err
		}
		if equal {
			continue
		}
		if name != "created_at" && name != "updated_at" {
			return false, nil
		}
		differed = true
	}
	return differed, nil
}

func enrichmentColumn(name string) bool {
	switch name {
	case "id", "chat_jid", "device_id", "content", "sender", "is_from_me",
		"media_key", "file_sha256", "file_enc_sha256", "timestamp", "created_at", "updated_at":
		return false
	default:
		return true
	}
}

func classifyMessage(base, incoming sqlRow, baseContent, incomingContent string, edits [][2]string) ([]ColumnDiff, []columnFill, *RowConflict, error) {
	id, err := rowString(base, "id")
	if err != nil {
		return nil, nil, nil, err
	}
	chat, err := rowString(base, "chat_jid")
	if err != nil {
		return nil, nil, nil, err
	}
	device, err := rowString(base, "device_id")
	if err != nil {
		return nil, nil, nil, err
	}
	names := append([]string{}, base.cols...)
	sort.Strings(names)
	var diffs []ColumnDiff
	var fills []columnFill
	blocking := ""
	for _, name := range names {
		if name == "id" || name == "chat_jid" || name == "device_id" {
			continue
		}
		baseValue, ok := rowField(base, name)
		if !ok {
			return nil, nil, nil, errMissing(name)
		}
		incomingValue, ok := rowField(incoming, name)
		if !ok {
			return nil, nil, nil, errMissing(name)
		}
		equal, err := valuesEqual(baseValue, incomingValue)
		if err != nil {
			return nil, nil, nil, err
		}
		if equal {
			continue
		}
		diff := ColumnDiff{
			Table: "messages", ID: id, ChatJID: chat, DeviceID: device, Column: name,
			BaseEmpty: isEmptyValue(baseValue), IncomingEmpty: isEmptyValue(incomingValue),
		}
		switch {
		case name == "created_at" || name == "updated_at":
			diff.Class = "provenance"
			diff.Resolution = "kept_base"
		case name == "timestamp":
			diff.Class = "timestamp"
			diff.Resolution = "kept_base"
			diff.BaseTimestamp, _ = rowString(base, "timestamp")
			diff.IncomingTimestamp, _ = rowString(incoming, "timestamp")
		case name == "content" || name == "sender" || name == "is_from_me" || name == "media_key" || name == "file_sha256" || name == "file_enc_sha256":
			diff.Resolution = "unresolved"
			if name == "content" && contentsLinked(baseContent, incomingContent, edits) {
				diff.Class = "edit_divergence"
			} else {
				diff.Class = "genuine_content"
			}
			blocking = raiseBlock(blocking, diff.Class)
		case diff.BaseEmpty && !diff.IncomingEmpty && enrichmentColumn(name):
			diff.Class = "enrichment"
			diff.Resolution = "filled_from_incoming"
			fills = append(fills, columnFill{id: id, chat: chat, device: device, column: name, value: incomingValue})
		case !diff.BaseEmpty && diff.IncomingEmpty && enrichmentColumn(name):
			diff.Class = "enrichment"
			diff.Resolution = "kept_base"
		default:
			diff.Class = "genuine_content"
			diff.Resolution = "unresolved"
			blocking = raiseBlock(blocking, diff.Class)
		}
		diffs = append(diffs, diff)
	}
	if blocking == "" {
		return diffs, fills, nil, nil
	}
	return diffs, nil, &RowConflict{Table: "messages", DeviceID: device, ChatJID: chat, ID: id, Class: blocking}, nil
}

func raiseBlock(current, next string) string {
	if current == "genuine_content" || next == "genuine_content" {
		return "genuine_content"
	}
	if current == "" {
		return next
	}
	return current
}

func contentsLinked(base, incoming string, edits [][2]string) bool {
	if base == incoming {
		return false
	}
	for _, pair := range edits {
		if pair[0] == pair[1] {
			continue
		}
		if (pair[0] == base && pair[1] == incoming) || (pair[0] == incoming && pair[1] == base) {
			return true
		}
	}
	return false
}

func valuesEqual(left, right any) (bool, error) {
	a, err := canonValue(left)
	if err != nil {
		return false, err
	}
	b, err := canonValue(right)
	if err != nil {
		return false, err
	}
	return a == b, nil
}

func isEmptyValue(value any) bool {
	if value == nil {
		return true
	}
	switch typed := value.(type) {
	case string:
		return typed == ""
	case []byte:
		return len(typed) == 0
	case bool:
		return false
	case int:
		return typed == 0
	case int64:
		return typed == 0
	case float64:
		return typed == 0
	default:
		canon, err := canonValue(value)
		return err == nil && canon == ""
	}
}

func errMissing(name string) error {
	return &columnError{name: name}
}

type columnError struct{ name string }

func (e *columnError) Error() string { return "column " + e.name + " is missing" }

func indexEdits(rows []sqlRow) (map[string][][2]string, error) {
	out := map[string][][2]string{}
	for _, row := range rows {
		device, err := rowString(row, "device_id")
		if err != nil {
			return nil, err
		}
		chat, err := rowString(row, "chat_jid")
		if err != nil {
			return nil, err
		}
		id, err := rowString(row, "original_message_id")
		if err != nil {
			return nil, err
		}
		previous, err := rowString(row, "previous_content")
		if err != nil {
			return nil, err
		}
		next, err := rowString(row, "new_content")
		if err != nil {
			return nil, err
		}
		key := device + "\x00" + chat + "\x00" + id
		out[key] = append(out[key], [2]string{previous, next})
	}
	return out, nil
}
