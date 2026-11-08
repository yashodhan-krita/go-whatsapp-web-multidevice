package dbreuse

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var identPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type sqlRow struct {
	cols []string
	vals []any
}

func loadRows(db *sql.DB, table string, cols []string) ([]sqlRow, error) {
	quoted, err := quoteIdents(cols)
	if err != nil {
		return nil, err
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

func insertRow(tx *sql.Tx, table string, row sqlRow) error {
	quoted, err := quoteIdents(row.cols)
	if err != nil {
		return err
	}
	tableName, err := quoteIdent(table)
	if err != nil {
		return err
	}
	holders := make([]string, len(row.cols))
	for i := range holders {
		holders[i] = "?"
	}
	_, err = tx.Exec(
		fmt.Sprintf(`INSERT INTO %s (%s) VALUES (%s)`, tableName, strings.Join(quoted, ", "), strings.Join(holders, ", ")),
		row.vals...,
	)
	return err
}

func quoteIdents(names []string) ([]string, error) {
	out := make([]string, len(names))
	for i, name := range names {
		quoted, err := quoteIdent(name)
		if err != nil {
			return nil, err
		}
		out[i] = quoted
	}
	return out, nil
}

func quoteIdent(name string) (string, error) {
	if !identPattern.MatchString(name) {
		return "", fmt.Errorf("refusing SQL identifier %q", name)
	}
	return `"` + name + `"`, nil
}

func cloneValue(v any) any {
	b, ok := v.([]byte)
	if !ok {
		return v
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}

func rowField(row sqlRow, name string) (any, bool) {
	for i, col := range row.cols {
		if col == name {
			return row.vals[i], true
		}
	}
	return nil, false
}

func rowString(row sqlRow, name string) (string, error) {
	val, ok := rowField(row, name)
	if !ok {
		return "", fmt.Errorf("column %s is missing", name)
	}
	if val == nil {
		return "", nil
	}
	switch t := val.(type) {
	case string:
		return t, nil
	case []byte:
		return string(t), nil
	case time.Time:
		return t.UTC().Format(time.RFC3339Nano), nil
	default:
		return "", fmt.Errorf("column %s has type %T", name, val)
	}
}

func withString(row sqlRow, name, value string) (sqlRow, error) {
	out := sqlRow{cols: row.cols, vals: append([]any(nil), row.vals...)}
	found := false
	for i, col := range out.cols {
		if col == name {
			out.vals[i] = value
			found = true
		}
	}
	if !found {
		return sqlRow{}, fmt.Errorf("column %s is missing", name)
	}
	return out, nil
}

func rowHash(row sqlRow, skip string) (string, error) {
	order := make([]int, 0, len(row.cols))
	for i, col := range row.cols {
		if col == skip {
			continue
		}
		order = append(order, i)
	}
	sort.Slice(order, func(i, j int) bool { return row.cols[order[i]] < row.cols[order[j]] })
	h := sha256.New()
	for _, i := range order {
		text, err := canonValue(row.vals[i])
		if err != nil {
			return "", err
		}
		h.Write([]byte(row.cols[i]))
		h.Write([]byte{0})
		h.Write([]byte(text))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func canonValue(v any) (string, error) {
	switch t := v.(type) {
	case nil:
		return "<nil>", nil
	case string:
		return "s:" + t, nil
	case []byte:
		return "b:" + hex.EncodeToString(t), nil
	case int64:
		return "i:" + strconv.FormatInt(t, 10), nil
	case int32:
		return "i:" + strconv.FormatInt(int64(t), 10), nil
	case int:
		return "i:" + strconv.Itoa(t), nil
	case float64:
		return "f:" + strconv.FormatFloat(t, 'g', -1, 64), nil
	case bool:
		if t {
			return "t:1", nil
		}
		return "t:0", nil
	case time.Time:
		return "d:" + t.UTC().Format(time.RFC3339Nano), nil
	default:
		return "", fmt.Errorf("refusing SQL value type %T", v)
	}
}

func rowKey(row sqlRow, cols []string) (string, error) {
	parts := make([]string, len(cols))
	for i, col := range cols {
		val, err := rowString(row, col)
		if err != nil {
			return "", err
		}
		parts[i] = val
	}
	return strings.Join(parts, "\x00"), nil
}

func fingerprintTables(q queryer, tables []string) (string, error) {
	h := sha256.New()
	for _, table := range tables {
		var name string
		err := q.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name)
		if err == sql.ErrNoRows {
			return "", fmt.Errorf("preserved table %s is missing", table)
		}
		if err != nil {
			return "", err
		}
		cols, err := columnNamesQuery(q, table)
		if err != nil {
			return "", err
		}
		rows, err := loadRowsQuery(q, table, cols)
		if err != nil {
			return "", err
		}
		h.Write([]byte(table))
		h.Write([]byte{0})
		for _, row := range rows {
			sum, err := rowHash(row, "")
			if err != nil {
				return "", err
			}
			h.Write([]byte(sum))
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

type queryer interface {
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

func columnNamesQuery(q queryer, table string) ([]string, error) {
	rows, err := q.Query(`SELECT name FROM pragma_table_info(?) ORDER BY cid`, table)
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

func loadRowsQuery(q queryer, table string, cols []string) ([]sqlRow, error) {
	quoted, err := quoteIdents(cols)
	if err != nil {
		return nil, err
	}
	tableName, err := quoteIdent(table)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(fmt.Sprintf(`SELECT %s FROM %s ORDER BY rowid`, strings.Join(quoted, ", "), tableName))
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
