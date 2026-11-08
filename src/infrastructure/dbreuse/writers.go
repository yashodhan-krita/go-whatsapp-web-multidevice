package dbreuse

import (
	"fmt"
	"sort"
	"strings"
)

// requiredQuiescedWriters are the processes that can commit history while a
// snapshot is taken. Absence of -shm does not prove the periodic writer is
// stopped: it can open the database and exit between checks.
var requiredQuiescedWriters = []string{
	"source-gowa",
	"source-periodic-merge",
	"target-gowa",
}

// RequiredQuiescedWriters returns the writer names a cutover attestation must
// list exactly.
func RequiredQuiescedWriters() []string {
	out := make([]string, len(requiredQuiescedWriters))
	copy(out, requiredQuiescedWriters)
	return out
}

func normalizeWriters(in []string) ([]string, error) {
	if len(in) == 0 {
		return []string{}, nil
	}
	seen := map[string]struct{}{}
	var out []string
	allowed := map[string]struct{}{}
	for _, name := range requiredQuiescedWriters {
		allowed[name] = struct{}{}
	}
	for _, raw := range in {
		for _, part := range strings.Split(raw, ",") {
			name := strings.TrimSpace(part)
			if name == "" {
				continue
			}
			if _, ok := allowed[name]; !ok {
				return nil, fmt.Errorf("unknown writer %q; attestation must name %s", name, strings.Join(requiredQuiescedWriters, ", "))
			}
			if _, ok := seen[name]; ok {
				return nil, fmt.Errorf("duplicate writer attestation %s", name)
			}
			seen[name] = struct{}{}
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out, nil
}

func missingWriters(attested []string) []string {
	have := map[string]struct{}{}
	for _, name := range attested {
		have[name] = struct{}{}
	}
	var missing []string
	for _, name := range requiredQuiescedWriters {
		if _, ok := have[name]; !ok {
			missing = append(missing, name)
		}
	}
	return missing
}

func writersQuiesced(attested []string, shm bool) bool {
	return !shm && len(missingWriters(attested)) == 0
}
