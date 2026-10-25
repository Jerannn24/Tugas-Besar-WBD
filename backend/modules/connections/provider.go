package connections

import (
	"context"
	"database/sql"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/stdlib"
)

// Lease is a pinned executor connection plus the preparer pool of one bound
// physical target. Release is idempotent and also closes Executor.
type Lease struct {
	TargetID uuid.UUID
	Executor *sql.Conn
	Preparer *sql.DB
	Release  func()
}

type rolePools struct{ executor, preparer *sql.DB }

func (p rolePools) close() {
	if p.executor != nil {
		_ = p.executor.Close()
	}
	if p.preparer != nil {
		_ = p.preparer.Close()
	}
}

// generation is one immutable set of role pools for a connection. A retired
// generation is closed only after its last lease is released.
type generation struct {
	number  uint64
	target  uuid.UUID
	pools   rolePools
	leases  int
	retired bool
}

type entry struct {
	next     uint64
	current  *generation
	retired  map[*generation]struct{}
	active   int // reservations + leases across all generations
	quiesced bool
	viewer   *sql.DB
}

// PoolManager owns the viewer pool (Open) and the role-aware lease provider
// (Acquire) for every connection.
type PoolManager struct {
	repository   *Repository
	mu           sync.Mutex
	entries      map[uuid.UUID]*entry
	closed       bool
	drainTimeout time.Duration

	// seams, replaced in unit tests
	load       func(context.Context, uuid.UUID) (targetConfig, *uuid.UUID, error)
	openViewer func(targetConfig) (*sql.DB, error)
	openRoles  func(targetConfig) (rolePools, error)
}

func NewPoolManager(repository *Repository) *PoolManager {
	return &PoolManager{
		repository:   repository,
		entries:      make(map[uuid.UUID]*entry),
		drainTimeout: 10 * time.Second,
		load:         repository.loadConfig,
		openViewer:   openViewer,
		openRoles:    openRolePools,
	}
}

func openRolePools(cfg targetConfig) (rolePools, error) {
	ec, err := connConfig(cfg, cfg.Executor)
	if err != nil {
		return rolePools{}, err
	}
	pc, err := connConfig(cfg, cfg.Preparer)
	if err != nil {
		return rolePools{}, err
	}
	executor := stdlib.OpenDB(*ec)
	// Executor sessions carry advisory locks and transaction state: never
	// keep idle ones around for reuse.
	executor.SetMaxOpenConns(4)
	executor.SetMaxIdleConns(0)
	preparer := stdlib.OpenDB(*pc)
	preparer.SetMaxOpenConns(4)
	preparer.SetMaxIdleConns(2)
	preparer.SetConnMaxIdleTime(time.Minute)
	return rolePools{executor: executor, preparer: preparer}, nil
}

func (m *PoolManager) entryLocked(id uuid.UUID) *entry {
	e := m.entries[id]
	if e == nil {
		e = &entry{retired: make(map[*generation]struct{})}
		m.entries[id] = e
	}
	return e
}

func (m *PoolManager) gcLocked(id uuid.UUID, e *entry) {
	if e.current == nil && e.viewer == nil && len(e.retired) == 0 && e.active == 0 && !e.quiesced {
		delete(m.entries, id)
	}
}

// Open returns the read-only VIEWER pool using viewer credentials only.
func (m *PoolManager) Open(ctx context.Context, id uuid.UUID) (*sql.DB, error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, ErrClosed
	}
	if e := m.entries[id]; e != nil && e.viewer != nil {
		db := e.viewer
		m.mu.Unlock()
		return db, nil
	}
	m.mu.Unlock()
	cfg, _, err := m.load(ctx, id)
	if err != nil {
		return nil, err
	}
	db, err := m.openViewer(cfg)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		_ = db.Close()
		return nil, ErrClosed
	}
	e := m.entryLocked(id)
	if e.viewer != nil {
		_ = db.Close()
		return e.viewer, nil
	}
	e.viewer = db
	return db, nil
}

// Acquire pins an executor connection and hands out the preparer pool of the
// physical target bound to id. Unbound connections yield ErrNotReady.
func (m *PoolManager) Acquire(ctx context.Context, id uuid.UUID) (*Lease, error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, ErrClosed
	}
	e := m.entryLocked(id)
	if e.quiesced {
		m.gcLocked(id, e)
		m.mu.Unlock()
		return nil, ErrBusy
	}
	e.active++ // reservation: blocks Quiesce until released
	have := e.current != nil
	m.mu.Unlock()

	fail := func(err error) (*Lease, error) {
		m.mu.Lock()
		e.active--
		m.gcLocked(id, e)
		m.mu.Unlock()
		return nil, err
	}

	var fresh *generation
	if !have {
		cfg, target, err := m.load(ctx, id)
		if err != nil {
			return fail(err)
		}
		if target == nil || !cfg.hasRoles() {
			return fail(ErrNotReady)
		}
		pools, err := m.openRoles(cfg)
		if err != nil {
			return fail(ErrTargetUnavailable)
		}
		fresh = &generation{target: *target, pools: pools}
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		if fresh != nil {
			fresh.pools.close()
		}
		return fail(ErrClosed)
	}
	if e.current == nil {
		if fresh == nil {
			// invalidated while reserving; caller can retry
			m.mu.Unlock()
			return fail(ErrBusy)
		}
		e.next++
		fresh.number = e.next
		e.current, fresh = fresh, nil
	}
	g := e.current
	g.leases++
	m.mu.Unlock()
	if fresh != nil {
		fresh.pools.close()
	}

	undo := func() {
		m.mu.Lock()
		m.releaseLocked(id, e, g)
		m.mu.Unlock()
		m.closeRetired(g)
	}
	conn, err := g.pools.executor.Conn(ctx)
	if err != nil {
		undo()
		return nil, ErrTargetUnavailable
	}
	var once sync.Once
	return &Lease{
		TargetID: g.target,
		Executor: conn,
		Preparer: g.pools.preparer,
		Release: func() {
			once.Do(func() {
				_ = conn.Close()
				undo()
			})
		},
	}, nil
}

// releaseLocked drops one lease of g.
func (m *PoolManager) releaseLocked(id uuid.UUID, e *entry, g *generation) {
	g.leases--
	e.active--
	if g.retired && g.leases == 0 {
		delete(e.retired, g)
	}
	m.gcLocked(id, e)
}

// closeRetired closes g's pools when it is retired and fully drained.
func (m *PoolManager) closeRetired(g *generation) {
	m.mu.Lock()
	drained := g.retired && g.leases == 0
	m.mu.Unlock()
	if drained {
		g.pools.close()
	}
}

// Invalidate retires the current role generation and closes the viewer pool.
// Generations with active leases keep working until drained.
func (m *PoolManager) Invalidate(id uuid.UUID) {
	m.mu.Lock()
	e := m.entries[id]
	if e == nil {
		m.mu.Unlock()
		return
	}
	viewer := e.viewer
	e.viewer = nil
	var closeNow *generation
	if g := e.current; g != nil {
		e.current = nil
		g.retired = true
		if g.leases == 0 {
			closeNow = g
		} else {
			e.retired[g] = struct{}{}
		}
	}
	m.gcLocked(id, e)
	m.mu.Unlock()
	if viewer != nil {
		_ = viewer.Close()
	}
	if closeNow != nil {
		closeNow.pools.close()
	}
}

// Quiesce reserves exclusive maintenance of id: it fails with ErrBusy while
// any lease or reservation is active and blocks new Acquire calls until the
// returned function runs. When committed, the connection is invalidated.
func (m *PoolManager) Quiesce(id uuid.UUID) (func(bool), error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, ErrClosed
	}
	e := m.entryLocked(id)
	if e.active > 0 || e.quiesced {
		m.gcLocked(id, e)
		m.mu.Unlock()
		return nil, ErrBusy
	}
	e.quiesced = true
	m.mu.Unlock()
	var once sync.Once
	return func(committed bool) {
		once.Do(func() {
			if committed {
				m.Invalidate(id)
			}
			m.mu.Lock()
			if e := m.entries[id]; e != nil {
				e.quiesced = false
				m.gcLocked(id, e)
			}
			m.mu.Unlock()
		})
	}, nil
}

// ActiveLeases reports leases and in-flight reservations for id.
func (m *PoolManager) ActiveLeases(id uuid.UUID) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e := m.entries[id]; e != nil {
		return e.active
	}
	return 0
}

// Close refuses new admissions, waits up to the drain deadline for active
// leases, then closes every pool.
func (m *PoolManager) Close() {
	m.mu.Lock()
	m.closed = true
	deadline := time.Now().Add(m.drainTimeout)
	m.mu.Unlock()
	for {
		m.mu.Lock()
		active := 0
		for _, e := range m.entries {
			active += e.active
		}
		m.mu.Unlock()
		if active == 0 || !time.Now().Before(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	m.mu.Lock()
	entries := m.entries
	m.entries = make(map[uuid.UUID]*entry)
	m.mu.Unlock()
	for _, e := range entries {
		if e.viewer != nil {
			_ = e.viewer.Close()
		}
		if e.current != nil {
			e.current.pools.close()
		}
		for g := range e.retired {
			g.pools.close()
		}
	}
}

func (m *PoolManager) IdleTimeout() time.Duration { return time.Minute }
