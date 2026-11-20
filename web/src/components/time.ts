export function ago(iso: string): string {
  const m = Math.round((Date.now() - Date.parse(iso)) / 60000)
  if (m < 60) return `${m} min ago`
  const h = Math.floor(m / 60)
  if (h < 48) return `${h} h${m % 60 ? ` ${m % 60} min` : ''} ago`
  return `${Math.floor(h / 24)} days ago`
}

// The local time as hours and minutes, on a 24 hour clock in every locale.
export function clock(iso: string): string {
  return new Date(iso).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', hourCycle: 'h23' })
}

export function duration(ms: number): string {
  const s = Math.round(ms / 1000)
  return s < 60 ? `${s} s` : `${Math.floor(s / 60)} min ${s % 60} s`
}
