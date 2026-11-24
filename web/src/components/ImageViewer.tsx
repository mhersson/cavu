import { ext, size } from '../api/images.ts'
import Dialog from './Dialog.tsx'
import { DownloadIcon, XIcon } from './icons.tsx'
import { useNarrow } from './useNarrow.ts'

type Props = { name: string; size: number; src: string; download: string; onClose: () => void }

// An image opened: it fills the window under a bar with its name, what it is, and what can be done.
export default function ImageViewer({ name, size: bytes, src, download, onClose }: Props) {
  const narrow = useNarrow()
  const save = (
    <a className="btn" href={download} download>
      <DownloadIcon />
      Download
    </a>
  )
  return (
    <Dialog title={name} className="viewer" onClose={onClose}>
      <div className="vbar">
        <span className="t2">
          <span>{ext(name)}</span>
          <span>{size(bytes)}</span>
        </span>
        <span className="spacer" />
        {!narrow && save}
        <button className="btn" onClick={onClose}>
          {!narrow && <XIcon />}
          Close
        </button>
      </div>
      <img src={src} alt="" />
      {narrow && <div className="vfoot">{save}</div>}
    </Dialog>
  )
}
