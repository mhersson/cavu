import { duration } from './time.ts'

const s = 1000
const min = 60 * s
const h = 60 * min
const day = 24 * h

test.each([
  [0, '0 s'],
  [45 * s, '45 s'],
  [59 * s, '59 s'],
  [60 * s, '1 min 0 s'],
  [12 * min + 5 * s, '12 min 5 s'],
  [59 * min + 59 * s, '59 min 59 s'],
  [h, '1 h'],
  [3 * h + 12 * min, '3 h 12 min'],
  [3 * h + 12 * min + 40 * s, '3 h 12 min'],
  [47 * h + 59 * min, '47 h 59 min'],
  [48 * h, '2 days'],
  [3 * day, '3 days'],
  [3 * day + 4 * h, '3 days 4 h'],
  [3 * day + 4 * h + 59 * min, '3 days 4 h'],
])('duration(%i) is %s', (ms, want) => {
  expect(duration(ms)).toBe(want)
})
