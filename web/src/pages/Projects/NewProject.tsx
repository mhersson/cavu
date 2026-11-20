import { useEffect, useState, type FormEvent, type ReactNode } from 'react'
import { api } from '../../api/client.ts'
import type { Project, Settings } from '../../api/types.ts'
import Dialog from '../../components/Dialog.tsx'
import Field from '../../components/Field.tsx'
import CredentialOptions from './CredentialOptions.tsx'

const blank = { name: '', repo_url: '', base_branch: 'main', forge_credential: '', setup_command: '', verify_command: '', verify_waived: false }

type Props = { submit: string; onCreated: (name: string) => void; onCancel?: () => void; children?: ReactNode }

// The server asks for no check of a new project, so Create asks for one; its refusals are shown as said.
export function NewProjectForm({ submit, onCreated, onCancel, children }: Props) {
  const [form, setForm] = useState(blank)
  const [credentials, setCredentials] = useState<Settings['forge_credentials']>([])
  const [refused, setRefused] = useState('')
  const [sending, setSending] = useState(false)

  useEffect(() => {
    api<Settings>('GET', '/settings')
      .then((s) => setCredentials(s.forge_credentials))
      .catch(() => {}) // the project uses the default
  }, [])

  const set = (key: keyof typeof blank) => (e: { target: { value: string } }) => setForm((f) => ({ ...f, [key]: e.target.value }))

  async function create(e: FormEvent) {
    e.preventDefault()
    setRefused('')
    setSending(true)
    try {
      const made = await api<Project>('POST', '/projects', form)
      await api('POST', `/projects/${made.id}/check`)
      onCreated(made.name)
    } catch (err) {
      setRefused(err instanceof Error ? err.message : String(err))
      setSending(false)
    }
  }

  return (
    <form onSubmit={(e) => void create(e)}>
      <Field label="Name">
        <input className="mono" autoFocus placeholder="lower-case letters, digits and hyphens" value={form.name} onChange={set('name')} />
      </Field>
      <Field label="Repository address">
        <input className="mono" placeholder="https://github.com/you/repository.git" value={form.repo_url} onChange={set('repo_url')} />
      </Field>
      <Field label="Base branch">
        <input className="mono" value={form.base_branch} onChange={set('base_branch')} />
      </Field>
      {/* With no named credential the default is the one choice, so there is nothing to pick. */}
      {credentials.length > 0 && (
        <Field label="GitHub credential">
          <select className="in" value={form.forge_credential} onChange={set('forge_credential')}>
            <CredentialOptions credentials={credentials} />
          </select>
        </Field>
      )}
      <Field label="Setup command">
        <input className="mono" placeholder="what installs the dependencies, or nothing" value={form.setup_command} onChange={set('setup_command')} />
      </Field>
      <Field label="Verify command">
        <input className="mono" placeholder="what must pass before a change is published" value={form.verify_command} onChange={set('verify_command')} />
      </Field>
      <Field label="No verify command: a person checks every change" check>
        <input type="checkbox" checked={form.verify_waived} onChange={(e) => setForm((f) => ({ ...f, verify_waived: e.target.checked }))} />
      </Field>
      {children}
      {refused && (
        <p className="refused" role="alert">
          {refused}
        </p>
      )}
      <div className="acts">
        {onCancel && (
          <button type="button" className="btn" onClick={onCancel}>
            Cancel
          </button>
        )}
        <button className="btn primary" disabled={sending}>
          {submit}
        </button>
      </div>
    </form>
  )
}

export default function NewProject({ onClose, onCreated }: { onClose: () => void; onCreated: (name: string) => void }) {
  return (
    <Dialog title="New project" onClose={onClose}>
      <NewProjectForm submit="Create" onCreated={onCreated} onCancel={onClose}>
        <p className="small">CAVU checks the environment as soon as the project exists.</p>
      </NewProjectForm>
    </Dialog>
  )
}
