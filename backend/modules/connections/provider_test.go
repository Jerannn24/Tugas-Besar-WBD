package connections

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

type fakeConnector struct{}

func (fakeConnector) Connect(context.Context) (driver.Conn, error) { return fakeConn{}, nil }
func (fakeConnector) Driver() driver.Driver                        { return nil }

type fakeConn struct{}

func (fakeConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("unsupported") }
func (fakeConn) Close() error                        { return nil }
func (fakeConn) Begin() (driver.Tx, error)           { return nil, errors.New("unsupported") }

func fakeManager(bound bool) *PoolManager {
	m := &PoolManager{entries: map[uuid.UUID]*entry{}, drainTimeout: 100 * time.Millisecond}
	m.load = func(context.Context, uuid.UUID) (targetConfig, *uuid.UUID, error) {
		cfg := targetConfig{Executor: credential{"x_executor", "p"}, Preparer: credential{"x_preparer", "p"}}
		if !bound {
			return cfg, nil, nil
		}
		t := uuid.New()
		return cfg, &t, nil
	}
	m.openRoles = func(targetConfig) (rolePools, error) {
		return rolePools{executor: sql.OpenDB(fakeConnector{}), preparer: sql.OpenDB(fakeConnector{})}, nil
	}
	return m
}

func TestAcquireUnboundNotReady(t *testing.T) {
	m := fakeManager(false)
	if _, err := m.Acquire(context.Background(), uuid.New()); !errors.Is(err, ErrNotReady) {
		t.Fatalf("err = %v", err)
	}
}

func TestLeaseCounterIdempotentReleaseAndQuiesce(t *testing.T) {
	m, id := fakeManager(true), uuid.New()
	l, err := m.Acquire(context.Background(), id)
	if err != nil || l.Executor == nil || l.Preparer == nil {
		t.Fatalf("acquire: %v", err)
	}
	if m.ActiveLeases(id) != 1 {
		t.Fatal("want 1 active lease")
	}
	if _, err := m.Quiesce(id); !errors.Is(err, ErrBusy) {
		t.Fatalf("quiesce with lease = %v, want ErrBusy", err)
	}
	l.Release()
	l.Release()
	if m.ActiveLeases(id) != 0 {
		t.Fatal("release must be idempotent and reach 0")
	}
	resume, err := m.Quiesce(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Acquire(context.Background(), id); !errors.Is(err, ErrBusy) {
		t.Fatalf("acquire while quiesced = %v", err)
	}
	resume(true)
	if l2, err := m.Acquire(context.Background(), id); err != nil {
		t.Fatal(err)
	} else {
		l2.Release()
	}
}

func TestInvalidateDefersCloseUntilDrained(t *testing.T) {
	m, id := fakeManager(true), uuid.New()
	l, err := m.Acquire(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	prep := l.Preparer
	m.Invalidate(id)
	if err := prep.PingContext(context.Background()); err != nil && errors.Is(err, sql.ErrConnDone) {
		t.Fatal("pool closed while lease active")
	}
	if _, err := prep.Conn(context.Background()); err != nil {
		t.Fatalf("preparer unusable while leased: %v", err)
	}
	l.Release()
	if _, err := prep.Conn(context.Background()); err == nil {
		t.Fatal("old generation should be closed after drain")
	}
}

func TestCloseRefusesAdmission(t *testing.T) {
	m, id := fakeManager(true), uuid.New()
	l, _ := m.Acquire(context.Background(), id)
	m.Close() // drain deadline expires with the lease still held
	if _, err := m.Acquire(context.Background(), id); !errors.Is(err, ErrClosed) {
		t.Fatalf("err = %v", err)
	}
	l.Release()
}
