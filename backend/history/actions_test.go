package history

import (
	"strings"
	"testing"
)

func TestAttemptActionsOfferCompensateWithSourceRule(t *testing.T) {
	for _, want := range []string{
		"'compensate',p.execution_id,j.mode)\n   AND length(trim(COALESCE(j.snapshot->>'sql_down',''))) > 0\n   THEN 'propose_compensate'",
		"gr.access='edit'",
		"blocker.blocks_database",
	} {
		if !strings.Contains(attemptPageQuery, want) {
			t.Fatalf("attempt actions query lost %q", want)
		}
	}
}
