package main

import (
	"crypto/tls"
	"fmt"
	"sync"
	"time"
)

// certReloader serves the certificate in a file pair and picks up a renewed one (cert-manager rewrites
// the files in place) without a restart.
type certReloader struct {
	certFile, keyFile string
	every             time.Duration
	now               func() time.Time

	mu      sync.Mutex
	cert    *tls.Certificate
	checked time.Time
}

func newCertReloader(certFile, keyFile string) (*certReloader, error) {
	r := &certReloader{certFile: certFile, keyFile: keyFile, every: time.Minute, now: time.Now}
	if err := r.load(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *certReloader) load() error {
	c, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		return fmt.Errorf("load edge TLS certificate: %w", err)
	}
	r.cert, r.checked = &c, r.now()
	return nil
}

// GetCertificate implements tls.Config.GetCertificate. A failed reload keeps the certificate in use.
func (r *certReloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.now().Sub(r.checked) >= r.every {
		if err := r.load(); err != nil {
			r.checked = r.now()
		}
	}
	return r.cert, nil
}

func (r *certReloader) tlsConfig() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: r.GetCertificate}
}
