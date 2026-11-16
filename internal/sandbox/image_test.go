package sandbox

import (
	"archive/tar"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	machO = "\xcf\xfa\xed\xfe\x07\x00\x00\x01"
	elf   = "\x7fELF\x02\x01\x01\x00"
)

func testCert(t *testing.T, cn string) []byte {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IsCA:         true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	require.NoError(t, err)

	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func buildOptions(t *testing.T, bin string) BuildOptions {
	t.Helper()

	path := filepath.Join(t.TempDir(), "cavu")
	require.NoError(t, os.WriteFile(path, []byte(bin), 0o600))

	return BuildOptions{
		CavuBinary:   path,
		CavuVersion:  "v1",
		AgentVersion: "1.2.3",
		AgentSHA512:  map[string]string{"amd64": strings.Repeat("a", 128), "arm64": strings.Repeat("b", 128)},
	}
}

type tarEntry struct {
	mode int64
	body string
}

func tarEntries(t *testing.T, b []byte) map[string]tarEntry {
	t.Helper()

	out := map[string]tarEntry{}
	r := tar.NewReader(bytes.NewReader(b))

	for {
		h, err := r.Next()
		if err == io.EOF {
			break
		}

		require.NoError(t, err)

		body, err := io.ReadAll(r)
		require.NoError(t, err)

		out[h.Name] = tarEntry{mode: h.Mode, body: string(body)}
	}

	return out
}

func buildArgs(argv []string) map[string]string {
	out := map[string]string{}

	for _, a := range argv {
		if kv, ok := strings.CutPrefix(a, "--build-arg="); ok {
			k, v, _ := strings.Cut(kv, "=")
			out[k] = v
		}
	}

	return out
}

func TestBuildContext(t *testing.T) {
	ca := append(testCert(t, "one"), testCert(t, "two")...)
	sum := sha256.Sum256(ca)

	for _, tc := range []struct {
		name    string
		ca      []byte
		wantSum string
	}{
		{"with a bundle", ca, hex.EncodeToString(sum[:])},
		{"without", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, f := fakeRuntime(t, "docker")
			o := buildOptions(t, elf+"body")
			o.CABundle = tc.ca

			var out bytes.Buffer

			f.set(t, "stdout", []byte("step 1\n"))
			require.NoError(t, r.BuildImage(t.Context(), o, &out))
			assert.Equal(t, "step 1\n", out.String())

			c := f.only(t)
			assert.Equal(t, "build", c.argv[0])
			assert.Equal(t, "-", c.argv[len(c.argv)-1])
			assert.Contains(t, c.argv, ImageName("v1"))

			args := buildArgs(c.argv)
			assert.Equal(t, "v1", args["CAVU_VERSION"])
			assert.Equal(t, "1.2.3", args["AGENT_VERSION"])
			assert.Equal(t, strings.Repeat("a", 128), args["AGENT_SHA512_AMD64"])
			assert.Equal(t, strings.Repeat("b", 128), args["AGENT_SHA512_ARM64"])
			assert.Equal(t, tc.wantSum, args["CA_BUNDLE_SHA256"])

			entries := tarEntries(t, c.stdin)
			require.Len(t, entries, 3)
			require.Contains(t, entries, "Dockerfile")
			require.Contains(t, entries, "cavu")
			require.Contains(t, entries, "ca-bundle.pem")
			assert.Equal(t, int64(0o755), entries["cavu"].mode)
			assert.Equal(t, elf+"body", entries["cavu"].body)
			assert.Equal(t, string(tc.ca), entries["ca-bundle.pem"].body)
		})
	}
}

func TestBuildContextIsStable(t *testing.T) {
	r, f := fakeRuntime(t, "docker")
	require.NoError(t, r.BuildImage(t.Context(), buildOptions(t, elf), io.Discard))

	// Equal inputs must give equal bytes or the layer cache misses, so nothing
	// in a header may come from the clock or the host.
	tr := tar.NewReader(bytes.NewReader(f.only(t).stdin))

	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}

		require.NoError(t, err)
		assert.True(t, h.ModTime.IsZero() || h.ModTime.Unix() == 0, h.Name)
		assert.Zero(t, h.Uid, h.Name)
		assert.Zero(t, h.Gid, h.Name)
		assert.Empty(t, h.Uname, h.Name)
		assert.Empty(t, h.Gname, h.Name)
		assert.Contains(t, []int64{0o644, 0o755}, h.Mode, h.Name)
	}
}

func TestBuildOutputStreamsBothStreams(t *testing.T) {
	r, f := fakeRuntime(t, "docker")
	f.set(t, "stdout", []byte("from stdout\n"))
	f.set(t, "stderr", []byte("from stderr\n"))

	var out bytes.Buffer

	require.NoError(t, r.BuildImage(t.Context(), buildOptions(t, elf), &out))
	assert.Contains(t, out.String(), "from stdout")
	assert.Contains(t, out.String(), "from stderr")
	assert.Contains(t, f.only(t).argv, "--progress=plain")

	r, f = fakeRuntime(t, "podman")
	f.set(t, "stdout", []byte("--> step\n"))
	f.outFor(t, "false", "info", "--format={{.Host.ServiceIsRemote}}")

	out.Reset()
	require.NoError(t, r.BuildImage(t.Context(), buildOptions(t, elf), &out))
	assert.Contains(t, out.String(), "step")

	for _, c := range f.calls(t) {
		assert.NotContains(t, c.argv, "--progress=plain")
	}
}

func TestBuildRefusesBadCABundle(t *testing.T) {
	good := testCert(t, "ok")

	for name, bundle := range map[string][]byte{
		"no certificate":    []byte("hello\n"),
		"not a certificate": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("k")}),
		"garbled":           pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("junk")}),
		"one bad of two":    append(slicesClone(good), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("junk")})...),
	} {
		t.Run(name, func(t *testing.T) {
			r, f := fakeRuntime(t, "docker")
			o := buildOptions(t, elf)
			o.CABundle = bundle

			require.Error(t, r.BuildImage(t.Context(), o, io.Discard))
			assert.Empty(t, f.calls(t))
		})
	}
}

func slicesClone(b []byte) []byte { return append([]byte(nil), b...) }

func TestBuildRefusesNonLinuxBinary(t *testing.T) {
	for name, body := range map[string]string{"mach-o": machO, "script": "#!/bin/sh\n", "empty": ""} {
		t.Run(name, func(t *testing.T) {
			r, f := fakeRuntime(t, "docker")

			err := r.BuildImage(t.Context(), buildOptions(t, body), io.Discard)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "--cavu-binary")
			assert.Empty(t, f.calls(t))
		})
	}
}

func TestBuildRefusesBadOptions(t *testing.T) {
	for name, mutate := range map[string]func(*BuildOptions){
		"no agent version":     func(o *BuildOptions) { o.AgentVersion = "" },
		"agent version quotes": func(o *BuildOptions) { o.AgentVersion = "1.0;rm" },
		"missing arch":         func(o *BuildOptions) { delete(o.AgentSHA512, "arm64") },
		"short checksum":       func(o *BuildOptions) { o.AgentSHA512["amd64"] = "abc" },
		"no cavu version":      func(o *BuildOptions) { o.CavuVersion = "" },
		"no binary":            func(o *BuildOptions) { o.CavuBinary = filepath.Join(t.TempDir(), "absent") },
	} {
		t.Run(name, func(t *testing.T) {
			r, f := fakeRuntime(t, "docker")
			o := buildOptions(t, elf)
			mutate(&o)

			require.Error(t, r.BuildImage(t.Context(), o, io.Discard))
			assert.Empty(t, f.calls(t))
		})
	}
}

func TestImageNameIsAValidTag(t *testing.T) {
	assert.Equal(t, "cavu-sandbox:dev", ImageName("dev"))
	assert.Equal(t, "cavu-sandbox:1.2.3-rc1-abc", ImageName("1.2.3-rc1+abc"))
	assert.Equal(t, "cavu-sandbox:dev", ImageName(""))
	assert.Regexp(t, `^cavu-sandbox:[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`, ImageName("-.x"+strings.Repeat("y", 200)))
}

func TestBuildFailureCarriesClientError(t *testing.T) {
	r, f := fakeRuntime(t, "docker")
	f.set(t, "exit", []byte("1"))
	f.set(t, "stderr", []byte("RUN failed"))

	err := r.BuildImage(t.Context(), buildOptions(t, elf), io.Discard)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "RUN failed")
}

func TestImageInfo(t *testing.T) {
	name := ImageName("v1")

	t.Run("found", func(t *testing.T) {
		r, f := fakeRuntime(t, "docker")
		f.outFor(t, "sha256:abc", "image", "ls", "--quiet", "--filter=reference="+name)
		f.outFor(t, `{"cavu.version":"v1","cavu.claude_code":"1.2.3","cavu.ca_bundle":""}`,
			"image", "inspect", "--format={{json .Config.Labels}}", name)

		cv, av, found, err := r.ImageInfo(t.Context(), name)
		require.NoError(t, err)
		assert.True(t, found)
		assert.Equal(t, "v1", cv)
		assert.Equal(t, "1.2.3", av)
	})

	t.Run("absent", func(t *testing.T) {
		r, f := fakeRuntime(t, "docker")

		_, _, found, err := r.ImageInfo(t.Context(), name)
		require.NoError(t, err)
		assert.False(t, found)
		assert.Len(t, f.calls(t), 1)
	})

	t.Run("refuses an option as a name", func(t *testing.T) {
		r, f := fakeRuntime(t, "docker")

		_, _, _, err := r.ImageInfo(t.Context(), "--all")
		require.Error(t, err)
		assert.Empty(t, f.calls(t))
	})
}
