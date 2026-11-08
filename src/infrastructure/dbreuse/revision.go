package dbreuse

import (
	"fmt"
	"strings"
)

// Installed Mac binary, verified from the public upstream commit.
// It is not this fork's base commit.
const (
	installedRevisionSHA = "07c20ff1c682398cda9b6b5c4fc8be99b3b30fa8"
	installedParentSHA   = "be8e3a252f3d091ec86effb23adc0e0c99c9a561"
	forkBaseSHA          = "4c4c4ed66d5130f1ef15ea2f92e331d1be8a8fbd"
)

// Revision is one source commit whose tree was inspected.
// DatabaseProvesRevision is false: chatstorage does not store the git SHA.
type Revision struct {
	SHA                      string           `json:"sha"`
	Parent                   string           `json:"parent"`
	ParentTag                string           `json:"parent_tag"`
	Subject                  string           `json:"subject"`
	CommittedAt              string           `json:"committed_at"`
	AppVersion               string           `json:"app_version"`
	Whatsmeow                string           `json:"whatsmeow"`
	SchemaVersion            int              `json:"schema_version"`
	GoFilesChangedFromParent bool             `json:"go_files_changed_from_parent"`
	FilesChanged             []string         `json:"files_changed"`
	PublicMainIdenticalOn    string           `json:"public_main_identical_on"`
	ForkBaseSHA              string           `json:"fork_base_sha"`
	ForkBaseIsInstalled      bool             `json:"fork_base_is_installed"`
	DatabaseProvesRevision   bool             `json:"database_proves_revision"`
	KeysURI                  string           `json:"keys_uri"`
	HistoryComplete          bool             `json:"history_complete"`
	Observed                 ObservedMetadata `json:"observed"`
	Notes                    []string         `json:"notes"`
}

// ObservedMetadata is operator-reported and was not measured from a database
// in this workspace. No session or history file was copied here.
type ObservedMetadata struct {
	MeasuredByTool         bool   `json:"measured_by_tool"`
	ObservedAt             string `json:"observed_at"`
	ChatstorageBytes       int64  `json:"chatstorage_bytes"`
	ChatstorageWALBytes    int64  `json:"chatstorage_wal_bytes"`
	ChatstorageWALLatest   string `json:"chatstorage_wal_latest"`
	SessionBytes           int64  `json:"session_bytes"`
	SessionWALBytes        int64  `json:"session_wal_bytes"`
	HistoryJSONFiles       int    `json:"history_json_files"`
	HistoryJSONBytes       int64  `json:"history_json_bytes"`
	HistoryJSONParsed      bool   `json:"history_json_parsed"`
	TargetChatstorageBytes int64  `json:"target_chatstorage_bytes"`
	TargetWAL              string `json:"target_wal"`
	Note                   string `json:"note"`
}

// InstalledRevision returns the only source commit this helper will treat as
// the installed Mac binary.
func InstalledRevision() Revision {
	return Revision{
		SHA:                      installedRevisionSHA,
		Parent:                   installedParentSHA,
		ParentTag:                "v9.9.0",
		Subject:                  "chore: update skill release note",
		CommittedAt:              "2026-11-06T01:39:20Z",
		AppVersion:               "v9.9.0",
		Whatsmeow:                "v0.0.0-20261105212532-307a2edf117d",
		SchemaVersion:            schemaOfficialV990,
		GoFilesChangedFromParent: false,
		FilesChanged:             []string{".agents/skills/update-release-note/SKILL.md"},
		PublicMainIdenticalOn:    "2026-11-08",
		ForkBaseSHA:              forkBaseSHA,
		ForkBaseIsInstalled:      false,
		DatabaseProvesRevision:   false,
		KeysURI:                  "unknown",
		HistoryComplete:          false,
		Observed: ObservedMetadata{
			MeasuredByTool:         false,
			ObservedAt:             "02:25 IST",
			ChatstorageBytes:       57929728,
			ChatstorageWALBytes:    4260112,
			ChatstorageWALLatest:   "02:04 IST",
			SessionBytes:           5746688,
			SessionWALBytes:        4124152,
			HistoryJSONFiles:       8,
			HistoryJSONBytes:       38606270,
			HistoryJSONParsed:      false,
			TargetChatstorageBytes: 23818240,
			TargetWAL:              "active, size not recorded",
			Note:                   "Sizes were reported by the operator. This process did not read those files. DB_KEYS_URI was absent from LaunchAgent env/args only; other runtime configuration was not verified.",
		},
		Notes: []string{
			"Public upstream 07c20ff1c682398cda9b6b5c4fc8be99b3b30fa8 has parent be8e3a252f3d091ec86effb23adc0e0c99c9a561, which is tag v9.9.0. The only file changed is the release-note skill. AppVersion is v9.9.0 and getMigrations ends at 54, the same statements as that tag.",
			"On 2026-11-08 the public main commit compared equal to 07c20ff. A later main commit would not be this binary.",
			"This fork's base 4c4c4ed66d5130f1ef15ea2f92e331d1be8a8fbd (28 Oct 2026) is older. Its AppVersion is v9.7.0 and its migration list ends at 50. Do not use that tree as the Mac schema.",
			"Schema 54 is also the v9.8.0 fingerprint. The database does not store AppVersion or vcs.revision, so preflight cannot prove which of those binaries last opened the file.",
			"whatsmeow in this tree is v0.0.0-20261105212532-307a2edf117d, the same module version as tag v9.9.0. The session file was not opened, so its on-disk compat version is unknown. A newer session than that upgrade table makes official v9.9.0 refuse it.",
			"DB_KEYS_URI is unknown, not absent. LaunchAgent env/args did not set it.",
			historyCompleteNote,
		},
	}
}

// LookupRevision accepts only the full installed SHA.
// Any other value, including this fork's base and a short prefix, blocks cutover.
func LookupRevision(sha string) (Revision, error) {
	sha = strings.TrimSpace(strings.ToLower(sha))
	if len(sha) != 40 || !hexSHA(sha) {
		return Revision{}, fmt.Errorf("revision must be the full 40-character SHA; short ids are ambiguous and real cutover is blocked")
	}
	if sha == forkBaseSHA {
		return Revision{}, fmt.Errorf("revision %s is this fork's base (28 Oct 2026, migration list ends at 50), not the installed Mac binary %s", sha, installedRevisionSHA)
	}
	if sha != installedRevisionSHA {
		return Revision{}, fmt.Errorf("revision %s is not the verified installed source %s; real cutover is blocked", sha, installedRevisionSHA)
	}
	rev := InstalledRevision()
	rev.HistoryComplete = false
	rev.DatabaseProvesRevision = false
	rev.ForkBaseIsInstalled = false
	return rev, nil
}

func hexSHA(sha string) bool {
	for _, r := range sha {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		default:
			return false
		}
	}
	return true
}
