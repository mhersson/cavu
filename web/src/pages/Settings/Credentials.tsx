import { useEffect, useRef, useState } from 'react'
import { api } from '../../api/client.ts'
import type { Project, Settings as Saved } from '../../api/types.ts'
import Dialog from '../../components/Dialog.tsx'
import Choice from './Choice.tsx'

type Kind = 'token' | 'app'

// One form for Add and every Replace; the default's Replace has the name "".
// chosen says a key file is chosen; its text is read only as it is sent.
type Form = { add: boolean; name: string; kind: Kind; token: string; appId: string; chosen: boolean }

const kinds: [Kind, string][] = [
  ['token', 'A token'],
  ['app', 'A GitHub App'],
]

const line = (e: unknown) => (e instanceof Error ? e.message : String(e))

// The default writes the settings and clears the other kind's; a named one is written whole.
function request(f: Form, key: string) {
  const app = f.kind === 'app'
  if (f.add || f.name) return { forge_credentials: { [f.name]: app ? { app_id: Number(f.appId), app_key: key } : { token: f.token } } }
  return app
    ? { settings: { forge_app_id: f.appId }, secrets: { forge_app_key: key, forge_token: '' } }
    : { settings: { forge_app_id: null }, secrets: { forge_token: f.token, forge_app_key: '' } }
}

export default function Credentials({ saved, onSaved }: { saved: Saved; onSaved: (s: Saved) => void }) {
  const [used, setUsed] = useState<Project[] | null>(null)
  const [form, setForm] = useState<Form | null>(null)
  const [removing, setRemoving] = useState('')
  const [refused, setRefused] = useState('')
  const file = useRef<HTMLInputElement>(null)

  useEffect(() => {
    api<{ projects: Project[] }>('GET', '/projects').then((r) => setUsed(r.projects), (e: unknown) => setRefused(line(e)))
  }, [])

  const edit = (p: Partial<Form>) => setForm((f) => f && { ...f, ...p })

  async function send(body: object) {
    setRefused('')
    try {
      onSaved(await api<Saved>('PATCH', '/settings', body))
      setForm(null)
    } catch (e) {
      setRefused(line(e))
    }
  }

  // The key is read only as it is sent, so its text is never kept.
  async function submit(f: Form) {
    await send(request(f, (await file.current?.files?.[0]?.text()) ?? ''))
  }

  function remove() {
    setRemoving('')
    void send({ forge_credentials: { [removing]: null } })
  }

  // The default's kind as forge.Token decides it: an App id wins over a token.
  const appId = String(saved.settings.forge_app_id ?? '')
  const kind: Kind | undefined = appId ? 'app' : saved.secrets.forge_token ? 'token' : undefined
  const rows = [{ name: '', kind, app_id: appId }, ...saved.forge_credentials]

  return (
    <>
      <p className="note">A project uses the credential it names, and the default when it names none.</p>
      <table className="rows creds">
        <tbody>
          {rows.map((c) => {
            const users = used?.filter((p) => p.forge_credential === c.name).map((p) => p.name)
            return (
              <tr key={c.name}>
                <td className="cname">{c.name || 'default'}</td>
                <td>
                  {c.kind === 'app' ? (
                    <>
                      A GitHub App, id <code>{c.app_id}</code>
                      {!c.name && !saved.secrets.forge_app_key && ', its key is not set'}
                    </>
                  ) : c.kind ? (
                    'A token'
                  ) : (
                    'Not set.'
                  )}
                </td>
                <td className="wait">{users && (users.length ? 'Used by ' + users.join(', ') : 'No project uses it')}</td>
                <td className="cacts">
                  <button className="btn" onClick={() => setForm({ add: false, name: c.name, kind: c.kind ?? 'token', token: '', appId: String(c.app_id ?? ''), chosen: false })}>
                    {c.kind ? 'Replace' : 'Set'}
                  </button>
                  {c.name && users?.length === 0 && (
                    <button className="btn danger" onClick={() => setRemoving(c.name)}>
                      Remove
                    </button>
                  )}
                </td>
              </tr>
            )
          })}
        </tbody>
      </table>
      {form ? (
        // A new target is a new form, so a file chosen for one is never sent for another.
        <div className="addcred" key={form.add ? 'add' : 'replace-' + form.name}>
          <div className="frow">
            <label htmlFor="cred-name">Name</label>
            {form.add ? (
              <input
                id="cred-name"
                type="text"
                className="in mono"
                autoComplete="off"
                spellCheck={false}
                placeholder="lower-case letters, digits and hyphens"
                value={form.name}
                onChange={(e) => edit({ name: e.target.value })}
              />
            ) : (
              <span className="mono">{form.name || 'default'}</span>
            )}
          </div>
          <Choice id="cred-kind" label="Kind" options={kinds} value={form.kind} onChange={(kind) => edit(kind === form.kind ? {} : { kind, chosen: false })} />
          {form.kind === 'token' ? (
            <div className="frow">
              <label htmlFor="cred-token">Token</label>
              <input id="cred-token" type="password" className="in" autoComplete="new-password" value={form.token} onChange={(e) => edit({ token: e.target.value })} />
            </div>
          ) : (
            <>
              <div className="frow">
                <label htmlFor="cred-app">App id</label>
                <input id="cred-app" type="text" className="in mono" autoComplete="off" spellCheck={false} value={form.appId} onChange={(e) => edit({ appId: e.target.value })} />
              </div>
              <div className="frow">
                <label htmlFor="cred-key">Private key</label>
                <input id="cred-key" type="file" accept=".pem" className="in" ref={file} onChange={(e) => edit({ chosen: !!e.target.files?.length })} />
              </div>
            </>
          )}
          <button className="btn primary" disabled={form.kind === 'token' ? !form.token : !form.chosen || !form.appId} onClick={() => void submit(form)}>
            {form.add ? 'Add' : 'Replace'}
          </button>{' '}
          <button className="btn" onClick={() => setForm(null)}>
            Cancel
          </button>
        </div>
      ) : (
        <button className="btn" onClick={() => setForm({ add: true, name: '', kind: 'token', token: '', appId: '', chosen: false })}>
          Add a credential
        </button>
      )}
      {refused && (
        <p role="alert" className="note err">
          {refused}
        </p>
      )}
      <p className="note">A credential that a project uses cannot be removed.</p>
      {removing && (
        <Dialog title={`Remove the credential ${removing}?`} onClose={() => setRemoving('')}>
          <div className="acts">
            <button className="btn" onClick={() => setRemoving('')}>
              Keep it
            </button>
            <button className="btn danger" onClick={remove}>
              Remove
            </button>
          </div>
        </Dialog>
      )}
    </>
  )
}
