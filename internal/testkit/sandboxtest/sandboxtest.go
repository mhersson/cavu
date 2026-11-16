// Package sandboxtest is what integration tests that need a container runtime
// share: the runtime under test, unique labelled names, the sandbox image built
// once per test binary, the host address a container can reach, and a test CA
// with a TLS server that CA signs for. It names no agent: the caller passes
// the agent version and checksums the image installs.
package sandboxtest

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mhersson/cavu/internal/sandbox"
)

// RunID is unique to this test binary and labels everything it creates.
var RunID = strings.ToLower(rand.Text()[:12])

// Runtime is the runtime named by CAVU_TEST_RUNTIME, docker by default.
func Runtime(t *testing.T) sandbox.Runtime {
	t.Helper()

	name := os.Getenv("CAVU_TEST_RUNTIME")
	if name == "" {
		name = "docker"
	}

	r, err := sandbox.Detect(t.Context(), name)
	require.NoError(t, err)

	return r
}

// UniqueName is a fresh container or volume name for a test.
func UniqueName() string { return "cavu-test-" + strings.ToLower(rand.Text()[:12]) }

// Labels is the label every test container and volume carries.
func Labels() map[string]string { return map[string]string{"cavu.test": RunID} }

type image struct {
	once sync.Once
	rt   sandbox.Runtime
	name string
	err  error
}

var built struct {
	mu     sync.Mutex
	images map[string]*image
	bin    string
	binDir string // the directory os.MkdirTemp returned for bin; the only one Cleanup removes
	binErr error
	binOne sync.Once
}

// Main runs the tests of a package, then removes what Image built. A package's
// TestMain is `func TestMain(m *testing.M) { sandboxtest.Main(m) }`.
func Main(m *testing.M) {
	code := m.Run()

	Cleanup()
	os.Exit(code)
}

// Cleanup removes the images and the binary Image built. It keeps the images'
// untagged parent layers: they are the runtime's layer cache, and Podman
// deletes its cache with them, which would make every later build start cold.
func Cleanup() {
	built.mu.Lock()
	defer built.mu.Unlock()

	for _, img := range built.images {
		if img.name == "" {
			continue
		}

		if out, err := exec.CommandContext(context.Background(), img.rt.Bin, "image", "rm", "--no-prune", img.name).CombinedOutput(); err != nil { //nolint:gosec // our own image, by name
			fmt.Fprintf(os.Stderr, "remove image %s: %v: %s\n", img.name, err, out)
		}
	}

	if built.binDir != "" {
		_ = os.RemoveAll(built.binDir)
	}
}

// CavuBinary is this tree's cavu built once for a Linux container on the
// host's architecture.
func CavuBinary(t *testing.T) string {
	t.Helper()

	built.binOne.Do(func() {
		dir, err := os.MkdirTemp("", "cavu-test-bin-")
		if err != nil {
			built.binErr = err

			return
		}

		built.mu.Lock()
		built.binDir = dir
		built.mu.Unlock()

		root, err := exec.CommandContext(context.Background(), "go", "list", "-m", "-f", "{{.Dir}}").Output()
		if err != nil {
			built.binErr = fmt.Errorf("find the module: %w", err)

			return
		}

		bin := filepath.Join(dir, "cavu")
		cmd := exec.CommandContext(context.Background(), "go", "build", "-trimpath", "-o", bin, "./cmd/cavu")
		cmd.Dir = strings.TrimSpace(string(root))

		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)

		if out, err := cmd.CombinedOutput(); err != nil {
			_ = os.RemoveAll(dir)
			built.binErr = fmt.Errorf("build cavu: %w: %s", err, out)

			return
		}

		built.mu.Lock()
		built.bin = bin
		built.mu.Unlock()
	})

	require.NoError(t, built.binErr)

	return built.bin
}

// Image builds the sandbox image through BuildImage, trusting ca when it is not
// empty, and returns its name. Each distinct bundle is built once per test
// binary under a name unique to it, and Cleanup removes it. A rebuild with the
// same inputs comes from the layer cache.
func Image(t *testing.T, r sandbox.Runtime, agentVersion string, agentSHA512 map[string]string, ca []byte) string {
	t.Helper()

	bin := CavuBinary(t)
	sum := sha256.Sum256(ca)
	key := hex.EncodeToString(sum[:])[:8]

	built.mu.Lock()

	if built.images == nil {
		built.images = map[string]*image{}
	}

	img := built.images[key]
	if img == nil {
		img = &image{rt: r}
		built.images[key] = img
	}

	built.mu.Unlock()

	img.once.Do(func() {
		o := sandbox.BuildOptions{
			CavuBinary:   bin,
			CavuVersion:  "test-" + RunID + "-" + key,
			AgentVersion: agentVersion,
			AgentSHA512:  agentSHA512,
			CABundle:     ca,
		}

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
		defer cancel()

		var log bytes.Buffer

		started := time.Now()

		if img.err = r.BuildImage(ctx, o, &log); img.err != nil {
			img.err = fmt.Errorf("%w\n%s", img.err, log.String())

			return
		}

		img.name = sandbox.ImageName(o.CavuVersion)
		fmt.Fprintf(os.Stderr, "built %s in %s\n", img.name, time.Since(started).Round(time.Millisecond))
	})

	require.NoError(t, img.err)

	return img.name
}

// RunInImage runs argv in a throwaway container of image on the bridge network
// as uid 1000, with host.docker.internal pointing at the host, and returns its
// combined output.
func RunInImage(t *testing.T, r sandbox.Runtime, image string, argv ...string) (string, error) {
	t.Helper()

	name := UniqueName()

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()

		require.NoError(t, r.Remove(ctx, name))
	})

	args := append([]string{
		"run", "--rm", "--pull=never", "--name=" + name, "--label=cavu.test=" + RunID,
		"--network=bridge", "--add-host=host.docker.internal:host-gateway", "--user=1000",
		image,
	}, argv...)
	out, err := exec.CommandContext(t.Context(), r.Bin, args...).CombinedOutput() //nolint:gosec // test containers with a test label

	return string(out), err
}

// HostAddr is the narrowest address on this host that a container started with
// --add-host=host.docker.internal:host-gateway reaches: the bridge gateway or,
// for rootless Podman, whose pasta maps the gateway to the host's primary
// address, that address.
func HostAddr(t *testing.T, r sandbox.Runtime) string {
	t.Helper()

	if r.Kind == "podman" {
		out, err := exec.CommandContext(t.Context(), r.Bin, "info", "--format={{.Host.Security.Rootless}}").Output() //nolint:gosec // our runtime
		require.NoError(t, err)

		if strings.TrimSpace(string(out)) == "true" {
			// A route lookup; UDP sends nothing.
			c, err := (&net.Dialer{}).DialContext(t.Context(), "udp4", "192.0.2.1:9")
			require.NoError(t, err)

			defer c.Close()

			return c.LocalAddr().(*net.UDPAddr).IP.String()
		}
	}

	network := "bridge"
	if r.Kind == "podman" {
		network = "podman"
	}

	out, err := exec.CommandContext(t.Context(), r.Bin, "network", "inspect", network, "--format={{(index .IPAM.Config 0).Gateway}}").Output() //nolint:gosec // our runtime
	if err != nil || strings.TrimSpace(string(out)) == "" {
		out, err = exec.CommandContext(t.Context(), r.Bin, "network", "inspect", network, "--format={{(index .Subnets 0).Gateway}}").Output() //nolint:gosec // our runtime
	}

	require.NoError(t, err)

	return strings.TrimSpace(string(out))
}

// CA is the same test CA on every run, so the image's CA layer and the layers
// after it stay in the layer cache: Ed25519 from a fixed seed, a fixed serial
// and fixed validity. It signs nothing real.
func CA(t *testing.T) (certPEM []byte, key ed25519.PrivateKey, cert *x509.Certificate) {
	t.Helper()

	return CAFromSeed(t, "cavu-test-ca-seed-0123456789abcd")
}

// CAFromSeed is a byte-stable test CA for a 32-byte seed.
func CAFromSeed(t *testing.T, seed string) (certPEM []byte, key ed25519.PrivateKey, cert *x509.Certificate) {
	t.Helper()

	key = ed25519.NewKeyFromSeed([]byte(seed))
	tpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "cavu test CA"},
		NotBefore:             time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		NotAfter:              time.Date(2124, 1, 1, 0, 0, 0, 0, time.UTC),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}

	der, err := x509.CreateCertificate(nil, tpl, tpl, key.Public(), key)
	require.NoError(t, err)

	cert, err = x509.ParseCertificate(der)
	require.NoError(t, err)

	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), key, cert
}

// SignatureLine is a line of the certificate's own signature, which differs
// between certificates where the first lines can match.
func SignatureLine(certPEM []byte) string {
	lines := strings.Split(strings.TrimSpace(string(certPEM)), "\n")

	return lines[len(lines)-2]
}

// StartTLSServer serves h over HTTPS on addr with a random port, with a
// certificate for host.docker.internal signed by CA(t), and returns the port.
// It stops with the test.
func StartTLSServer(t *testing.T, addr string, h http.Handler) (port string) {
	t.Helper()

	_, caKey, ca := CA(t)
	_, srvKey, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)

	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "host.docker.internal"},
		DNSNames:     []string{"host.docker.internal"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(nil, tpl, ca, srvKey.Public(), caKey)
	require.NoError(t, err)

	ln, err := tls.Listen("tcp", net.JoinHostPort(addr, "0"), &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: srvKey}},
		MinVersion:   tls.VersionTLS12,
	})
	require.NoError(t, err)

	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}

	go func() { _ = srv.Serve(ln) }()

	t.Cleanup(func() { _ = srv.Close() })

	_, port, err = net.SplitHostPort(ln.Addr().String())
	require.NoError(t, err)

	return port
}
