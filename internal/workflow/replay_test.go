package workflow

import (
	"os"
	"strings"
	"testing"
)

// Current UNKNOWN/cancel history is replayed by
// TestTemporalUnknownWaitCancelAcrossWorkersAndReplay with a real server.
// Old-history upgrade replay remains a separate requirement. This source guard
// only catches common direct nondeterministic I/O; it is not a replay test.
func TestWorkflowDefinitionContainsNoDirectNondeterministicIO(t *testing.T) {
	data, err := os.ReadFile("release.go")
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	for _, forbidden := range []string{"time.Now(", "rand.", "net/http", "pgx", "database/sql"} {
		if strings.Contains(source, forbidden) {
			t.Fatalf("workflow source contains forbidden operation %q", forbidden)
		}
	}
}
