import { useRef, useState, type ChangeEvent, type ReactNode } from 'react'
import { ext, imageTypes, imageURL, removeImage, size, type Upload, type Uploads } from '../api/images.ts'
import type { Image, Task } from '../api/types.ts'
import Dialog from './Dialog.tsx'
import ImageViewer from './ImageViewer.tsx'
import { AttachIcon, CameraIcon, XIcon } from './icons.tsx'
import { useNarrow } from './useNarrow.ts'
import './Images.css'

// Where a section is: what a drop goes on, and what stops an upload.
const target = { task: 'the task', dialog: 'the form', page: 'the form' }
const leaving = { task: 'Leaving the task', dialog: 'Closing the form', page: 'Leaving the page' }

// Over what a file is dragged across: where it goes, and what it may be.
export function DropBox({ what, cap }: { what: string; cap: number }) {
  return (
    <div className="drop">
      <div>
        <AttachIcon />
        Drop to attach it to {what}
        <span>PNG, JPEG, GIF or WebP{cap > 0 && `, at most ${size(cap)}`}.</span>
      </div>
    </div>
  )
}

type TileProps = { name: string; onOpen?: () => void; onRemove?: () => void; children: ReactNode }

// One image: its picture and name, which open it when onOpen is given, and an × when onRemove is.
export function Tile({ name, onOpen, onRemove, children }: TileProps) {
  const face = (
    <>
      {children}
      <span className="nm">{name}</span>
    </>
  )
  return (
    <div className="tile">
      {onOpen ? (
        <button type="button" className="tile-open" onClick={onOpen}>
          {face}
        </button>
      ) : (
        face
      )}
      {onRemove && (
        <button type="button" className="x" aria-label={`Remove ${name}`} onClick={onRemove}>
          <XIcon />
        </button>
      )}
    </div>
  )
}

type SectionProps = {
  where: keyof typeof leaving
  tiles: ReactNode[]
  going: Upload[]
  refused: string
  cap: number
  onAdd?: (files: File[]) => void
  children?: ReactNode
}

// Images as tiles under a head with their count, those under way with a moving bar, and what attaches more when onAdd
// is given. On a phone the section folds, open at first.
export function ImagesSection({ where, tiles, going, refused, cap, onAdd, children }: SectionProps) {
  const narrow = useNarrow()
  const picker = useRef<HTMLInputElement>(null)
  const camera = useRef<HTMLInputElement>(null)
  const n = going.length
  // Emptied, so the same file can be chosen again.
  const chosen = (e: ChangeEvent<HTMLInputElement>) => {
    onAdd!([...(e.target.files ?? [])])
    e.target.value = ''
  }
  const head = (
    <>
      Images <span className="dim">{tiles.length + n}</span>
    </>
  )
  const body = (
    <>
      <div className="tiles">
        {tiles}
        {going.map((u) => (
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
          {n === 1 ? `Uploading 1 image. ${leaving[where]} stops it.` : `Uploading ${n} images. ${leaving[where]} stops them.`}
        </p>
      )}
      {refused && (
        <p role="alert" className="err">
          {refused}
        </p>
      )}
      {onAdd &&
        (narrow ? (
          <div className="acts two">
            <button type="button" className="btn" onClick={() => camera.current!.click()}>
              <CameraIcon />
              Take a photo
            </button>
            <button type="button" className="btn" onClick={() => picker.current!.click()}>
              <AttachIcon />
              Choose an image
            </button>
          </div>
        ) : (
          <p className="note">
            Drop an image on {target[where]}, or paste one, to attach it.{cap > 0 && ` At most ${size(cap)} each.`}
          </p>
        ))}
      {onAdd && (
        <>
          <input ref={picker} type="file" accept={imageTypes.join(',')} multiple hidden onChange={chosen} />
          {narrow && <input ref={camera} type="file" accept="image/*" capture="environment" hidden onChange={chosen} />}
        </>
      )}
      {children}
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
        {onAdd && (
          <button type="button" className="btn link" onClick={() => picker.current!.click()}>
            <AttachIcon />
            Attach an image
          </button>
        )}
      </div>
      {body}
    </section>
  )
}

type Props = { task: Task; up: Uploads; onTask: (t: Task) => void }

// A task's images, each opening to fill the window. A draft's take more and lose one asked first; any other task's are
// read only, and show only when there are some or an upload has news.
export default function Images({ task, up, onTask }: Props) {
  const [opened, setOpened] = useState<Image>()
  const [removing, setRemoving] = useState<Image>()
  const draft = task.status === 'draft'
  const remove = (img: Image) =>
    removeImage(task.id, img.id).then(
      (t) => {
        up.refuse('')
        onTask(t)
      },
      (e: unknown) => up.refuse(e instanceof Error ? e.message : String(e)),
    )

  if (!draft && task.images.length + up.going.length === 0 && !up.refused) return null
  return (
    <ImagesSection
      where="task"
      tiles={task.images.map((img) => (
        <Tile key={img.id} name={img.name} onOpen={() => setOpened(img)} onRemove={draft ? () => setRemoving(img) : undefined}>
          <img src={imageURL(task.id, img.id)} alt="" loading="lazy" />
        </Tile>
      ))}
      going={up.going}
      refused={up.refused}
      cap={up.cap}
      onAdd={draft ? up.send : undefined}
    >
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
    </ImagesSection>
  )
}
