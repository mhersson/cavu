import { render, screen } from '@testing-library/react'
import { expect, it } from 'vitest'
import Field from './Field.tsx'

it("a field's label names its control", () => {
  render(
    <>
      <Field label="Title">
        <input />
      </Field>
      <Field label="Start now" check>
        <input type="checkbox" />
      </Field>
    </>,
  )
  expect(screen.getByLabelText('Title')).toBeInstanceOf(HTMLInputElement)
  expect(screen.getByLabelText('Start now')).toHaveAttribute('type', 'checkbox')
})
