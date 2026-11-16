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

	out := proc.NewBuffer(maxFilesSize)

	argv := append([]string{"bash", "-c", readScript, "bash", commit}, filePatterns()...)

	if err := proc.Run(ctx, c, proc.Command{Argv: argv, Stdout: out}, nil); err != nil {
		return nil, fmt.Errorf("read the root files: %w", err)
	}

	if out.Dropped() {
		return nil, errFilesTooLarge
	}

	return parseFiles(out.Bytes())
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
