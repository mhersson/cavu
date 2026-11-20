export function ago(iso: string): string {
  const m = Math.round((Date.now() - Date.parse(iso)) / 60000)
  if (m < 60) return `${m} min ago`
  const h = Math.floor(m / 60)
  if (h < 48) return `${h} h${m % 60 ? ` ${m % 60} min` : ''} ago`
  return `${Math.floor(h / 24)} days ago`
}

// The local time as hours and minutes, on a 24 hour clock in every locale, after the day when it is not today.
export function clock(iso: string): string {
  const d = new Date(iso)
  const time = d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', hourCycle: 'h23' })
  if (d.toDateString() === new Date().toDateString()) return time
  return d.toLocaleDateString([], { weekday: 'short', day: 'numeric', month: 'short' }) + ' ' + time
}

// In the steps ago uses: a task that waited in review runs for hours or days.
export function duration(ms: number): string {
  const s = Math.round(ms / 1000)
  if (s < 60) return `${s} s`
  const m = Math.floor(s / 60)
  if (m < 60) return `${m} min ${s % 60} s`
  const h = Math.floor(m / 60)
  if (h < 48) return m % 60 ? `${h} h ${m % 60} min` : `${h} h`
  const d = Math.floor(h / 24)
  return h % 24 ? `${d} days ${h % 24} h` : `${d} days`
}
