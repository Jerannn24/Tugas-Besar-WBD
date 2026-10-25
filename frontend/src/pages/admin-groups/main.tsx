import { useCallback, useState, type ReactNode } from 'react'
import { createGroup, deleteGroup, listConnections, listGroups, listUsers, updateGroup } from '../../api/endpoints'
import type { ID } from '../../api/types'
import { Alert } from '../../components/Alert'
import { Button } from '../../components/Button'
import { ConfirmDialog } from '../../components/ConfirmDialog'
import { EmptyState } from '../../components/EmptyState'
import { FailureAlert } from '../../components/FailureAlert'
import { Layout } from '../../components/Layout'
import { Loading } from '../../components/Loading'
import { MutationAlert } from '../../components/MutationAlert'
import { Panel } from '../../components/Panel'
import { ADMIN_FORBIDDEN, useLoad, type LoadState } from '../../lib/load'
import { mountPage } from '../../lib/mount'
import { describeFailure, type MutationFailure } from '../../lib/mutation'
import { useBeforeUnload } from '../../lib/unload'
import { GrantEditor } from './GrantEditor'
import { GroupName } from './GroupName'
import { MemberEditor } from './MemberEditor'

type SectionProps<T> = {
  state: LoadState<T>
  title: string
  loadingLabel: string
  onRetry: () => void
  children: (data: T) => ReactNode
}

function Section<T>({ state, title, loadingLabel, onRetry, children }: SectionProps<T>) {
  if (state.status === 'ready') return <>{children(state.data)}</>
  return (
    <Panel title={title}>
      {state.status === 'loading' ? (
        <Loading label={loadingLabel} />
      ) : (
        <FailureAlert failure={state.failure} forbiddenText={ADMIN_FORBIDDEN} onRetry={onRetry} />
      )}
    </Panel>
  )
}

function AccessExplainer() {
  return (
    <Alert variant="info" title="Cara akses database diberikan">
      <ul className="mb-0">
        <li>Akses diberikan <strong>per database</strong> melalui group: View Only atau View + Edit. Tidak ada grant langsung ke user.</li>
        <li>User di beberapa group memperoleh level tertinggi untuk tiap database.</li>
        <li>
          Status <strong>Platform Admin bukan level akses database</strong>. Admin tetap perlu menjadi anggota group yang
          punya grant untuk membuka atau mengubah database.
        </li>
      </ul>
    </Alert>
  )
}

function GroupsPage() {
  const groupsLoad = useLoad(listGroups)
  const usersLoad = useLoad(listUsers)
  const connectionsLoad = useLoad(listConnections)
  const update = groupsLoad.update
  const [selectedId, setSelectedId] = useState<ID | null>(null)
  const [pendingSelect, setPendingSelect] = useState<ID | null | undefined>(undefined)
  const [memberDirty, setMemberDirty] = useState(false)
  const [grantDirty, setGrantDirty] = useState(false)
  const [deleteOpen, setDeleteOpen] = useState(false)
  const [deletePending, setDeletePending] = useState(false)
  const [deleteFailure, setDeleteFailure] = useState<MutationFailure | null>(null)
  const [notice, setNotice] = useState<string | null>(null)

  const dirty = memberDirty || grantDirty
  useBeforeUnload(dirty)
  const onMemberDirty = useCallback((d: boolean) => setMemberDirty(d), [])
  const onGrantDirty = useCallback((d: boolean) => setGrantDirty(d), [])

  const groups = groupsLoad.state.status === 'ready' ? groupsLoad.state.data.items : []
  const selected = groups.find((g) => g.id === selectedId) ?? null

  function select(id: ID | null) {
    if (id === selectedId) return
    if (dirty) {
      setPendingSelect(id)
      return
    }
    applySelect(id)
  }

  function applySelect(id: ID | null) {
    setSelectedId(id)
    setMemberDirty(false)
    setGrantDirty(false)
    setDeleteFailure(null)
    setNotice(null)
    setPendingSelect(undefined)
  }

  async function confirmDelete() {
    if (!selected || deletePending) return
    setDeletePending(true)
    setDeleteFailure(null)
    const res = await deleteGroup(selected.id)
    setDeletePending(false)
    setDeleteOpen(false)
    if (res.ok) {
      update((d) => ({ ...d, items: d.items.filter((g) => g.id !== selected.id) }))
      applySelect(null)
      setNotice(`Group ${selected.name} dihapus.`)
      return
    }
    setDeleteFailure(describeFailure(res, 'Group gagal dihapus'))
  }

  return (
    <>
      <AccessExplainer />
      {notice && <Alert variant="success">{notice}</Alert>}
      <div className="admin-layout">
        <div>
          {selected ? (
            <>
              <Panel
                title={<>Group: {selected.name}</>}
                actions={
                  <Button variant="danger" className="btn-sm" onClick={() => setDeleteOpen(true)}>
                    Hapus group
                  </Button>
                }
              >
                <MutationAlert failure={deleteFailure} />
                <GroupName
                  key={selected.id}
                  initial={selected.name}
                  label="Nama group"
                  submitLabel="Ganti nama"
                  failureTitle="Nama group gagal diganti"
                  submit={(name) => updateGroup(selected.id, { name })}
                  onDone={(g) => update((d) => ({ ...d, items: d.items.map((x) => (x.id === g.id ? g : x)) }))}
                />
              </Panel>
              <Section
                state={usersLoad.state}
                title="Anggota"
                loadingLabel="Memuat daftar user…"
                onRetry={() => void usersLoad.reload()}
              >
                {(data) => (
                  <MemberEditor
                    key={`m-${selected.id}`}
                    group={selected}
                    users={data.items}
                    onDirtyChange={onMemberDirty}
                  />
                )}
              </Section>
              <Section
                state={connectionsLoad.state}
                title="Grant database"
                loadingLabel="Memuat daftar connection…"
                onRetry={() => void connectionsLoad.reload()}
              >
                {(data) => (
                  <GrantEditor
                    key={`g-${selected.id}`}
                    group={selected}
                    connections={data.items}
                    onDirtyChange={onGrantDirty}
                  />
                )}
              </Section>
            </>
          ) : (
            <Panel title="Detail group">
              <p className="text-secondary mb-0">Pilih group di daftar untuk mengatur anggota dan grant database.</p>
            </Panel>
          )}
        </div>
        <div>
          <Section
            state={groupsLoad.state}
            title="Daftar group"
            loadingLabel="Memuat daftar group…"
            onRetry={() => void groupsLoad.reload()}
          >
            {() => (
              <>
                <Panel title="Daftar group">
                  {groups.length === 0 ? (
                    <EmptyState title="Belum ada group">Buat group di bawah lalu beri anggota dan grant.</EmptyState>
                  ) : (
                    <div className="list-group">
                      {groups.map((g) => (
                        <button
                          key={g.id}
                          type="button"
                          className={`list-group-item list-group-item-action${g.id === selectedId ? ' active' : ''}`}
                          aria-current={g.id === selectedId ? 'true' : undefined}
                          onClick={() => select(g.id)}
                        >
                          {g.name}
                        </button>
                      ))}
                    </div>
                  )}
                </Panel>
                <Panel title="Buat group">
                  <GroupName
                    label="Nama group"
                    submitLabel="Buat group"
                    failureTitle="Group gagal dibuat"
                    submit={(name) => createGroup({ name })}
                    onDone={(g) => {
                      update((d) => ({ ...d, items: [...d.items, g] }))
                      select(g.id)
                    }}
                  />
                </Panel>
              </>
            )}
          </Section>
        </div>
      </div>

      <ConfirmDialog
        open={deleteOpen}
        title="Hapus group?"
        danger
        confirmLabel="Hapus"
        pending={deletePending}
        onConfirm={() => void confirmDelete()}
        onCancel={() => setDeleteOpen(false)}
      >
        <p>
          Group <strong>{selected?.name}</strong> akan dihapus beserta seluruh keanggotaan dan grant database-nya.
        </p>
        <p className="mb-0 text-secondary">
          Anggota dapat kehilangan akses dan session mereka dicabut. User dan riwayat migration tidak ikut terhapus.
        </p>
      </ConfirmDialog>

      <ConfirmDialog
        open={pendingSelect !== undefined}
        title="Buang perubahan?"
        confirmLabel="Buang perubahan"
        danger
        onConfirm={() => applySelect(pendingSelect ?? null)}
        onCancel={() => setPendingSelect(undefined)}
      >
        Anggota atau grant group ini belum disimpan. Perubahan akan hilang jika Anda pindah group.
      </ConfirmDialog>
    </>
  )
}

mountPage(
  <Layout activePage="admin-groups" title="Admin: Groups">
    <GroupsPage />
  </Layout>,
)
