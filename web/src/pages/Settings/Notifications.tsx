import { useState } from 'react'
import { pushState, turnOff, turnOn } from '../../push.ts'

const lines = {
  off: 'Off.',
  on: 'On. None arrive while CAVU has the focus in this browser.',
  'install-first': 'On an iPhone or iPad, add CAVU to the Home Screen first, then turn notifications on in the app it adds.',
  denied: 'This browser blocks notifications from CAVU. Allow them in its settings for this site, then reload.',
  unsupported: 'Notifications need CAVU on an https address, in a browser with Web Push.',
}

// pushKey is unset until the settings are read; it is the key the browser subscribes with.
export default function Notifications({ pushKey }: { pushKey?: string }) {
  const [state, setState] = useState(pushState)
  const [sending, setSending] = useState(false)
  const [refused, setRefused] = useState('')

  // The action is called inside the click, so the permission request stays in the gesture.
  function act() {
    setSending(true)
    setRefused('')
    ;(state === 'on' ? turnOff() : turnOn(pushKey!))
      .catch((e: unknown) => setRefused(e instanceof Error ? e.message : String(e)))
      .finally(() => {
        setState(pushState())
        setSending(false)
      })
  }

  const button = state === 'off' || state === 'on'
  return (
    <>
      <div className="frow">
        <label>Notifications</label>
        <div className="secret">
          <span className="notify">{lines[state]}</span>
          {button && (
            <button className="btn" disabled={sending || (state === 'off' && !pushKey)} onClick={act}>
              {state === 'on' ? 'Turn off' : 'Turn on'}
            </button>
          )}
        </div>
      </div>
      {refused && (
        <p role="alert" className="note err">
          {refused}
        </p>
      )}
    </>
  )
}
