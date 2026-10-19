package workflow

import (
	"os"
	"strings"
	"testing"

	"go.temporal.io/sdk/worker"
)

// Current UNKNOWN/cancel history is replayed by
// TestTemporalUnknownWaitCancelAcrossWorkersAndReplay with a real server.
// Two main-version error histories are covered below. This source guard
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

// Captured from the exact pre-marker main revision, not synthesized by the new
// Workflow. Replay must consume old commands without scheduling recovery.
func TestMainUnknownHistoriesReplay(t *testing.T) {
	for _, name := range []string{"deploy", "rollback"} {
		t.Run(name, func(t *testing.T) {
			replayer := worker.NewWorkflowReplayer()
			replayer.RegisterWorkflow(ReleaseWorkflow)
			if err := replayer.ReplayWorkflowHistoryFromJSONFile(nil, "testdata/main-8eb8a40-"+name+"-unknown.json"); err != nil {
				t.Fatal(err)
			}
		})
	}
}
