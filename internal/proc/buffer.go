package proc

// Buffer keeps the first limit bytes written to it and drops the rest. A
// write never fails, so a command writing to it runs to its end.
type Buffer struct {
	limit   int
	kept    []byte
	dropped bool
}

// NewBuffer returns a Buffer that keeps at most limit bytes.
func NewBuffer(limit int) *Buffer { return &Buffer{limit: limit} }

func (b *Buffer) Write(p []byte) (int, error) {
	n := len(p)
	if room := b.limit - len(b.kept); n > room {
		p, b.dropped = p[:room], true
	}

	b.kept = append(b.kept, p...)

	return n, nil
}

// Bytes returns what the buffer kept.
func (b *Buffer) Bytes() []byte { return b.kept }

// Dropped reports whether a write went past the limit.
func (b *Buffer) Dropped() bool { return b.dropped }
