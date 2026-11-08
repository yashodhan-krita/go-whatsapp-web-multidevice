package dbreuse

import (
	"strconv"
	"time"
)

// ColumnClassCount is an aggregate. It names a column and a difference class.
// It does not include the column value.
type ColumnClassCount struct {
	Column string `json:"column"`
	Class  string `json:"class"`
	Rows   int    `json:"rows"`
}

func valueRelation(column string, left, right any) (string, error) {
	equal, err := valuesEqual(left, right)
	if err != nil {
		return "", err
	}
	if equal {
		return "equal", nil
	}
	if emptyValue(left) && emptyValue(right) {
		return "empty_normalization", nil
	}
	if sameBit(left, right) {
		return "type_normalization", nil
	}
	if sameBytes(left, right) {
		return "type_normalization", nil
	}
	if sameInteger(left, right) {
		return "type_normalization", nil
	}
	if column == "timestamp" || column == "created_at" || column == "updated_at" {
		leftTime, leftOK := asTime(left)
		rightTime, rightOK := asTime(right)
		if leftOK && rightOK {
			if leftTime.Equal(rightTime) {
				return "timestamp_normalization", nil
			}
			if leftTime.Unix() == rightTime.Unix() {
				return "timestamp_precision", nil
			}
		}
	}
	if nilVersusZero(left, right) {
		return "numeric_default", nil
	}
	return "different", nil
}

func emptyValue(value any) bool {
	if value == nil {
		return true
	}
	switch typed := value.(type) {
	case string:
		return typed == ""
	case []byte:
		return len(typed) == 0
	default:
		return false
	}
}

func sameBit(left, right any) bool {
	l, lok := bitValue(left)
	r, rok := bitValue(right)
	return lok && rok && l == r
}

func bitValue(value any) (int, bool) {
	switch typed := value.(type) {
	case bool:
		if typed {
			return 1, true
		}
		return 0, true
	case int:
		if typed == 0 || typed == 1 {
			return typed, true
		}
	case int64:
		if typed == 0 || typed == 1 {
			return int(typed), true
		}
	case float64:
		if typed == 0 || typed == 1 {
			return int(typed), true
		}
	case string:
		switch typed {
		case "0", "false":
			return 0, true
		case "1", "true":
			return 1, true
		}
	}
	return 0, false
}

func sameBytes(left, right any) bool {
	l, lok := byteValue(left)
	r, rok := byteValue(right)
	if !lok || !rok || len(l) != len(r) {
		return false
	}
	for i := range l {
		if l[i] != r[i] {
			return false
		}
	}
	return true
}

func byteValue(value any) ([]byte, bool) {
	switch typed := value.(type) {
	case []byte:
		return typed, true
	case string:
		return []byte(typed), true
	default:
		return nil, false
	}
}

func sameInteger(left, right any) bool {
	l, lok := integerValue(left)
	r, rok := integerValue(right)
	return lok && rok && l == r
}

func integerValue(value any) (int64, bool) {
	switch typed := value.(type) {
	case int:
		return int64(typed), true
	case int64:
		return typed, true
	case float64:
		if typed == float64(int64(typed)) {
			return int64(typed), true
		}
	case string:
		n, err := strconv.ParseInt(typed, 10, 64)
		if err == nil {
			return n, true
		}
	}
	return 0, false
}

func nilVersusZero(left, right any) bool {
	if left == nil {
		n, ok := integerValue(right)
		return ok && n == 0
	}
	if right == nil {
		n, ok := integerValue(left)
		return ok && n == 0
	}
	return false
}

func asTime(value any) (time.Time, bool) {
	switch typed := value.(type) {
	case time.Time:
		return typed.UTC(), true
	case string:
		return parseTimeText(typed)
	default:
		return time.Time{}, false
	}
}

func parseTimeText(value string) (time.Time, bool) {
	zoned := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05Z07:00",
		"2006-01-02 15:04:05.999999999Z07:00",
		"2006-01-02T15:04:05Z07:00",
	}
	for _, layout := range zoned {
		parsed, err := time.Parse(layout, value)
		if err == nil {
			return parsed.UTC(), true
		}
	}
	unzoned := []string{
		"2006-01-02 15:04:05",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02T15:04:05",
		"2006-01-02T15:04:05.999999999",
	}
	for _, layout := range unzoned {
		parsed, err := time.ParseInLocation(layout, value, time.UTC)
		if err == nil {
			return parsed.UTC(), true
		}
	}
	return time.Time{}, false
}

// legacyStoredTimestamp converts a legacy TIMESTAMP to the UTC text GOWA stores.
// A value with no zone is kept as that clock and reported zone_unspecified.
// An unparseable value returns ok false. No zone is invented.
func legacyStoredTimestamp(value any) (stored, class string, ok bool) {
	switch typed := value.(type) {
	case time.Time:
		if typed.IsZero() {
			return "", "", false
		}
		return typed.UTC().Format(sqliteTimestamp), "zoned", true
	case int64:
		return time.Unix(typed, 0).UTC().Format(sqliteTimestamp), "unix_seconds", true
	case int:
		return time.Unix(int64(typed), 0).UTC().Format(sqliteTimestamp), "unix_seconds", true
	case float64:
		if typed != float64(int64(typed)) {
			return "", "", false
		}
		return time.Unix(int64(typed), 0).UTC().Format(sqliteTimestamp), "unix_seconds", true
	case string:
		if typed == "" {
			return "", "", false
		}
		if parsed, zoned := parseZonedTime(typed); zoned {
			return parsed.UTC().Format(sqliteTimestamp), "zoned", true
		}
		if parsed, ok := parseTimeText(typed); ok {
			return parsed.UTC().Format(sqliteTimestamp), "zone_unspecified", true
		}
	}
	return "", "", false
}

func parseZonedTime(value string) (time.Time, bool) {
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05Z07:00",
		"2006-01-02 15:04:05.999999999Z07:00",
	}
	for _, layout := range layouts {
		parsed, err := time.Parse(layout, value)
		if err == nil {
			return parsed, true
		}
	}
	return time.Time{}, false
}

const sqliteTimestamp = "2006-01-02 15:04:05"

type stagedColumnDiff struct {
	column string
	class  string
	level  string
}

func classifyStagedRow(source, staged sqlRow, archiveFilled bool) ([]stagedColumnDiff, string, error) {
	names := map[string]struct{}{}
	for _, name := range source.cols {
		names[name] = struct{}{}
	}
	for _, name := range staged.cols {
		names[name] = struct{}{}
	}
	var diffs []stagedColumnDiff
	worst := "identical"
	for name := range names {
		if name == "device_id" {
			continue
		}
		left, leftOK := rowField(source, name)
		right, rightOK := rowField(staged, name)
		if !leftOK || !rightOK {
			diffs = append(diffs, stagedColumnDiff{column: name, class: "column_set", level: "unexplained"})
			worst = worseLevel(worst, "unexplained")
			continue
		}
		relation, err := valueRelation(name, left, right)
		if err != nil {
			return nil, "", err
		}
		if relation == "equal" {
			continue
		}
		class, level := stagedClass(name, relation, left, right, archiveFilled)
		diffs = append(diffs, stagedColumnDiff{column: name, class: class, level: level})
		worst = worseLevel(worst, level)
	}
	return diffs, worst, nil
}

func stagedClass(name, relation string, source, staged any, archiveFilled bool) (string, string) {
	switch relation {
	case "type_normalization", "timestamp_normalization", "empty_normalization":
		return relation, "normalized"
	case "numeric_default", "timestamp_precision":
		return relation, "policy"
	}
	switch name {
	case "created_at", "updated_at":
		return "provenance", "policy"
	case "timestamp":
		return "timestamp_kept_base", "policy"
	case "content":
		if emptyValue(source) && !emptyValue(staged) && archiveFilled {
			return "archive_text_fill", "policy"
		}
		return "genuine", "unexplained"
	case "sender", "is_from_me", "media_key", "file_sha256", "file_enc_sha256":
		return "genuine", "unexplained"
	default:
		if enrichmentColumn(name) && emptyValue(source) && !emptyValue(staged) {
			return "enrichment_kept_base", "policy"
		}
		return "dropped_source", "unexplained"
	}
}

func worseLevel(current, next string) string {
	rank := map[string]int{"identical": 0, "normalized": 1, "policy": 2, "unexplained": 3}
	if rank[next] > rank[current] {
		return next
	}
	return current
}
