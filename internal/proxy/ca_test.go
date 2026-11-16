package proxy

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTLSUpstream serves a certificate for 127.0.0.1 signed by a throwaway CA and returns the CA as PEM.
func newTLSUpstream(t *testing.T, h http.Handler) (*httptest.Server, []byte) {
	t.Helper()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	require.NoError(t, err)

	ca, err := x509.ParseCertificate(caDER)
	require.NoError(t, err)

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "leaf"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, ca, &leafKey.PublicKey, caKey)
	require.NoError(t, err)

	srv := httptest.NewUnstartedServer(h)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}}}
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	t.Cleanup(srv.Close)

	return srv, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
}

func TestCABundle(t *testing.T) {
	up, caPEM := newTLSUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "{}")
	}))
	_, otherPEM := newTLSUpstream(t, http.NotFoundHandler())

	for _, tc := range []struct {
		name   string
		bundle []byte
		status int
	}{
		{"bundle holds the CA", caPEM, http.StatusOK},
		{"no bundle", nil, http.StatusBadGateway},
		{"bundle holds another CA", otherPEM, http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := startProxy(t, Config{Model: modelRoute(up.URL, authBearer), CABundle: tc.bundle})

			resp := post(t, p.URL, "/v1/messages", authBearer, strings.NewReader("{}"))
			assert.Equal(t, tc.status, resp.StatusCode)
		})
	}
}

func TestCredentialNeverInErrors(t *testing.T) {
	var logs bytes.Buffer

	prev := log.Writer()

	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(prev) })

	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()

	cut := serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: x\n")
		w.(http.Flusher).Flush()

		panic(http.ErrAbortHandler)
	}))

	tlsUp, _ := newTLSUpstream(t, http.NotFoundHandler())

	var sb strings.Builder

	for _, base := range []string{deadURL, cut.URL, tlsUp.URL} {
		p := startProxy(t, Config{Model: modelRoute(base, authBearer)})

		resp := post(t, p.URL, "/v1/messages", authBearer, strings.NewReader("{}"))
		b, _ := io.ReadAll(resp.Body)

		sb.Write(b)
		sb.WriteString(resp.Status)

		for k, v := range resp.Header {
			sb.WriteString(k + strings.Join(v, ","))
		}
	}

	sb.WriteString(logs.String())

	assert.NotContains(t, sb.String(), realKey)
	assert.NotContains(t, sb.String(), standIn)
	// The reverse proxy would log a cut stream to the default logger if its ErrorLog were not discarded.
	assert.Empty(t, logs.String())
}
