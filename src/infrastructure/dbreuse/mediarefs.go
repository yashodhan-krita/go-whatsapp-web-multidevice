package dbreuse

import (
	"database/sql"
	"fmt"
	"strings"
)

// OperatorMediaInventory is the completed walk reported on 8 November 2026.
// This package did not measure it and does not copy or delete those files.
var OperatorMediaInventory = MediaInventory{
	MeasuredByTool:      false,
	Files:               490698,
	Directories:         33266,
	SymlinksFollowed:    1,
	AliasDedupedByInode: true,
	MissingOrUnreadable: 0,
	LogicalGB:           "129.21",
	AllocatedGB:         "130.48",
	BoxFreeGB:           "49.21",
	MacFreeGB:           "63.86",
	RawShortfallGB:      "80",
	ReserveShortfallGB:  "92",
	Reserve:             "allocated size plus 10 GiB, against box free space",
	Copied:              false,
	Deleted:             false,
}

// MediaInventory is an operator-supplied size report, not a tool measurement.
type MediaInventory struct {
	MeasuredByTool      bool   `json:"measured_by_tool"`
	Files               int    `json:"files"`
	Directories         int    `json:"directories"`
	SymlinksFollowed    int    `json:"symlinks_followed"`
	AliasDedupedByInode bool   `json:"alias_deduped_by_inode"`
	MissingOrUnreadable int    `json:"missing_or_unreadable"`
	LogicalGB           string `json:"logical_gb"`
	AllocatedGB         string `json:"allocated_gb"`
	BoxFreeGB           string `json:"box_free_gb"`
	MacFreeGB           string `json:"mac_free_gb"`
	RawShortfallGB      string `json:"raw_shortfall_gb"`
	ReserveShortfallGB  string `json:"reserve_shortfall_gb"`
	Reserve             string `json:"reserve"`
	Copied              bool   `json:"copied"`
	Deleted             bool   `json:"deleted"`
}

// MediaFieldEvidence is one fact the next source adapter must show before a
// byte subset can be selected. Display names are not that evidence.
type MediaFieldEvidence struct {
	Field  string `json:"field"`
	Needed string `json:"needed"`
}

// MediaRefRequest asks for a manifest of media fields stored on messages.
// MediaRoot, when set, is refused. This command does not walk a media tree.
type MediaRefRequest struct {
	DB        string
	Archives  []string
	MediaRoot string
}

// MediaRefReport says whether chatstorage and history JSON identify local bytes.
// SubsetProven, WholeTreeCopyAllowed, PurgeAllowed, and HistoryComplete stay false.
type MediaRefReport struct {
	Database                   string               `json:"database"`
	MediaRows                  int                  `json:"media_rows"`
	RowsWithFilename           int                  `json:"rows_with_filename"`
	RowsWithURL                int                  `json:"rows_with_url"`
	RowsWithDirectPath         int                  `json:"rows_with_direct_path"`
	RowsWithFileSHA256         int                  `json:"rows_with_file_sha256"`
	RowsWithFileEncSHA256      int                  `json:"rows_with_file_enc_sha256"`
	RowsWithMediaKey           int                  `json:"rows_with_media_key"`
	AmbiguousDisplayNameGroups int                  `json:"ambiguous_display_name_groups"`
	PathShapedFilenames        int                  `json:"path_shaped_filenames"`
	FilenameIsDisplayName      bool                 `json:"filename_is_display_name"`
	LocalPathStored            bool                 `json:"local_path_stored"`
	ArchiveFiles               int                  `json:"archive_files"`
	ArchiveMessageIDs          int                  `json:"archive_message_ids"`
	ArchiveLocalPaths          int                  `json:"archive_local_paths"`
	MetadataOnlyArchives       int                  `json:"metadata_only_archives"`
	BytesSelected              *int64               `json:"bytes_selected"`
	SubsetProven               bool                 `json:"subset_proven"`
	WholeTreeCopyAllowed       bool                 `json:"whole_tree_copy_allowed"`
	WholeTreeFits              bool                 `json:"whole_tree_fits"`
	PurgeAllowed               bool                 `json:"purge_allowed"`
	OldBridgeAssessed          bool                 `json:"old_bridge_assessed"`
	HistoryComplete            bool                 `json:"history_complete"`
	Inventory                  MediaInventory       `json:"inventory"`
	Unresolved                 []string             `json:"unresolved"`
	NextAdapterFields          []MediaFieldEvidence `json:"next_adapter_fields"`
	Notes                      []string             `json:"notes"`
}

// MediaReferences reads message media columns and optional history JSON.
// It does not open a media directory, hash file bytes, or choose a copy set.
func MediaReferences(req MediaRefRequest) (*MediaRefReport, error) {
	report := &MediaRefReport{
		Database:              req.DB,
		FilenameIsDisplayName: true,
		LocalPathStored:       false,
		ArchiveLocalPaths:     0,
		SubsetProven:          false,
		WholeTreeCopyAllowed:  false,
		WholeTreeFits:         false,
		PurgeAllowed:          false,
		OldBridgeAssessed:     false,
		HistoryComplete:       false,
		Inventory:             OperatorMediaInventory,
		Unresolved:            []string{},
		NextAdapterFields:     requiredMediaEvidence(),
		Notes: []string{
			"messages.filename is a display name from the media type and caption, or a document's original name. Downloads are written under statics/media/<unix>-<uuid><ext> or statics/media/<phone>/<date>/ and that path is not stored on the message.",
			"file_sha256, file_enc_sha256, url, direct_path, and media_key do not name a local file. This command does not read those values into the report.",
			"The operator inventory is 129.21 GB logical and 130.48 GB allocated, with 49.21 GB free on the box. The whole tree is about 80 GB short, and about 92 GB short with allocated size plus a 10 GiB reserve. It was not copied or deleted.",
			"Absence of an old-bridge importer is unverified coverage. It is not permission to discard files this manifest cannot name.",
			historyCompleteNote,
		},
	}
	if strings.TrimSpace(req.MediaRoot) != "" {
		return report, fmt.Errorf("refusing to match display filenames against a media tree")
	}
	if req.DB == "" {
		return report, fmt.Errorf("media-refs requires --db")
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
		return report, fmt.Errorf("media-refs refused the snapshot: %s", reason)
	}
	db, err := openSQLite(req.DB, true)
	if err != nil {
		return report, err
	}
	defer db.Close()
	if err := countMediaReferences(db, report); err != nil {
		return report, err
	}
	for _, path := range req.Archives {
		parsed, err := readHistoryArchive(path)
		if err != nil {
			return report, err
		}
		report.ArchiveFiles++
		if !parsed.MessageAudit {
			report.MetadataOnlyArchives++
			continue
		}
		report.ArchiveMessageIDs += len(parsed.Messages)
	}
	report.Unresolved = mediaUnresolved(report)
	report.SubsetProven = false
	report.WholeTreeCopyAllowed = false
	report.PurgeAllowed = false
	report.HistoryComplete = false
	report.BytesSelected = nil
	return report, nil
}

func countMediaReferences(db *sql.DB, report *MediaRefReport) error {
	cols, err := columnNamesQuery(db, "messages")
	if err != nil {
		return err
	}
	have := map[string]struct{}{}
	for _, col := range cols {
		have[col] = struct{}{}
	}
	for _, col := range []string{"id", "chat_jid", "device_id", "media_type", "filename", "url", "direct_path", "file_length", "file_sha256", "file_enc_sha256", "media_key"} {
		if _, ok := have[col]; !ok {
			return fmt.Errorf("messages.%s is missing; refusing to guess a media reference", col)
		}
	}
	row := db.QueryRow(`
		SELECT
			SUM(CASE WHEN COALESCE(media_type, '') != '' OR COALESCE(filename, '') != '' OR COALESCE(url, '') != '' OR COALESCE(direct_path, '') != '' OR COALESCE(file_length, 0) != 0 OR file_sha256 IS NOT NULL OR file_enc_sha256 IS NOT NULL OR media_key IS NOT NULL THEN 1 ELSE 0 END),
			SUM(CASE WHEN COALESCE(filename, '') != '' THEN 1 ELSE 0 END),
			SUM(CASE WHEN COALESCE(url, '') != '' THEN 1 ELSE 0 END),
			SUM(CASE WHEN COALESCE(direct_path, '') != '' THEN 1 ELSE 0 END),
			SUM(CASE WHEN file_sha256 IS NOT NULL AND length(file_sha256) > 0 THEN 1 ELSE 0 END),
			SUM(CASE WHEN file_enc_sha256 IS NOT NULL AND length(file_enc_sha256) > 0 THEN 1 ELSE 0 END),
			SUM(CASE WHEN media_key IS NOT NULL AND length(media_key) > 0 THEN 1 ELSE 0 END),
			SUM(CASE WHEN filename LIKE '%/%' OR filename LIKE '%\\%' THEN 1 ELSE 0 END)
		FROM messages`)
	var media, names, urls, direct, plain, enc, key, shaped sql.NullInt64
	if err := row.Scan(&media, &names, &urls, &direct, &plain, &enc, &key, &shaped); err != nil {
		return err
	}
	report.MediaRows = nullInt(media)
	report.RowsWithFilename = nullInt(names)
	report.RowsWithURL = nullInt(urls)
	report.RowsWithDirectPath = nullInt(direct)
	report.RowsWithFileSHA256 = nullInt(plain)
	report.RowsWithFileEncSHA256 = nullInt(enc)
	report.RowsWithMediaKey = nullInt(key)
	report.PathShapedFilenames = nullInt(shaped)
	var groups int
	if err := db.QueryRow(`
		SELECT COUNT(*) FROM (
			SELECT filename FROM messages
			WHERE COALESCE(filename, '') != ''
			GROUP BY filename
			HAVING COUNT(*) > 1
		)`).Scan(&groups); err != nil {
		return err
	}
	report.AmbiguousDisplayNameGroups = groups
	return nil
}

func nullInt(value sql.NullInt64) int {
	if !value.Valid {
		return 0
	}
	return int(value.Int64)
}

func mediaUnresolved(report *MediaRefReport) []string {
	reasons := []string{
		"no message column stores the local path written at download time",
		"a display filename is not a unique byte identity, including when every name is distinct",
	}
	if report.AmbiguousDisplayNameGroups > 0 {
		reasons = append(reasons, fmt.Sprintf("%d display filenames are used by more than one message", report.AmbiguousDisplayNameGroups))
	}
	if report.PathShapedFilenames > 0 {
		reasons = append(reasons, fmt.Sprintf("%d filenames contain a path separator and are still not treated as local paths", report.PathShapedFilenames))
	}
	if report.ArchiveFiles > 0 {
		reasons = append(reasons, "history JSON message ids do not name local files")
	}
	reasons = append(reasons,
		"the old-bridge database was not opened; its files cannot be classified as referenced or unreferenced",
		"the whole media tree does not fit on the box and this command does not choose a smaller copy set",
	)
	return reasons
}

func requiredMediaEvidence() []MediaFieldEvidence {
	return []MediaFieldEvidence{
		{Field: "account_storage_jid", Needed: "registry storage JID for the account that owned the file; not the slot UUID"},
		{Field: "chat_jid", Needed: "chat JID stored with the message"},
		{Field: "message_id", Needed: "message id stored with the chat JID"},
		{Field: "relative_path", Needed: "path relative to one named root, not messages.filename"},
		{Field: "byte_length", Needed: "size of that file in bytes"},
		{Field: "sha256", Needed: "hash of that file's bytes"},
		{Field: "inode_and_device", Needed: "so a symlink alias is not counted as a second copy"},
		{Field: "root_name", Needed: "which tree the relative path is under, including the single followed symlink"},
		{Field: "display_name_is_path", Needed: "must be false; a display name is not accepted as the path"},
	}
}
