import { useCallback } from 'react'
import { useBlocker, type BlockerFunction } from 'react-router'
import { paths } from '../lib/paths'
import { useBeforeUnload } from '../lib/unload'
import { ConfirmDialog } from './ConfirmDialog'

type Props = { when: boolean; message?: string }

export function LeaveGuard({ when, message = 'Perubahan yang belum disimpan akan hilang.' }: Props) {
  useBeforeUnload(when)

  const shouldBlock = useCallback<BlockerFunction>(
    ({ currentLocation, nextLocation }) =>
      when &&
      nextLocation.pathname !== paths.login &&
      (currentLocation.pathname !== nextLocation.pathname || currentLocation.search !== nextLocation.search),
    [when],
  )
  const blocker = useBlocker(shouldBlock)

  return (
    <ConfirmDialog
      open={blocker.state === 'blocked'}
      title="Tinggalkan halaman ini?"
      confirmLabel="Tinggalkan"
      cancelLabel="Tetap di sini"
      danger
      onCancel={() => blocker.reset?.()}
      onConfirm={() => blocker.proceed?.()}
    >
      {message}
    </ConfirmDialog>
  )
}
