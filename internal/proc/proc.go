//go:build unix

// Package proc runs a command as a supervised process: stdout as lines or
// copied whole, a bounded stderr tail, an exit code, and a stop that reaches
// the whole process group.
package proc

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// maxLine caps one stdout line.
const maxLine = 64 << 20

// stderrTailSize caps StderrTail; a tail this long may have lost its start.
const stderrTailSize = 16 << 10

// process is a running child in its own process group.
type process struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	lines   chan []byte
	maxLine int
	tail    *tailBuffer

	abandon     chan struct{}
	abandonOnce sync.Once

	readErrMu sync.Mutex
	readErr   error

	done    chan struct{}
	code    int   // set before done is closed
	waitErr error // set before done is closed
}

// start runs c in its own process group with piped stdin, stdout split into
// lines of at most limit bytes (or copied to c.Stdout), a bounded stderr
// tail, and c.Feed on file descriptor 3. Cancellation is the caller's Stop,
// because a context kill would reach only the group leader.
func start(c Command, limit int) (*process, error) {
	if len(c.Argv) == 0 {
		return nil, errors.New("empty argv")
	}

	cmd := exec.Command(c.Argv[0], c.Argv[1:]...) //nolint:gosec,noctx // caller-chosen argv; Stop signals the whole group
	cmd.Dir = c.Dir
	// Never nil: a nil Env would hand the child this process's environment.
	cmd.Env = append([]string{}, c.Env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// Raw pipes keep cmd.Wait from closing the read ends under the readers.
	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}

	errR, errW, err := os.Pipe()
	if err != nil {
		closeAll(outR, outW)

		return nil, fmt.Errorf("stderr pipe: %w", err)
	}

	toClose := []*os.File{outW, errW}

	var feedW *os.File

	if len(c.Feed) > 0 {
		feedR, w, err := os.Pipe()
		if err != nil {
			closeAll(outR, outW, errR, errW)

			return nil, fmt.Errorf("feed pipe: %w", err)
		}

		cmd.ExtraFiles = []*os.File{feedR}
		feedW = w

		toClose = append(toClose, feedR)
	}

	cmd.Stdout = outW
	cmd.Stderr = errW

	stdin, err := cmd.StdinPipe()
	if err != nil {
		closeAll(append(toClose, outR, errR, feedW)...)

		return nil, fmt.Errorf("stdin pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		closeAll(append(toClose, outR, errR, feedW)...)

		return nil, fmt.Errorf("start %s: %w", c.Argv[0], err)
	}

	closeAll(toClose...)

	if feedW != nil {
		go func() {
			_, _ = feedW.Write(c.Feed)
			_ = feedW.Close()
		}()
	}

	p := &process{
		cmd:     cmd,
		stdin:   stdin,
		lines:   make(chan []byte),
		maxLine: limit,
		tail:    &tailBuffer{},
		abandon: make(chan struct{}),
		done:    make(chan struct{}),
	}

	var readers sync.WaitGroup

	readers.Go(func() {
		if c.Stdout != nil {
			close(p.lines)
			p.copyStdout(outR, c.Stdout)

			return
		}

		p.readStdout(outR)
	})

	readers.Go(func() {
		var w io.Writer = p.tail
		if c.Stderr != nil {
			w = io.MultiWriter(p.tail, &bestEffort{w: c.Stderr})
		}

		_, _ = io.Copy(w, errR)
	})

	go func() {
		exitErr := cmd.Wait()
		if _, ok := errors.AsType[*exec.ExitError](exitErr); ok {
			exitErr = nil
		}

		p.code = -1
		if cmd.ProcessState != nil {
			p.code = exitCode(cmd.ProcessState)
		}

		// Kill what the leader left behind so a grandchild holding a pipe
		// can neither outlive the run nor hang the readers.
		_ = p.signalGroup(syscall.SIGKILL)

		readers.Wait()
		closeAll(outR, errR)

		// Unblocks the feed writer when a descendant that left the group
		// still holds file descriptor 3.
		if feedW != nil {
			_ = feedW.Close()
		}

		p.waitErr = errors.Join(exitErr, p.getReadErr())

		close(p.done)
	}()

	return p, nil
}

// exitCode reports a death by signal as 128 plus the signal, as shells do.
func exitCode(s *os.ProcessState) int {
	if ws, ok := s.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}

	return s.ExitCode()
}

func closeAll(files ...*os.File) {
	for _, f := range files {
		if f != nil {
			_ = f.Close()
		}
	}
}

// Stdin is the child's standard input.
func (p *process) Stdin() io.WriteCloser { return p.stdin }

// Lines yields stdout lines without the trailing newline. It is closed at
// stdout EOF or on a read error, and at once when stdout goes to a writer.
func (p *process) Lines() <-chan []byte { return p.lines }

// StderrTail returns the last 16 KiB of stderr.
func (p *process) StderrTail() string { return p.tail.String() }

// Stop sends SIGTERM to the process group and SIGKILL after grace, to the
// group and, on Linux, to every descendant that left it. It is idempotent and
// returns nil once the process has exited.
func (p *process) Stop(grace time.Duration) error {
	p.abandonLines()

	select {
	case <-p.done:
		return nil
	default:
	}

	if err := p.signalGroup(syscall.SIGTERM); err != nil {
		return fmt.Errorf("terminate process group: %w", err)
	}

	timer := time.NewTimer(grace)
	defer timer.Stop()

	select {
	case <-p.done:
		return nil
	case <-timer.C:
	}

	killDescendants(p.cmd.Process.Pid)

	err := p.signalGroup(syscall.SIGKILL)

	<-p.done

	if err != nil {
		return fmt.Errorf("kill process group: %w", err)
	}

	return nil
}

// Wait returns the exit code and any failure to read stdout; a non-zero exit
// is not an error. Lines not yet delivered are dropped, so it returns even if
// nobody reads Lines.
func (p *process) Wait() (int, error) {
	p.abandonLines()

	<-p.done

	return p.code, p.waitErr
}

func (p *process) abandonLines() {
	p.abandonOnce.Do(func() { close(p.abandon) })
}

// signalGroup treats an already-empty group (ESRCH) as success.
func (p *process) signalGroup(sig syscall.Signal) error {
	err := syscall.Kill(-p.cmd.Process.Pid, sig)
	if err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}

	return nil
}

func (p *process) setReadErr(err error) {
	p.readErrMu.Lock()
	defer p.readErrMu.Unlock()

	p.readErr = err
}

func (p *process) getReadErr() error {
	p.readErrMu.Lock()
	defer p.readErrMu.Unlock()

	return p.readErr
}

// bestEffort stops writing after the first failure and reports success, so a
// broken copy never ends the tail beside it.
type bestEffort struct {
	w      io.Writer
	failed bool
}

func (b *bestEffort) Write(data []byte) (int, error) {
	if !b.failed {
		if _, err := b.w.Write(data); err != nil {
			b.failed = true
		}
	}

	return len(data), nil
}

// copyStdout keeps draining after a failed write so the child never blocks
// on a full pipe.
func (p *process) copyStdout(r io.Reader, w io.Writer) {
	if _, err := io.Copy(w, r); err != nil {
		p.setReadErr(fmt.Errorf("copy stdout: %w", err))

		_, _ = io.Copy(io.Discard, r)
	}
}

func (p *process) readStdout(r io.Reader) {
	defer close(p.lines)

	br := bufio.NewReaderSize(r, 64<<10)

	var acc []byte

	for {
		chunk, err := br.ReadSlice('\n')
		acc = append(acc, chunk...)

		content := len(acc)
		if bytes.HasSuffix(acc, []byte("\n")) {
			content--
		}

		if content > p.maxLine {
			p.setReadErr(fmt.Errorf("stdout line exceeds the %d byte limit", p.maxLine))
			// Killing is the only way to stop a child that keeps writing a
			// line that will never fit.
			_ = p.signalGroup(syscall.SIGKILL)

			return
		}

		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}

		if err == nil || len(acc) > 0 && errors.Is(err, io.EOF) {
			if !p.deliver(bytes.TrimSuffix(acc, []byte("\n"))) {
				// Keep draining so a child blocked on a full pipe can exit.
				_, _ = io.Copy(io.Discard, br)

				return
			}

			acc = nil
		}

		if err != nil {
			if !errors.Is(err, io.EOF) {
				p.setReadErr(fmt.Errorf("read stdout: %w", err))
			}

			return
		}
	}
}

// deliver sends a copy of line, reporting false once Stop or Wait has begun.
func (p *process) deliver(line []byte) bool {
	select {
	case p.lines <- bytes.Clone(line):
		return true
	case <-p.abandon:
		return false
	}
}

type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (b *tailBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.buf = append(b.buf, data...)
	if over := len(b.buf) - stderrTailSize; over > 0 {
		b.buf = append(b.buf[:0], b.buf[over:]...)
	}

	return len(data), nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return string(b.buf)
}
