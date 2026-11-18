
package executioncontract

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var ErrNotConfigured = errors.New("adapter tidak dikonfigurasi")
var ErrRequestConflict = errors.New("REQUEST_CONFLICT")

type FakeJobs struct {
	mu            sync.Mutex
	requests      map[string]EnqueueRequest
	jobs          map[string]JobView
	Enabled       bool
	EnqueueError  error
	FinalizeError error
	Attempts      []Attempt
}

func (f *FakeJobs) EnqueueTx(
	_ context.Context,
	_ *sql.Tx,
	in EnqueueRequest,
) (EnqueueResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if !f.Enabled {
		return EnqueueResult{}, ErrNotConfigured
	}

	if f.EnqueueError != nil {
		return EnqueueResult{}, f.EnqueueError
	}

	if f.requests == nil {
		f.requests = map[string]EnqueueRequest{}
		f.jobs = map[string]JobView{}
	}

	key := in.DatabaseID + ":" + in.RequestID

	if old, ok := f.requests[key]; ok {
		if old.ActorID != in.ActorID ||
			old.TargetID != in.TargetID ||
			old.MigrationID != in.MigrationID ||
			old.ProposalID != in.ProposalID ||
			old.Revision != in.Revision ||
			old.Action != in.Action ||
			old.Direction != in.Direction ||
			old.SnapshotHash != in.SnapshotHash ||
			old.ApprovalFingerprint != in.ApprovalFingerprint ||
			!sameID(old.SourceExecutionID, in.SourceExecutionID) {
			return EnqueueResult{}, ErrRequestConflict
		}

		return EnqueueResult{
			JobAccepted: f.jobs[key].JobAccepted,
			Replayed:    true,
		}, nil
	}

	n := len(f.jobs) + 1

	jobID := fmt.Sprintf(
		"50000000-0000-4000-8000-%012d",
		n,
	)

	executionID := fmt.Sprintf(
		"60000000-0000-4000-8000-%012d",
		n,
	)

	if in.Action == Resume {
		if in.SourceExecutionID == nil ||
			*in.SourceExecutionID == "" {
			return EnqueueResult{}, ErrRequestConflict
		}

		executionID = *in.SourceExecutionID
	}

	accepted := JobAccepted{
		JobID:       jobID,
		ExecutionID: executionID,
		ProposalID:  in.ProposalID,
		DatabaseID:  in.DatabaseID,
		Status:      Queued,
		StatusURL: "/api/databases/" +
			in.DatabaseID + "/jobs/" + jobID,
	}

	if in.SourceExecutionID != nil {
		source := *in.SourceExecutionID
		in.SourceExecutionID = &source
	}

	f.requests[key] = in

	f.jobs[key] = JobView{
		JobAccepted:       accepted,
		MigrationID:       in.MigrationID,
		Revision:          in.Revision,
		Action:             in.Action,
		Mode:               in.Snapshot.Mode,
		ExecutionStatus:   Queued,
		AttemptNo:         1,
		SourceExecutionID: in.SourceExecutionID,
		CreatedAt:         time.Now().UTC(),
		AllowedActions:    []AllowedAction{},
	}

	return EnqueueResult{
		JobAccepted: accepted,
	}, nil
}

func sameID(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}

	return *a == *b
}

func (f *FakeJobs) GetJob(
	_ context.Context,
	db string,
	id string,
) (JobView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if !f.Enabled {
		return JobView{}, ErrNotConfigured
	}

	for _, j := range f.jobs {
		if j.DatabaseID == db && j.JobID == id {
			return j, nil
		}
	}

	return JobView{}, ErrNotConfigured
}

func (f *FakeJobs) ListJobs(
	_ context.Context,
	db string,
	p Page,
) (JobPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if !f.Enabled {
		return JobPage{}, ErrNotConfigured
	}

	if p.Limit == 0 {
		p.Limit = 20
	}

	if p.Limit < 1 || p.Limit > 100 {
		return JobPage{}, ErrNotConfigured
	}

	all := []JobView{}

	for _, j := range f.jobs {
		if j.DatabaseID == db {
			all = append(all, j)
		}
	}

	sort.Slice(all, func(i, j int) bool {
		return all[i].JobID < all[j].JobID
	})

	start := 0

	if p.Cursor != nil {
		var err error

		parts := strings.SplitN(*p.Cursor, ":", 2)

		if len(parts) != 2 || parts[0] != db {
			return JobPage{}, ErrNotConfigured
		}

		start, err = strconv.Atoi(parts[1])

		if err != nil || start < 0 || start > len(all) {
			return JobPage{}, ErrNotConfigured
		}
	}

	end := start + p.Limit

	var next *string

	if end < len(all) {
		v := db + ":" + strconv.Itoa(end)
		next = &v
	} else {
		end = len(all)
	}

	return JobPage{
		Items:      all[start:end],
		NextCursor: next,
	}, nil
}

func (f *FakeJobs) ListAttempts(
	_ context.Context,
	db string,
	p Page,
) (AttemptPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if !f.Enabled {
		return AttemptPage{}, ErrNotConfigured
	}

	if p.Limit == 0 {
		p.Limit = 20
	}

	if p.Limit < 1 || p.Limit > 100 {
		return AttemptPage{}, ErrNotConfigured
	}

	all := []Attempt{}

	for _, v := range f.Attempts {
		if v.DatabaseID == db {
			all = append(all, v)
		}
	}

	sort.Slice(all, func(i, j int) bool {
		return all[i].AttemptID < all[j].AttemptID
	})

	start := 0

	if p.Cursor != nil {
		parts := strings.SplitN(*p.Cursor, ":", 2)

		if len(parts) != 2 || parts[0] != db {
			return AttemptPage{}, ErrNotConfigured
		}

		var err error

		start, err = strconv.Atoi(parts[1])

		if err != nil || start < 0 || start > len(all) {
			return AttemptPage{}, ErrNotConfigured
		}
	}

	end := start + p.Limit

	var next *string

	if end < len(all) {
		v := db + ":" + strconv.Itoa(end)
		next = &v
	} else {
		end = len(all)
	}

	return AttemptPage{
		Items:      all[start:end],
		NextCursor: next,
	}, nil
}

func (f *FakeJobs) FinalizeJobTx(
	_ context.Context,
	_ *sql.Tx,
	in JobUpdate,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if !f.Enabled {
		return ErrNotConfigured
	}

	if f.FinalizeError != nil {
		return f.FinalizeError
	}

	for k, j := range f.jobs {
		if j.DatabaseID == in.Job.DatabaseID &&
			j.JobID == in.Job.JobID &&
			j.ExecutionID == in.Job.ExecutionID {
			f.jobs[k] = in.Job
			return nil
		}
	}

	return ErrNotConfigured
}

type FakeGate struct {
	RevalidateFunc func(
		context.Context,
		RevalidationRequest,
	) (ExecutionPlan, error)

	AllowedActionsFunc func(
		context.Context,
		string,
		string,
		string,
	) ([]AllowedAction, error)

	RecordCommitFunc func(
		context.Context,
		*sql.Tx,
		CommitEvidence,
	) error
}

func (f FakeGate) Revalidate(
	c context.Context,
	in RevalidationRequest,
) (ExecutionPlan, error) {
	if f.RevalidateFunc == nil {
		return ExecutionPlan{}, ErrNotConfigured
	}

	return f.RevalidateFunc(c, in)
}

func (f FakeGate) AllowedActions(
	c context.Context,
	a string,
	d string,
	e string,
) ([]AllowedAction, error) {
	if f.AllowedActionsFunc == nil {
		return nil, ErrNotConfigured
	}

	return f.AllowedActionsFunc(c, a, d, e)
}

func (f FakeGate) RecordCommitTx(
	c context.Context,
	tx *sql.Tx,
	in CommitEvidence,
) error {
	if f.RecordCommitFunc == nil {
		return ErrNotConfigured
	}

	return f.RecordCommitFunc(c, tx, in)
}

// Compile-time interface checks.
var _ Jobs = (*FakeJobs)(nil)
var _ WorkflowGate = FakeGate{}
