package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mhersson/cavu/internal/env"
	"github.com/mhersson/cavu/internal/proc"
	"github.com/mhersson/cavu/internal/verify"
)

const (
	maxFilesSize = 8 << 20
	filesTimeout = time.Minute
	stopGrace    = 2 * time.Second
)

// errFilesTooLarge is a repository whose root files the read takes are too
// large together: a fact about the repository, not a fault of the sandbox.
var errFilesTooLarge = fmt.Errorf("the repository's root version and build files are larger than %d MiB together", maxFilesSize>>20)

var commitID = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

// readScript prints, for each regular file at the root of commit $1 whose
// name matches one of the patterns after it, a "<size> <name>" line and the
// file's bytes. It reads git's objects, so the files are the commit's
// whatever the working tree holds.
const readScript = `set -o pipefail
c=$1
shift
git -C /workspace ls-tree --format='%(objectmode) %(objectsize) %(path)' --end-of-options "$c" |
while IFS= read -r line; do
  mode=${line%% *}
  rest=${line#* }
  size=${rest%% *}
  name=${rest#* }
  case $mode in 100644 | 100755) ;; *) continue ;; esac
  for p in "$@"; do
    case $name in
    $p)
      printf '%s %s\n' "$size" "$name"
      git -C /workspace cat-file blob "$c:$name" || exit 1
      break
      ;;
    esac
  done
done`

// filePatterns are the root files detection and the verify suggestion read;
// some are path.Match patterns.
func filePatterns() []string {
	out := append(env.VersionFiles(), verify.SuggestFiles()...)
	slices.Sort(out)

	return slices.Compact(out)
}

// readFiles reads the root files detection and the verify suggestion need as
// they are at commit, in one command. A file the commit lacks is absent.
func readFiles(ctx context.Context, c container, commit string) (map[string][]byte, error) {
	if !commitID.MatchString(commit) {
		return nil, errors.New("read the root files: the base commit is not a full commit id")
	}

	ctx, cancel := context.WithTimeout(ctx, filesTimeout)
	defer cancel()

	out := &boundedBuffer{limit: maxFilesSize}

	argv := append([]string{"bash", "-c", readScript, "bash", commit}, filePatterns()...)

	p, err := c.Start(ctx, proc.Command{Argv: argv, Stdout: out})
	if err != nil {
		return nil, fmt.Errorf("read the root files: %w", err)
	}

	_ = p.Stdin().Close()

	type exit struct {
		code int
		err  error
	}

	done := make(chan exit, 1)

	go func() {
		code, err := p.Wait()
		done <- exit{code, err}
	}()

	var e exit

	select {
	case e = <-done:
	case <-ctx.Done():
		_ = p.Stop(stopGrace)

		return nil, fmt.Errorf("read the root files: %w", ctx.Err())
	}

	switch {
	case errors.Is(e.err, errFilesTooLarge):
		return nil, errFilesTooLarge
	case e.err != nil:
		return nil, fmt.Errorf("read the root files: %w", e.err)
	case e.code != 0:
		return nil, fmt.Errorf("read the root files: exit %d: %s", e.code, strings.TrimSpace(p.StderrTail()))
	}

	return parseFiles(out.buf.Bytes())
}

// parseFiles reads what readScript prints.
func parseFiles(b []byte) (map[string][]byte, error) {
	files := map[string][]byte{}

	for len(b) > 0 {
		header, rest, ok := bytes.Cut(b, []byte("\n"))
		sizeText, name, named := strings.Cut(string(header), " ")
		size, err := strconv.Atoi(sizeText)

		if !ok || !named || name == "" || err != nil || size < 0 || size > len(rest) {
			return nil, errors.New("read the root files: the listing is malformed")
		}

		files[name] = rest[:size:size]
		b = rest[size:]
	}

	return files, nil
}

// boundedBuffer fails a write that would take it past limit.
type boundedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if b.buf.Len()+len(p) > b.limit {
		return 0, errFilesTooLarge
	}

	return b.buf.Write(p)
}
