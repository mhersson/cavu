package proxy

import (
	"crypto/x509"
	"errors"
)

// rootsWith returns the system roots plus the PEM bundle, or nil (system roots
// only) when no bundle is given. A bundle without a certificate is an error.
func rootsWith(bundle []byte) (*x509.CertPool, error) {
	if len(bundle) == 0 {
		return nil, nil
	}

	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}

	if !pool.AppendCertsFromPEM(bundle) {
		return nil, errors.New("proxy: CA bundle holds no PEM certificate")
	}

	return pool, nil
}
