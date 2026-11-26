import type { ReactNode } from 'react'

// A line icon on a 16 unit grid, drawn in the colour of its text.
export function Icon({ children }: { children: ReactNode }) {
  return (
    <svg className="ic" viewBox="0 0 16 16" aria-hidden="true">
      {children}
    </svg>
  )
}

export const AttachIcon = () => (
  <Icon>
    <path d="m13 7.5-5.2 5.2a3 3 0 0 1-4.3-4.3L9 2.9a2 2 0 0 1 2.9 2.9l-5.4 5.4a1 1 0 0 1-1.4-1.4L10 4.9" />
  </Icon>
)

export const CameraIcon = () => (
  <Icon>
    <path d="M2 5.5h2.6L6 3.5h4l1.4 2H14V13H2z" />
    <circle cx="8" cy="9" r="2.3" />
  </Icon>
)

export const DownloadIcon = () => (
  <Icon>
    <path d="M8 2.5v8M4.8 7.5 8 10.7l3.2-3.2M3 13.5h10" />
  </Icon>
)

export const XIcon = () => (
  <Icon>
    <path d="m4.5 4.5 7 7M11.5 4.5l-7 7" />
  </Icon>
)

export const InboxIcon = () => (
  <Icon>
    <path d="M2 9.5 4 3h8l2 6.5V13H2z" />
    <path d="M2 9.5h3.5l1 1.5h3l1-1.5H14" />
  </Icon>
)

export const StatusIcon = () => (
  <Icon>
    <path d="M1.5 8h3l2-5 3 10 2-5h3" />
  </Icon>
)

export const AddIcon = () => (
  <Icon>
    <path d="M8 3v10M3 8h10" />
  </Icon>
)

export const ProjectsIcon = () => (
  <Icon>
    <path d="M2 4h4l1.3 1.6H14V12.5H2z" />
  </Icon>
)

export const MoreIcon = () => (
  <Icon>
    <circle cx="3.5" cy="8" r=".6" />
    <circle cx="8" cy="8" r=".6" />
    <circle cx="12.5" cy="8" r=".6" />
  </Icon>
)
