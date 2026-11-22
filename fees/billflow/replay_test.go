package billflow_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/worker"

	"github.com/lmeow01/pave-fees/fees/billflow"
)

// Bills stay open for a whole billing period, so a deployed change to the
// workflow replays histories that the previous code wrote. Every history in
// testdata, recorded with scripts/record-workflow-histories.sh, must still
// replay against the current code.
func TestRecordedHistoriesReplay(t *testing.T) {
	histories, err := filepath.Glob("testdata/*.json")
	require.NoError(t, err)
	require.NotEmpty(t, histories)

	for _, history := range histories {
		t.Run(filepath.Base(history), func(t *testing.T) {
			replayer := worker.NewWorkflowReplayer()
			replayer.RegisterWorkflow(billflow.Bill)
			require.NoError(t, replayer.ReplayWorkflowHistoryFromJSONFile(nil, history))
		})
	}
}
