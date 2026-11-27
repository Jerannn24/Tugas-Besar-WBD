import type { DatabaseSummary, Proposal } from '../../api/types'
import { Button } from '../../components/Button'
import { ConfirmDialog } from '../../components/ConfirmDialog'
import { Panel } from '../../components/Panel'
import { WorkflowAlert } from '../../components/WorkflowAlert'
import type { useEnqueue } from './enqueue'

type Props = {
  database: DatabaseSummary
  proposal: Proposal
  enqueue: ReturnType<typeof useEnqueue>
}

export function EnqueuePanel({ database, proposal, enqueue }: Props) {
  const { state } = enqueue
  const canEnqueue = proposal.allowed_actions.includes('enqueue')
  const posting = state.phase === 'posting'

  return (
    <Panel title="Execute">
      <p className="text-secondary">
        Execute memasukkan proposal ke antrean. Worker menjalankan SQL secara asinkron; job yang diterima belum berarti SQL selesai.
      </p>

      {state.phase === 'failed' && (
        <WorkflowAlert
          failure={state.failure}
          title="Execute ditolak"
          note={
            state.resend
              ? 'Kirim ulang memakai request ID yang sama sehingga job tidak dibuat dua kali.'
              : 'Proposal dimuat ulang untuk menampilkan keadaan terbaru.'
          }
        />
      )}

      <div className="d-flex flex-wrap align-items-center gap-2">
        {enqueue.hasIntent ? (
          <Button variant="success" pending={posting} onClick={enqueue.resend}>
            Kirim ulang
          </Button>
        ) : (
          canEnqueue && (
            <Button variant="success" pending={posting} onClick={enqueue.open}>
              Execute revision {proposal.revision}
            </Button>
          )
        )}
      </div>

      <ConfirmDialog
        open={state.phase === 'confirming'}
        title="Masukkan ke antrean?"
        confirmLabel="Ya, execute"
        onCancel={enqueue.cancel}
        onConfirm={() => enqueue.confirm(proposal.revision)}
      >
        <p>
          Proposal revision <strong>{proposal.revision}</strong> untuk database <strong>{database.name}</strong> akan
          dimasukkan ke antrean. Worker memvalidasi ulang approval sebelum menjalankan SQL.
        </p>
        <pre className="sql-block">{proposal.snapshot.sql_up}</pre>
      </ConfirmDialog>
    </Panel>
  )
}
