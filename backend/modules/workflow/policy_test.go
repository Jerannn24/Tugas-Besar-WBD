package workflow

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestEffectiveAccessPrefersEdit(t *testing.T) {
	if got := EffectiveAccess(AccessView, AccessEdit); got != AccessEdit {
		t.Fatalf("access = %q, want edit", got)
	}
}

func TestViewOnlyLeaderMayApprove(t *testing.T) {
	leader := uuid.New()
	if !MayApprove(leader, ApprovalRequirement{LeaderUserID: &leader}) {
		t.Fatal("view-only leader should be allowed to approve")
	}
}

func TestApprovalRequiresTheGroupLeader(t *testing.T) {
	leader, other := uuid.New(), uuid.New()
	if MayApprove(other, ApprovalRequirement{LeaderUserID: &leader}) {
		t.Fatal("non-leader should not approve")
	}
}

func TestOneLeaderCanApproveTwoGroups(t *testing.T) {
	leader := uuid.New()
	requirements := []ApprovalRequirement{
		{GroupID: uuid.New(), LeaderUserID: &leader},
		{GroupID: uuid.New(), LeaderUserID: &leader},
	}
	for _, requirement := range requirements {
		if !MayApprove(leader, requirement) {
			t.Fatal("leader should approve each group separately")
		}
	}
}

func TestApprovalSummaryComplete(t *testing.T) {
	if !(ApprovalSummary{Required: 2, Approved: 2}).Complete() {
		t.Fatal("all required approvals should complete the summary")
	}
	if (ApprovalSummary{Required: 2, Approved: 1}).Complete() {
		t.Fatal("partial approvals must remain incomplete")
	}
}

func TestCreateRollbackRequiresSource(t *testing.T) {
	s := &Service{}
	_, err := s.Create(context.Background(), uuid.New(), CreateInput{
		DatabaseID: uuid.NewString(), MigrationID: uuid.NewString(), Action: "rollback",
	})
	if !errors.Is(err, ErrInvalidProposal) {
		t.Fatalf("rollback without source: got %v, want ErrInvalidProposal", err)
	}
}
