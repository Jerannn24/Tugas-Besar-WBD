import { useCallback, useEffect, useRef, useState } from 'react'
import { Link } from 'react-router'
import { errorMessage, type ApiFailure } from '../../api/client'
import type { DatabaseSummary, ID, Proposal } from '../../api/types'
import { getProposal } from '../../api/workflow'
import { Alert } from '../../components/Alert'
import { Button } from '../../components/Button'
import { FailureAlert } from '../../components/FailureAlert'
import { MigrationSpec } from '../../components/MigrationSpec'
import { Panel } from '../../components/Panel'
import { ShortId } from '../../components/ShortId'
import { ListSkeleton } from '../../components/Skeleton'
import { ProposalBadge } from '../../components/StateBadge'
import { WorkflowAlert } from '../../components/WorkflowAlert'
import { paths } from '../../lib/paths'
import { useSession } from '../../lib/session'
import { useProposalActions } from './actions'
import { Approvals } from './Approvals'
import { EnqueuePanel } from './EnqueuePanel'
import { useEnqueue } from './enqueue'
import { ACTION_LABELS } from './labels'
import { ValidationResult } from './ValidationResult'

type LoadState =
  | { status: 'loading' }
  | { status: 'failed'; failure: ApiFailure }
  | { status: 'ready'; proposal: Proposal; refreshing: boolean; refreshFailure: ApiFailure | null }

type Props = { database: DatabaseSummary; proposalId: ID }

export function ProposalDetail({ database, proposalId }: Props) {
  const [state, setState] = useState<LoadState>({ status: 'loading' })
  const seq = useRef(0)
  const me = useSession().user.id

  const request = useCallback(
    async (mode: 'initial' | 'refresh') => {
      const mine = ++seq.current
      const res = await getProposal(database.id, proposalId)
      if (mine !== seq.current) return
      setState((s) => {
        if (res.ok) return { status: 'ready', proposal: res.data, refreshing: false, refreshFailure: null }
        if (mode === 'refresh' && s.status === 'ready') return { ...s, refreshing: false, refreshFailure: res }
        return { status: 'failed', failure: res }
      })
    },
    [database.id, proposalId],
  )

  const fetchProposal = useCallback(
    (mode: 'initial' | 'refresh') => {
      setState((s) =>
        mode === 'refresh' && s.status === 'ready' ? { ...s, refreshing: true, refreshFailure: null } : { status: 'loading' },
      )
      return request(mode)
    },
    [request],
  )

  useEffect(() => {
    void request('initial')
    const ref = seq
    return () => {
      ref.current++
    }
  }, [request])

  const refresh = useCallback(() => void fetchProposal('refresh'), [fetchProposal])
  const replace = useCallback((proposal: Proposal) => {
    seq.current++
    setState({ status: 'ready', proposal, refreshing: false, refreshFailure: null })
  }, [])

  const actions = useProposalActions({ databaseId: database.id, proposalId, onProposal: replace, onReload: refresh })
  const enqueue = useEnqueue({ databaseId: database.id, proposalId, onReload: refresh })

  if (state.status === 'loading') {
    return (
      <Panel title="Proposal">
        <ListSkeleton label="Memuat proposal…" items={6} heading />
      </Panel>
    )
  }
  if (state.status === 'failed') {
    return (
      <FailureAlert
        failure={state.failure}
        notFoundTitle="Proposal tidak ditemukan"
        invalidTitle="ID proposal tidak valid"
        onRetry={() => void fetchProposal('initial')}
      />
    )
  }

  const p = state.proposal
  const allowed = p.allowed_actions
  const result = actions.result
  const showEnqueue = allowed.includes('enqueue') || enqueue.hasIntent || enqueue.state.phase === 'failed'

  return (
    <>
      <Panel
        title={
          <span className="d-inline-flex flex-wrap align-items-center gap-2">
            {p.snapshot.title} <ProposalBadge status={p.status} />
          </span>
        }
        actions={
          <Button variant="outline-secondary" pending={state.refreshing} onClick={refresh}>
            Muat ulang
          </Button>
        }
      >
        <dl className="detail-list">
          <dt>Proposal</dt>
          <dd>
            <ShortId id={p.proposal_id} />
          </dd>
          <dt>Aksi</dt>
          <dd>{ACTION_LABELS[p.action] ?? p.action}</dd>
          <dt>Revision</dt>
          <dd>{p.revision}</dd>
          <dt>Migration</dt>
          <dd>
            {database.access === 'edit' ? (
              <Link to={paths.draft(database.id, p.migration_id)}>
                <ShortId id={p.migration_id} />
              </Link>
            ) : (
              <ShortId id={p.migration_id} />
            )}
          </dd>
          <dt>Source execution</dt>
          <dd>{p.source_execution_id ? <ShortId id={p.source_execution_id} /> : '—'}</dd>
        </dl>

        {state.refreshFailure && (
          <Alert variant="danger" title="Gagal memuat ulang proposal">
            {errorMessage(state.refreshFailure)} Data di bawah mungkin bukan yang terbaru.
          </Alert>
        )}
        <StatusNote proposal={p} />
        {result && 'success' in result && <Alert variant="success" title={result.success} />}
        {result && 'failure' in result && <WorkflowAlert failure={result.failure} title={result.title} />}

        {allowed.includes('submit_review') && (
          <Button
            pending={actions.busy?.kind === 'submit'}
            disabled={actions.busy !== null}
            onClick={() => void actions.submit(p.revision)}
          >
            Ajukan review (validasi)
          </Button>
        )}
      </Panel>

      <div className="row row-cards">
        <div className="col-12 col-lg-5 order-lg-2">
          <Panel title="Validasi">
            <ValidationResult validation={p.validation} />
          </Panel>
          <Panel title="Approval">
            <Approvals
              approvals={p.approvals}
              meId={me}
              canApprove={allowed.includes('approve')}
              pendingGroup={actions.busy?.kind === 'approve' ? actions.busy.groupId : null}
              onApprove={(groupId) => void actions.approve(p.revision, groupId)}
            />
          </Panel>
          {showEnqueue && <EnqueuePanel database={database} proposal={p} enqueue={enqueue} />}
        </div>
        <div className="col-12 col-lg-7 order-lg-1">
          <Panel title="Snapshot">
            <MigrationSpec spec={p.snapshot} />
          </Panel>
        </div>
      </div>
    </>
  )
}

function StatusNote({ proposal }: { proposal: Proposal }) {
  switch (proposal.status) {
    case 'draft':
      return proposal.validation?.status === 'failed' ? null : (
        <p className="text-secondary">Ajukan review untuk menjalankan validasi dan meminta approval.</p>
      )
    case 'pending_review':
      return <p className="text-secondary">Menunggu approval dari leader setiap group yang wajib.</p>
    case 'approved':
      return <p className="text-secondary">Semua approval lengkap. Proposal siap dimasukkan ke antrean.</p>
    case 'queued':
      return (
        <Alert variant="info" title="Sudah diantrekan">
          Proposal ini sudah masuk antrean. Pantau statusnya dari halaman job.
        </Alert>
      )
    case 'superseded':
      return (
        <Alert variant="warning" title="Proposal usang">
          Draft sudah berubah sejak proposal ini dibuat. Ajukan proposal baru dari draft.
        </Alert>
      )
  }
}
