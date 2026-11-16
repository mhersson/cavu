package sandbox

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	_ "embed" // the Dockerfile
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
)

const imageRepo = "cavu-sandbox"

//go:embed Dockerfile
var dockerfile []byte

var (
	versionText = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`)
	sha512Hex   = regexp.MustCompile(`^[0-9a-f]{128}$`)
	badTagChars = regexp.MustCompile(`[^A-Za-z0-9_.-]`)
)

// ImageName is the sandbox image for one cavu version. Characters a tag cannot
// hold become -.
func ImageName(cavuVersion string) string {
	tag := badTagChars.ReplaceAllString(cavuVersion, "-")
	tag = strings.TrimLeft(tag, ".-")

	if tag == "" {
		tag = "dev"
	}

	if len(tag) > 128 {
		tag = tag[:128]
	}

	return imageRepo + ":" + tag
}

// BuildOptions are the only inputs of the image: the Dockerfile sets no
// default for the agent, so a build cannot quietly pick another release.
type BuildOptions struct {
	CavuBinary   string // a Linux executable for the image's architecture
	CavuVersion  string
	AgentVersion string
	AgentSHA512  map[string]string // hex sha512 of the agent's release by GOARCH: amd64 and arm64
	CABundle     []byte            // PEM certificates for a network that inspects TLS; empty for none
}

// BuildImage builds ImageName(o.CavuVersion) from the embedded Dockerfile.
// The context is a tar stream on stdin, so no host path is involved. The
// client's stdout goes to out; a failure carries the tail of its stderr.
func (r Runtime) BuildImage(ctx context.Context, o BuildOptions, out io.Writer) error {
	bundleSum, err := checkBundle(o.CABundle)
	if err != nil {
		return err
	}

	if err := checkBuildOptions(o); err != nil {
		return err
	}

	bin, err := readLinuxBinary(o.CavuBinary)
	if err != nil {
		return err
	}

	ctxTar, err := buildContext(bin, o.CABundle)
	if err != nil {
		return err
	}

	args := []string{
		"build", "-t", ImageName(o.CavuVersion),
		"--build-arg=CAVU_VERSION=" + o.CavuVersion,
		"--build-arg=AGENT_VERSION=" + o.AgentVersion,
		"--build-arg=AGENT_SHA512_AMD64=" + o.AgentSHA512["amd64"],
		"--build-arg=AGENT_SHA512_ARM64=" + o.AgentSHA512["arm64"],
		"--build-arg=CA_BUNDLE_SHA256=" + bundleSum,
		"-",
	}

	// Docker prints its steps on stderr and Podman on stdout; both go to out.
	w := &lockedWriter{w: out}

	if r.Kind == "docker" {
		// Plain lines instead of a redrawn display.
		args = slices.Insert(args, 1, "--progress=plain")
	}

	if err := r.run(ctx, call{args: args, stdin: ctxTar, stdout: w, stderr: w}); err != nil {
		return fmt.Errorf("build %s: %w", ImageName(o.CavuVersion), err)
	}

	return nil
}

// checkBundle returns the bundle's sha256, or "" for no bundle. Every PEM
// block must be a certificate that parses, so a mistaken file fails here and
// not as a trust failure inside the image.
func checkBundle(b []byte) (string, error) {
	if len(bytes.TrimSpace(b)) == 0 {
		return "", nil
	}

	n := 0
	rest := b

	for {
		var blk *pem.Block

		blk, rest = pem.Decode(rest)
		if blk == nil {
			break
		}

		if blk.Type != "CERTIFICATE" {
			return "", fmt.Errorf("CA bundle: a %s block, want only CERTIFICATE", blk.Type)
		}

		if _, err := x509.ParseCertificate(blk.Bytes); err != nil {
			return "", fmt.Errorf("CA bundle: certificate %d: %w", n+1, err)
		}

		n++
	}

	if n == 0 {
		return "", errors.New("CA bundle: no PEM certificate found")
	}

	sum := sha256.Sum256(b)

	return hex.EncodeToString(sum[:]), nil
}

func checkBuildOptions(o BuildOptions) error {
	if !versionText.MatchString(o.CavuVersion) {
		return fmt.Errorf("cavu version %q: want letters, digits and . _ + -", o.CavuVersion)
	}

	if !versionText.MatchString(o.AgentVersion) {
		return fmt.Errorf("agent version %q: want letters, digits and . _ + -", o.AgentVersion)
	}

	for _, arch := range []string{"amd64", "arm64"} {
		if !sha512Hex.MatchString(o.AgentSHA512[arch]) {
			return fmt.Errorf("agent checksum for %s: want 128 lower-case hex characters", arch)
		}
	}

	return nil
}

// readLinuxBinary refuses what could not run in the image, which is what a
// binary built on a Mac is.
func readLinuxBinary(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cavu binary: %w", err)
	}

	if !bytes.HasPrefix(b, []byte("\x7fELF")) {
		return nil, fmt.Errorf("%s is not a Linux executable: build one with GOOS=linux and pass it with --cavu-binary", path)
	}

	return b, nil
}

// buildContext fixes names, order, modes and times, so equal inputs give equal
// bytes and the runtime's layer cache hits.
func buildContext(bin, bundle []byte) ([]byte, error) {
	var buf bytes.Buffer

	w := tar.NewWriter(&buf)

	for _, f := range []struct {
		name string
		mode int64
		body []byte
	}{
		{"Dockerfile", 0o644, dockerfile},
		{"cavu", 0o755, bin},
		{"ca-bundle.pem", 0o644, bundle},
	} {
		if err := w.WriteHeader(&tar.Header{Name: f.name, Mode: f.mode, Size: int64(len(f.body)), Typeflag: tar.TypeReg}); err != nil {
			return nil, err
		}

		if _, err := w.Write(f.body); err != nil {
			return nil, err
		}
	}

	if err := w.Close(); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// ImageInfo reads the labels of a local image; an absent image is not an error.
func (r Runtime) ImageInfo(ctx context.Context, name string) (cavuVersion, agentVersion string, found bool, err error) {
	if err := checkArg("image", name); err != nil {
		return "", "", false, err
	}

	ids, err := r.output(ctx, "image", "ls", "--quiet", "--filter=reference="+name)
	if err != nil {
		return "", "", false, fmt.Errorf("list image %s: %w", name, err)
	}

	if len(bytes.TrimSpace(ids)) == 0 {
		return "", "", false, nil
	}

	raw, err := r.output(ctx, "image", "inspect", "--format={{json .Config.Labels}}", name)
	if err != nil {
		return "", "", false, fmt.Errorf("inspect image %s: %w", name, err)
	}

	var labels map[string]string
	if err := json.Unmarshal(bytes.TrimSpace(raw), &labels); err != nil {
		return "", "", false, fmt.Errorf("inspect image %s: %w", name, err)
	}

	return labels["cavu.version"], labels["cavu.claude_code"], true, nil
}

// lockedWriter lets stdout and stderr of one client share a writer.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.w.Write(b)
}
