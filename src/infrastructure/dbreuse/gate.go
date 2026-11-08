package dbreuse

import (
	"context"
	"fmt"
	"strings"
)

const (
	standingKeysURI = "DB_KEYS_URI is unknown. It was absent from LaunchAgent env/args only; other runtime configuration was not verified, so this is not an absence."
	standingHistory = "history_complete is false. Snapshot, preflight, reconcile, overlay, and coverage do not prove a complete WhatsApp history."
	standingSession = "whatsapp.db was not opened. The session compat version and any separate keys database are unknown. A newer whatsmeow schema than official v9.9.0 makes that binary refuse the session."
	standingArchive = "History JSON archives are not imported. The reported eight files were not parsed by this command. Overlap, once audited, still does not set history_complete."
)

// GateRequest is the cutover decision for one already-copied chatstorage file.
type GateRequest struct {
	DB             string
	ExpectRevision string
	Writers        []string
	Target         string
	WorkDir        string
}

// GateReport says whether the mechanical checks passed. RealCutoverBlocked
// stays true: this helper does not clear keys, archives, or completeness.
type GateReport struct {
	Path                  string    `json:"path"`
	SchemaVersion         int       `json:"schema_version"`
	SchemaAccepted        bool      `json:"schema_accepted"`
	RevisionVerified      bool      `json:"revision_verified"`
	WritersQuiesced       bool      `json:"writers_quiesced"`
	TargetCompared        bool      `json:"target_compared"`
	TargetOnly            int       `json:"target_only"`
	Changed               int       `json:"changed"`
	MechanicalGatesPassed bool      `json:"mechanical_gates_passed"`
	RealCutoverBlocked    bool      `json:"real_cutover_blocked"`
	HistoryComplete       bool      `json:"history_complete"`
	HistoryCompleteReason string    `json:"history_complete_reason"`
	Blockers              []string  `json:"blockers"`
	StandingLimits        []string  `json:"standing_limits"`
	Revision              *Revision `json:"revision,omitempty"`
	Notes                 []string  `json:"notes"`
}

// Gate checks the snapshot schema, the attested revision, writer freeze, and
// an optional target comparison. It does not modify either database.
func Gate(ctx context.Context, req GateRequest) (*GateReport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	report := &GateReport{
		Path:                  req.DB,
		HistoryComplete:       false,
		HistoryCompleteReason: historyCompleteNote,
		Blockers:              []string{},
		StandingLimits: []string{
			standingKeysURI,
			standingHistory,
			standingSession,
			standingArchive,
		},
		Notes: []string{
			"Operator-reported file sizes live on the revision record and are not measured from this path.",
			"No -shm does not prove the periodic merge writer is stopped. It can open the database and exit between checks.",
			"Freeze source GOWA, the source periodic merge writer, and target GOWA before the snapshot that will be installed. A SQLite backup is transactionally consistent and can still fall in the middle of a multi-transaction merge.",
		},
		RealCutoverBlocked: true,
	}
	writers, err := normalizeWriters(req.Writers)
	if err != nil {
		report.Blockers = append(report.Blockers, err.Error())
		return report, err
	}
	checked, checkErr := Check(req.DB)
	if checked != nil {
		report.SchemaVersion = checked.SchemaVersion
	}
	if checkErr != nil {
		report.Blockers = append(report.Blockers, checkErr.Error())
		report.RealCutoverBlocked = true
		return report, checkErr
	} else if checked == nil || !checked.Reusable {
		reason := "schema was not accepted"
		if checked != nil && len(checked.Reasons) > 0 {
			reason = strings.Join(checked.Reasons, "; ")
		}
		report.Blockers = append(report.Blockers, "unknown or mismatched schema; real cutover is blocked: "+reason)
	} else if checked.SchemaVersion != schemaOfficialV990 {
		report.Blockers = append(report.Blockers, fmt.Sprintf("schema %d is not the verified %s migration result (%d). Official v9.9.0 can upgrade an older known schema, but this file does not show that the installed binary finished InitializeSchema. Real cutover is blocked", checked.SchemaVersion, installedRevisionSHA, schemaOfficialV990))
	} else {
		report.SchemaAccepted = true
		report.Notes = append(report.Notes, "Schema 54 matches the migration list in the verified revision and in v9.8.0. The file does not prove which binary last opened it.")
	}

	rev, revErr := LookupRevision(req.ExpectRevision)
	if revErr != nil {
		report.Blockers = append(report.Blockers, revErr.Error())
	} else {
		report.RevisionVerified = true
		report.Revision = &rev
	}

	_, wal, err := siblingFile(req.DB, "-wal")
	if err != nil {
		return report, err
	}
	_, shm, err := siblingFile(req.DB, "-shm")
	if err != nil {
		return report, err
	}
	if shm {
		report.Blockers = append(report.Blockers, "a -shm file is present; a writer is connected, so a freeze attestation is not accepted")
	}
	if wal {
		report.Blockers = append(report.Blockers, "a -wal file is present; gate the single-file snapshot taken after the writers were frozen")
	}
	if missing := missingWriters(writers); len(missing) > 0 {
		report.Blockers = append(report.Blockers, "writers not attested quiesced: "+strings.Join(missing, ", "))
	}
	report.WritersQuiesced = writersQuiesced(writers, shm) && !wal

	if req.Target == "" {
		report.Blockers = append(report.Blockers, "target chatstorage was not compared; target-only messages would be dropped by adopting the source file")
	} else {
		if req.WorkDir == "" {
			return report, fmt.Errorf("gate requires --work-dir when --target is set")
		}
		compared, err := Reconcile(ctx, req.DB, req.Target, req.WorkDir)
		if err != nil {
			report.Blockers = append(report.Blockers, err.Error())
			return report, err
		}
		report.TargetCompared = true
		report.TargetOnly = len(compared.TargetOnly)
		report.Changed = len(compared.Changed)
		if report.TargetOnly > 0 {
			report.Blockers = append(report.Blockers, fmt.Sprintf("target-only messages: %d. Reconcile does not merge them. Overlay can stage a new file; this gate does not apply one", report.TargetOnly))
		}
		if report.Changed > 0 {
			report.Blockers = append(report.Blockers, fmt.Sprintf("message payload conflicts: %d. Neither version is dropped", report.Changed))
		}
	}

	report.MechanicalGatesPassed = report.SchemaAccepted && report.RevisionVerified && report.WritersQuiesced && report.TargetCompared && report.TargetOnly == 0 && report.Changed == 0 && len(report.Blockers) == 0
	report.RealCutoverBlocked = true
	report.HistoryComplete = false
	return report, nil
}
