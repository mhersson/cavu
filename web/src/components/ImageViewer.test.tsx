import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { narrow } from '../test-server.ts'
import ImageViewer from './ImageViewer.tsx'

afterEach(() => vi.unstubAllGlobals())

const show = (onClose = () => {}) =>
  render(<ImageViewer name="save-button.png" size={190464} src="/api/tasks/7/images/3" download="/api/tasks/7/images/3?download=1" onClose={onClose} />)

test.each([false, true])('the viewer, on a phone %s, shows the image and a download of it, and closes by Close and by Escape', async (phone) => {
  narrow(phone)
  const onClose = vi.fn()
  show(onClose)
  const viewer = screen.getByRole('dialog', { name: 'save-button.png' })
  expect(viewer.querySelector('img')).toHaveAttribute('src', '/api/tasks/7/images/3')
  const download = within(viewer).getByRole('link')
  expect(download).toHaveAttribute('href', '/api/tasks/7/images/3?download=1')
  expect(download).toHaveAttribute('download')
  await userEvent.click(within(viewer).getByRole('button'))
  await userEvent.keyboard('{Escape}')
  expect(onClose).toHaveBeenCalledTimes(2)
})
