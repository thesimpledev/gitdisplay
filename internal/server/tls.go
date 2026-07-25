package server

import (
	"crypto/tls"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"
)

// CertReloader holds the current certificate and picks up a new one after
// certbot renews.
//
// certbot rewrites the PEM files in place roughly every 60 days. A process
// that read them once at start would keep serving the old certificate until
// it was restarted, and would eventually serve an expired one. This checks
// the files' modification times and reloads when they change, so renewal
// needs no hook, no restart and no downtime.
type CertReloader struct {
	certFile string
	keyFile  string

	// checkEvery bounds how often the files are stat'ed. A TLS handshake
	// must not turn into a syscall storm under load.
	checkEvery time.Duration

	mu        sync.RWMutex
	cert      *tls.Certificate
	certMod   time.Time
	keyMod    time.Time
	lastCheck time.Time
}

// NewCertReloader loads the pair once so a bad path fails at startup rather
// than on the first request.
func NewCertReloader(certFile, keyFile string) (*CertReloader, error) {
	r := &CertReloader{certFile: certFile, keyFile: keyFile, checkEvery: time.Minute}
	if err := r.reload(); err != nil {
		return nil, err
	}
	return r, nil
}

// GetCertificate satisfies tls.Config.GetCertificate.
func (r *CertReloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.RLock()
	cert, last := r.cert, r.lastCheck
	r.mu.RUnlock()

	if time.Since(last) >= r.checkEvery {
		if changed, err := r.reloadIfChanged(); err != nil {
			// Keep serving the certificate already loaded. A failed reload
			// must not take the site down; it is logged and retried.
			log.Printf("warning: reloading TLS certificate: %v", err)
		} else if changed {
			r.mu.RLock()
			cert = r.cert
			r.mu.RUnlock()
			log.Printf("reloaded TLS certificate from %s", r.certFile)
		}
	}

	if cert == nil {
		return nil, fmt.Errorf("no TLS certificate loaded")
	}
	return cert, nil
}

// Reload forces a reload, used by the SIGHUP handler.
func (r *CertReloader) Reload() error { return r.reload() }

func (r *CertReloader) reloadIfChanged() (bool, error) {
	certInfo, err := os.Stat(r.certFile)
	if err != nil {
		r.touch()
		return false, err
	}
	keyInfo, err := os.Stat(r.keyFile)
	if err != nil {
		r.touch()
		return false, err
	}

	r.mu.RLock()
	same := certInfo.ModTime().Equal(r.certMod) && keyInfo.ModTime().Equal(r.keyMod)
	r.mu.RUnlock()
	if same {
		r.touch()
		return false, nil
	}
	if err := r.reload(); err != nil {
		return false, err
	}
	return true, nil
}

func (r *CertReloader) touch() {
	r.mu.Lock()
	r.lastCheck = time.Now()
	r.mu.Unlock()
}

func (r *CertReloader) reload() error {
	cert, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		return fmt.Errorf("load key pair %s / %s: %w", r.certFile, r.keyFile, err)
	}
	certInfo, err := os.Stat(r.certFile)
	if err != nil {
		return err
	}
	keyInfo, err := os.Stat(r.keyFile)
	if err != nil {
		return err
	}

	r.mu.Lock()
	r.cert = &cert
	r.certMod = certInfo.ModTime()
	r.keyMod = keyInfo.ModTime()
	r.lastCheck = time.Now()
	r.mu.Unlock()
	return nil
}

// TLSConfig returns a server TLS configuration using the reloader.
//
// TLS 1.2 is the floor. The cipher list applies to 1.2 only; 1.3 picks its
// own suites and ignores this field.
func TLSConfig(r *CertReloader) *tls.Config {
	return &tls.Config{
		GetCertificate: r.GetCertificate,
		MinVersion:     tls.VersionTLS12,
		CipherSuites: []uint16{
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,
			tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,
		},
		CurvePreferences: []tls.CurveID{tls.X25519, tls.CurveP256},
		NextProtos:       []string{"h2", "http/1.1"},
	}
}

// RedirectHandler sends everything to HTTPS, except the ACME challenge path.
//
// certbot --webroot writes its challenge token under webroot; serving it here
// means certbot never needs port 80 for itself, so renewal does not require
// stopping this service.
func RedirectHandler(publicHost, webroot string) http.Handler {
	mux := http.NewServeMux()

	if webroot != "" {
		const prefix = "/.well-known/acme-challenge/"
		fs := http.FileServer(http.Dir(webroot + prefix))
		mux.Handle(prefix, http.StripPrefix(prefix, fs))
	}

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		target := "https://" + publicHost + r.URL.RequestURI()
		http.Redirect(w, r, target, http.StatusMovedPermanently)
	})
	return mux
}
