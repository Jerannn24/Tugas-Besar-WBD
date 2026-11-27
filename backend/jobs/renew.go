package jobs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/executioncontract"
	"github.com/google/uuid"
)

type RenewLeaseRequest struct {
	JobID         string
	LeaseToken    string
	LeaseDuration time.Duration
	// AfterLock is an optional test seam called while holding the row lock before commit.
	AfterLock func(ctx context.Context) error
}

type RenewResult struct {
	Renewed        bool      `json:"renewed"`
	LeaseExpiresAt time.Time `json:"lease_expires_at,omitempty"`
}

type LeaseRenewal = RenewResult

// RenewLease extends the lease of a running job if the token matches and the current lease is unexpired.
// Stale token, expired lease, missing or non-running job do not mutate state and return a distinct no-renewal result (Renewed: false, nil).
// Expiry is verified using PostgreSQL clock_timestamp() while owning the row lock.
// The lease token and attempt count are not mutated.
func (s *Service) RenewLease(ctx context.Context, in RenewLeaseRequest) (*RenewResult, error) {
	if s.db == nil {
		return nil, errors.New("db is required")
	}
	if in.LeaseDuration == 0 {
		in.LeaseDuration = 30 * time.Second
	} else if in.LeaseDuration < time.Millisecond {
		return nil, executioncontract.ContractError{
			Code:    "INVALID_REQUEST",
			Message: "lease_duration must be at least 1ms",
		}
	}

	jobUUID, err := uuid.Parse(in.JobID)
	if err != nil {
		return &RenewResult{Renewed: false}, nil
	}
	tokenUUID, err := uuid.Parse(in.LeaseToken)
	if err != nil {
		return &RenewResult{Renewed: false}, nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin renew tx: %w", err)
	}
	defer tx.Rollback()

	// shortcut: only renews existing unexpired lease without takeover or retry scheduling, upgrade in takeover/retry increment
	var (
		status    string
		token     sql.NullString
		unexpired sql.NullBool
	)
	err = tx.QueryRowContext(ctx, `
		SELECT status, lease_token::text, (lease_expires_at > clock_timestamp()) AS unexpired
		FROM app.jobs
		WHERE id = $1
		FOR UPDATE
	`, jobUUID.String()).Scan(&status, &token, &unexpired)
	if errors.Is(err, sql.ErrNoRows) {
		return &RenewResult{Renewed: false}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lock job for renewal: %w", err)
	}

	if in.AfterLock != nil {
		if err := in.AfterLock(ctx); err != nil {
			return nil, err
		}
	}

	if status != "running" || !token.Valid || token.String != tokenUUID.String() || !unexpired.Valid || !unexpired.Bool {
		return &RenewResult{Renewed: false}, nil
	}

	var newExpiresAt time.Time
	err = tx.QueryRowContext(ctx, `
		UPDATE app.jobs
		SET lease_expires_at = GREATEST(lease_expires_at, clock_timestamp() + ($1 * interval '1 millisecond')),
		    updated_at = clock_timestamp()
		WHERE id = $2
		  AND lease_token = $3
		  AND status = 'running'
		  AND lease_expires_at > clock_timestamp()
		RETURNING lease_expires_at
	`, in.LeaseDuration.Milliseconds(), jobUUID.String(), tokenUUID.String()).Scan(&newExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return &RenewResult{Renewed: false}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("update renewed lease: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit renew tx: %w", err)
	}

	return &RenewResult{
		Renewed:        true,
		LeaseExpiresAt: newExpiresAt,
	}, nil
}
