package forge

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCustomCA(t *testing.T) {
	caPEM, leaf := newTestCA(t)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v3/repos/o/n", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})

	srv := httptest.NewUnstartedServer(mux)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{leaf}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	t.Cleanup(srv.Close)

	r, err := ParseRepo(srv.URL + "/o/n")
	require.NoError(t, err)

	check := func(t *testing.T, o Options) error {
		t.Helper()

		c, err := New(o)
		require.NoError(t, err)
		t.Cleanup(c.http.CloseIdleConnections)

		return c.Check(t.Context(), r, Access{Token: testToken})
	}

	t.Run("refused without the bundle", func(t *testing.T) {
		err := check(t, Options{})

		var unknown *tls.CertificateVerificationError
		require.ErrorAs(t, err, &unknown)
		assert.Contains(t, err.Error(), srv.URL)
	})

	t.Run("read with the bundle", func(t *testing.T) {
		require.NoError(t, check(t, Options{CABundle: caPEM}))
	})

	t.Run("a bundle with no certificate is an error", func(t *testing.T) {
		_, err := New(Options{CABundle: []byte("not a certificate")})
		require.Error(t, err)
	})
}

// newTestCA returns a CA's certificate as PEM and a server certificate it
// signed for 127.0.0.1.
func newTestCA(t *testing.T) ([]byte, tls.Certificate) {
	t.Helper()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	now := time.Now()
	ca := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "cavu test CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	require.NoError(t, err)

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, ca, &leafKey.PublicKey, caKey)
	require.NoError(t, err)

	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		tls.Certificate{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}
}

func TestTokenStaysOnItsHost(t *testing.T) {
	const logPath = "/api/v3/repos/o/n/actions/jobs/1/logs"

	var movedAuth, storageAuth []string

	api := http.NewServeMux()
	// A renamed repository answers on its old path with a redirect on the same host.
	api.HandleFunc("GET "+logPath, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://ghe.example.com/api/v3/repositories/7/actions/jobs/1/logs", http.StatusMovedPermanently)
	})
	api.HandleFunc("GET /api/v3/repositories/7/actions/jobs/1/logs", func(w http.ResponseWriter, r *http.Request) {
		movedAuth = append(movedAuth, r.Header.Get("Authorization"))
		http.Redirect(w, r, "https://storage.ghe.example.com/log?sig=signed", http.StatusFound)
	})

	storage := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		storageAuth = append(storageAuth, r.Header.Get("Authorization"))
		_, _ = w.Write([]byte("the log"))
	})

	c := newTestClient(t, nil, "ghe.example.com", api, map[string]http.Handler{"storage.ghe.example.com": storage})
	r := Repo{Host: "ghe.example.com", Owner: "o", Name: "n"}

	resp, err := c.rest(t.Context(), r, testToken, http.MethodGet, r.path()+"/actions/jobs/1/logs", nil)
	require.NoError(t, err)
	assert.Equal(t, "the log", string(resp.body))
	assert.Equal(t, []string{"Bearer " + testToken}, movedAuth)
	assert.Equal(t, []string{""}, storageAuth)
}

func TestErrorKeepsARedirectsAddressOut(t *testing.T) {
	const signed = "sig=signed-credential"

	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://storage.example.com/log?"+signed, http.StatusFound)
	})
	broken := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if assert.NoError(t, err) {
			_ = conn.Close()
		}
	})

	c := newTestClient(t, nil, "api.github.com", api, map[string]http.Handler{"storage.example.com": broken})

	_, err := c.rest(t.Context(), testRepo, testToken, http.MethodGet, testRepo.path()+"/actions/jobs/1/logs", nil)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), signed)
}
