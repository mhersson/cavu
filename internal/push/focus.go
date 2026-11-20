package push

import "sync"

// Focus is which devices a person is looking at, in memory only: a device
// counts while its last word was focused and at least one of its live streams
// is open. Its zero value is ready, and it is safe for concurrent use.
type Focus struct {
	mu      sync.Mutex
	devices map[int64]look
}

type look struct {
	focused bool
	streams int
}

// Set records whether device's page has focus.
func (f *Focus) Set(device int64, focused bool) {
	f.change(device, func(l *look) { l.focused = focused })
}

// Connect counts a live stream of device's open until done is called, once.
func (f *Focus) Connect(device int64) (done func()) {
	f.change(device, func(l *look) { l.streams++ })

	return func() { f.change(device, func(l *look) { l.streams-- }) }
}

// Looking reports whether device is being looked at.
func (f *Focus) Looking(device int64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	l := f.devices[device]

	return l.focused && l.streams > 0
}

func (f *Focus) change(device int64, fn func(*look)) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.devices == nil {
		f.devices = map[int64]look{}
	}

	l := f.devices[device]
	fn(&l)
	f.devices[device] = l
}
