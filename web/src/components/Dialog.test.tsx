import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { expect, it, vi } from 'vitest'
import Dialog from './Dialog.tsx'

it('the dialog is named by its title', () => {
  render(<Dialog title="New task" onClose={() => {}}>body</Dialog>)
  expect(screen.getByRole('dialog', { name: 'New task' })).toHaveAttribute('aria-modal', 'true')
})

it('Escape closes the dialog', async () => {
  const onClose = vi.fn()
  render(<Dialog title="New task" onClose={onClose}>body</Dialog>)
  await userEvent.keyboard('{Escape}')
  expect(onClose).toHaveBeenCalledTimes(1)
})
