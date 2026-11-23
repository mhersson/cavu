import { useRef, useState, type ChangeEvent } from 'react'
import { ext, imageTypes, imageURL, removeImage, size, type Uploads } from '../api/images.ts'
import type { Image, Task } from '../api/types.ts'
import Dialog from './Dialog.tsx'
import ImageViewer from './ImageViewer.tsx'
import { AttachIcon, CameraIcon, XIcon } from './icons.tsx'
import { useNarrow } from './useNarrow.ts'
import './Images.css'

type Props = { task: Task; up: Uploads; onTask: (t: Task) => void }

// Over a task a file is dragged across: where it goes, and what it may be.
export function DropBox({ cap }: { cap: number }) {
  return (
    <div className="drop">
      <div>
        <AttachIcon />
        Drop to attach it to this task
        <span>PNG, JPEG, GIF or WebP{cap > 0 && `, at most ${size(cap)}`}.</span>
      </div>
    </div>
  )
}

// A task's images, each opening to fill the window. A draft's take more and lose one asked first; any other task's are
// read only, and show only when there are some or an upload has news. On a phone the section folds, open at first.
export default function Images({ task, up, onTask }: Props) {
  const narrow = useNarrow()
  const [opened, setOpened] = useState<Image>()
  const [removing, setRemoving] = useState<Image>()
  const picker = useRef<HTMLInputElement>(null)
  const camera = useRef<HTMLInputElement>(null)
  const draft = task.status === 'draft'
  const n = up.going.length
  // Emptied, so the same file can be chosen again.
  const chosen = (e: ChangeEvent<HTMLInputElement>) => {
    up.send([...(e.target.files ?? [])])
    e.target.value = ''
  }
  const remove = (img: Image) =>
    removeImage(task.id, img.id).then(
      (t) => {
        up.refuse('')
        onTask(t)
      },
      (e: unknown) => up.refuse(e instanceof Error ? e.message : String(e)),
    )

  if (!draft && task.images.length + n === 0 && !up.refused) return null
  const head = (
    <>
      Images <span className="dim">{task.images.length + n}</span>
    </>
  )
  const body = (
    <>
      <div className="tiles">
        {task.images.map((img) => (
          <div key={img.id} className="tile">
            <button className="tile-open" onClick={() => setOpened(img)}>
              <img src={imageURL(task.id, img.id)} alt="" loading="lazy" />
              <span className="nm">{img.name}</span>
            </button>
            {draft && (
              <button className="x" aria-label={`Remove ${img.name}`} onClick={() => setRemoving(img)}>
                <XIcon />
              </button>
            )}
          </div>
        ))}
        {up.going.map((u) => (
          <div key={u.key} className="tile up" aria-busy="true">
            <span className="doc">
              {ext(u.name)} · {size(u.size)}
            </span>
            <span className="bar" />
            <span className="nm">{u.name}</span>
          </div>
        ))}
      </div>
      {n > 0 && (
        <p className="fline">
          <AttachIcon />
          {n === 1 ? 'Uploading 1 image. Leaving the task stops it.' : `Uploading ${n} images. Leaving the task stops them.`}
        </p>
      )}
      {up.refused && (
        <p role="alert" className="err">
          {up.refused}
        </p>
      )}
      {draft &&
        (narrow ? (
          <div className="acts two">
            <button className="btn" onClick={() => camera.current!.click()}>
              <CameraIcon />
              Take a photo
            </button>
            <button className="btn" onClick={() => picker.current!.click()}>
              <AttachIcon />
              Choose an image
            </button>
          </div>
        ) : (
          <p className="note">Drop an image on the task, or paste one, to attach it.{up.cap > 0 && ` At most ${size(up.cap)} each.`}</p>
        ))}
      {draft && (
        <>
          <input ref={picker} type="file" accept={imageTypes.join(',')} multiple hidden onChange={chosen} />
          {narrow && <input ref={camera} type="file" accept="image/*" capture="environment" hidden onChange={chosen} />}
        </>
      )}
      {opened && (
        <ImageViewer
          name={opened.name}
          size={opened.size}
          src={imageURL(task.id, opened.id)}
          download={imageURL(task.id, opened.id) + '?download=1'}
          onClose={() => setOpened(undefined)}
        />
      )}
      {removing && (
        <Dialog title={`Remove ${removing.name}?`} onClose={() => setRemoving(undefined)}>
          <p className="note">The image is removed from the task for good. This cannot be undone.</p>
          <div className="acts">
            <button className="btn" onClick={() => setRemoving(undefined)}>
              Keep it
            </button>
            <button
              className="btn danger"
              onClick={() => {
                setRemoving(undefined)
                void remove(removing)
              }}
            >
              Remove
            </button>
          </div>
        </Dialog>
      )}
    </>
  )
  return narrow ? (
    <details className="sect" open>
      <summary>{head}</summary>
      {body}
    </details>
  ) : (
    <section className="sect">
      <div className="sect-head">
        <h2>{head}</h2>
        {draft && (
          <button className="btn link" onClick={() => picker.current!.click()}>
            <AttachIcon />
            Attach an image
          </button>
        )}
      </div>
      {body}
    </section>
  )
}
