package dbreuse

import (
	"database/sql"
	"fmt"
	"strings"
)

// LegacySchemaReport describes tables, columns, and indexes. It does not
// contain row values. OldBridgeAssessed stays false: seeing a schema is not
// history coverage and does not authorize an import or a purge.
type LegacySchemaReport struct {
	Path                string        `json:"path"`
	SHA256              string        `json:"sha256"`
	UserVersion         int           `json:"user_version"`
	ApplicationID       int           `json:"application_id"`
	PageSize            int           `json:"page_size"`
	PageCount           int           `json:"page_count"`
	GOWAChatStorage     bool          `json:"gowa_chatstorage"`
	Tables              []LegacyTable `json:"tables"`
	SessionRefused      bool          `json:"session_refused"`
	OldBridgeAssessed   bool          `json:"old_bridge_assessed"`
	HistoryComplete     bool          `json:"history_complete"`
	ImportFormatDefined bool          `json:"import_format_defined"`
	Notes               []string      `json:"notes"`
}

// LegacyTable is one SQLite table's declared shape and its row count.
type LegacyTable struct {
	Name        string         `json:"name"`
	RowCount    int            `json:"row_count"`
	Columns     []LegacyColumn `json:"columns"`
	Indexes     []LegacyIndex  `json:"indexes"`
	ForeignKeys []LegacyFK     `json:"foreign_keys"`
}

// LegacyColumn is a declared column. Default expressions are omitted.
type LegacyColumn struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	NotNull bool   `json:"not_null"`
	Primary bool   `json:"primary"`
}

// LegacyIndex is a non-partial description of an index. The WHERE text of a
// partial index is not copied.
type LegacyIndex struct {
	Name    string   `json:"name"`
	Unique  bool     `json:"unique"`
	Origin  string   `json:"origin"`
	Partial bool     `json:"partial"`
	Columns []string `json:"columns"`
}

// LegacyFK is a declared foreign key. It does not include row samples.
type LegacyFK struct {
	Table string `json:"table"`
	From  string `json:"from"`
	To    string `json:"to"`
}

// InspectLegacySchema reads the SQLite catalog of a history database.
// A session or key database is refused before any table shape is returned.
func InspectLegacySchema(path string) (*LegacySchemaReport, error) {
	report := &LegacySchemaReport{
		Path:                path,
		Tables:              []LegacyTable{},
		OldBridgeAssessed:   false,
		HistoryComplete:     false,
		ImportFormatDefined: false,
		Notes: []string{
			"This report is table, column, and index metadata plus row counts. It does not select row values.",
			"A session or key database is refused. messages.db can be copied with snapshot; that copy is not an import format.",
			"old_bridge_assessed stays false. Schema inspection does not prove which history is present and does not authorize a purge.",
		},
	}
	if path == "" {
		return report, fmt.Errorf("legacy-schema requires --db")
	}
	sum, err := hashFile(path)
	if err == nil {
		report.SHA256 = sum
	}
	db, err := openSQLite(path, true)
	if err != nil {
		if strings.Contains(err.Error(), "session or key") {
			report.SessionRefused = true
		}
		return report, err
	}
	defer db.Close()
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&report.UserVersion); err != nil {
		return report, err
	}
	if err := db.QueryRow(`PRAGMA application_id`).Scan(&report.ApplicationID); err != nil {
		return report, err
	}
	if err := db.QueryRow(`PRAGMA page_size`).Scan(&report.PageSize); err != nil {
		return report, err
	}
	if err := db.QueryRow(`PRAGMA page_count`).Scan(&report.PageCount); err != nil {
		return report, err
	}
	report.GOWAChatStorage, err = isGOWAChatStorage(db)
	if err != nil {
		return report, err
	}
	names, err := tableNames(db)
	if err != nil {
		return report, err
	}
	for _, name := range names {
		table, err := describeTable(db, name)
		if err != nil {
			return report, err
		}
		report.Tables = append(report.Tables, table)
	}
	return report, nil
}

func legacySQLite(path string) (bool, error) {
	db, err := openSQLite(path, true)
	if err != nil {
		return false, err
	}
	defer db.Close()
	chat, err := isGOWAChatStorage(db)
	if err != nil {
		return false, err
	}
	return !chat, nil
}

func isGOWAChatStorage(db *sql.DB) (bool, error) {
	for _, name := range []string{"schema_info", "devices", "messages", "chats"} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&n); err != nil {
			return false, err
		}
		if n == 0 {
			return false, nil
		}
	}
	return true, nil
}

func tableNames(db *sql.DB) ([]string, error) {
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
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
	return names, rows.Err()
}

func describeTable(db *sql.DB, name string) (LegacyTable, error) {
	quoted, err := quoteIdent(name)
	if err != nil {
		return LegacyTable{}, err
	}
	table := LegacyTable{Name: name, Columns: []LegacyColumn{}, Indexes: []LegacyIndex{}, ForeignKeys: []LegacyFK{}}
	if err := db.QueryRow(fmt.Sprintf(`SELECT COUNT(*) FROM %s`, quoted)).Scan(&table.RowCount); err != nil {
		return LegacyTable{}, err
	}
	columns, err := db.Query(fmt.Sprintf(`PRAGMA table_info(%s)`, quoted))
	if err != nil {
		return LegacyTable{}, err
	}
	for columns.Next() {
		var cid, notNull, pk int
		var colName, colType string
		var dflt any
		if err := columns.Scan(&cid, &colName, &colType, &notNull, &dflt, &pk); err != nil {
			columns.Close()
			return LegacyTable{}, err
		}
		table.Columns = append(table.Columns, LegacyColumn{Name: colName, Type: colType, NotNull: notNull == 1, Primary: pk > 0})
	}
	if err := columns.Err(); err != nil {
		columns.Close()
		return LegacyTable{}, err
	}
	columns.Close()
	indexes, err := db.Query(fmt.Sprintf(`PRAGMA index_list(%s)`, quoted))
	if err != nil {
		return LegacyTable{}, err
	}
	var indexNames []LegacyIndex
	for indexes.Next() {
		var seq, unique, partial int
		var indexName, origin string
		if err := indexes.Scan(&seq, &indexName, &unique, &origin, &partial); err != nil {
			indexes.Close()
			return LegacyTable{}, err
		}
		indexNames = append(indexNames, LegacyIndex{Name: indexName, Unique: unique == 1, Origin: origin, Partial: partial == 1, Columns: []string{}})
	}
	if err := indexes.Err(); err != nil {
		indexes.Close()
		return LegacyTable{}, err
	}
	indexes.Close()
	for i := range indexNames {
		quotedIndex, err := quoteIdent(indexNames[i].Name)
		if err != nil {
			return LegacyTable{}, err
		}
		info, err := db.Query(fmt.Sprintf(`PRAGMA index_info(%s)`, quotedIndex))
		if err != nil {
			return LegacyTable{}, err
		}
		for info.Next() {
			var seqno, cid int
			var col string
			if err := info.Scan(&seqno, &cid, &col); err != nil {
				info.Close()
				return LegacyTable{}, err
			}
			indexNames[i].Columns = append(indexNames[i].Columns, col)
		}
		if err := info.Err(); err != nil {
			info.Close()
			return LegacyTable{}, err
		}
		info.Close()
	}
	table.Indexes = indexNames
	fks, err := db.Query(fmt.Sprintf(`PRAGMA foreign_key_list(%s)`, quoted))
	if err != nil {
		return LegacyTable{}, err
	}
	defer fks.Close()
	for fks.Next() {
		var id, seq int
		var ref, from, to, onUpdate, onDelete, match string
		if err := fks.Scan(&id, &seq, &ref, &from, &to, &onUpdate, &onDelete, &match); err != nil {
			return LegacyTable{}, err
		}
		table.ForeignKeys = append(table.ForeignKeys, LegacyFK{Table: ref, From: from, To: to})
	}
	return table, fks.Err()
}
