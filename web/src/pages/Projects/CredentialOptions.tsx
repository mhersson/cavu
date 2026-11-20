import type { Settings } from '../../api/types.ts'

export default function CredentialOptions({ credentials }: { credentials: Settings['forge_credentials'] }) {
  return (
    <>
      <option value="">the default</option>
      {credentials.map((c) => (
        <option key={c.name} value={c.name}>
          {c.name}
        </option>
      ))}
    </>
  )
}
