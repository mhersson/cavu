package proc

import (
	"fmt"
	"math"
	"sync"
	"unicode/utf8"
)

// CaptureCap is the output cap for a command run to its end.
const CaptureCap = 64 << 10

const markerFormat = "\n[%d bytes dropped]\n"

// markerMax is the longest marker, so the head and tail sizes do not depend
// on the input's length and the streaming form can fix them up front.
var markerMax = len(fmt.Sprintf(markerFormat, math.MaxInt))

// HeadTail caps s at limit bytes, keeping the start and the end with a marker
// line between them that states how many bytes were dropped. Capture is the
// streaming form: an io.Writer that never holds more than limit bytes.
func HeadTail(s string, limit int) string {
	if len(s) <= limit {
		return s
	}

	headN, tailN, marked := split(limit)
	head := s[:headCut(s, headN)]

	if !marked {
		return head
	}

	return join(head, s[len(s)-tailN:], len(s))
}

// split gives the head and tail sizes for a capped result. A limit with no
// room for the marker keeps a plain prefix.
func split(limit int) (head, tail int, marked bool) {
	budget := limit - markerMax
	if budget <= 0 {
		return max(limit, 0), 0, false
	}

	return budget / 2, budget - budget/2, true
}

// headCut backs n off to a rune start so the head never splits a character.
func headCut[T ~string | ~[]byte](s T, n int) int {
	for i := 0; i < utf8.UTFMax-1 && n > 0 && n < len(s) && !utf8.RuneStart(s[n]); i++ {
		n--
	}

	return n
}

// join drops the tail's leading continuation bytes and puts the marker,
// counting every byte not kept, between head and tail.
func join(head, tail string, total int) string {
	for i := 0; i < utf8.UTFMax-1 && tail != "" && !utf8.RuneStart(tail[0]); i++ {
		tail = tail[1:]
	}

	return head + fmt.Sprintf(markerFormat, total-len(head)-len(tail)) + tail
}

// Capture is the streaming form of HeadTail. String returns what HeadTail
// would return for everything written; Write never fails.
type Capture struct {
	mu    sync.Mutex
	limit int
	total int
	over  bool
	head  []byte // everything until the limit is passed, then the kept head
	tail  []byte // the last tail-size bytes once the limit is passed
}

// NewCapture returns a Capture that keeps at most limit bytes.
func NewCapture(limit int) *Capture {
	limit = max(limit, 0)

	return &Capture{limit: limit, head: make([]byte, 0, limit)}
}

func (c *Capture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.total += len(p)

	switch {
	case c.over:
		c.keepTail(p)
	case len(c.head)+len(p) <= c.limit:
		c.head = append(c.head, p...)
	default:
		c.overflow(p)
	}

	return len(p), nil
}

// overflow fixes the head, which needs one byte past the head size to find a
// rune start, and starts the tail from what is held plus p.
func (c *Capture) overflow(p []byte) {
	headN, tailN, _ := split(c.limit)

	head := make([]byte, 0, headN+1)
	head = append(head, c.head[:min(len(c.head), headN+1)]...)
	head = append(head, p[:headN+1-len(head)]...)

	c.tail = make([]byte, 0, tailN)
	c.keepTail(c.head)
	c.keepTail(p)

	cut := headCut(head, headN)
	c.head = append(make([]byte, 0, cut), head[:cut]...)
	c.over = true
}

// keepTail never grows the tail past its capacity.
func (c *Capture) keepTail(p []byte) {
	if n := cap(c.tail); len(p) >= n {
		c.tail = append(c.tail[:0], p[len(p)-n:]...)

		return
	}

	if drop := len(c.tail) + len(p) - cap(c.tail); drop > 0 {
		c.tail = c.tail[:copy(c.tail, c.tail[drop:])]
	}

	c.tail = append(c.tail, p...)
}

func (c *Capture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.over {
		return string(c.head)
	}

	if _, _, marked := split(c.limit); !marked {
		return string(c.head)
	}

	return join(string(c.head), string(c.tail), c.total)
}
