package migration

import (
	"context"
	"strings"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/history"
)

func (h *Handler) Reconcile(ctx context.Context, databaseID, executionID string) (history.Execution, error) {
	ctx, cancel := context.WithTimeout(ctx, h.d.Timeout)
	defer cancel()
	if !validUUID(databaseID) || !validUUID(executionID) {
		return history.Execution{}, fault(400, "INVALID_REQUEST", "Invalid reconciliation request")
	}
	databaseID, executionID = strings.ToLower(databaseID), strings.ToLower(executionID)
	a, err := h.store.load(ctx, databaseID, executionID)
	if err != nil {
		return history.Execution{}, err
	}
	if terminal(a.Status) {
		return a.Execution, nil
	}
	session, err := h.openTarget(ctx, databaseID, a.TargetID)
	if err != nil {
		return a.Execution, err
	}
	defer session.close(h.d.Timeout)
	a, err = h.store.load(ctx, databaseID, executionID)
	if err != nil {
		return history.Execution{}, err
	}
	if terminal(a.Status) {
		return a.Execution, nil
	}
	r, found, receiptErr := session.readReceipt(ctx, a.ID)
	result := recoveryOutcome(r, found, receiptErr)
	finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(ctx), h.d.Timeout)
	defer finishCancel()
	updated, err := h.finish(finishCtx, a, result)
	if err != nil {
		if updated.ID != "" {
			return updated.Execution, err
		}
		return a.Execution, err
	}
	return updated.Execution, nil
}

func recoveryOutcome(r receipt, found bool, err error) outcome {
	if err != nil {
		return unknown("RECEIPT_UNCONFIRMED", "Execution receipt could not be confirmed")
	}
	if !found {
		return unknown("OUTCOME_UNCONFIRMED", "Execution outcome is not confirmed")
	}
	return outcome{status: "succeeded", receipt: &r}
}
