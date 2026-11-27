package model

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func tlsServer(t *testing.T, clientAuth tls.ClientAuthType) (*httptest.Server, *x509.CertPool) {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.TLS = &tls.Config{ClientAuth: clientAuth}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return srv, pool
}

func addrOf(srv *httptest.Server) string { return strings.TrimPrefix(srv.URL, "https://") }

// Hermetic-e2e finding: a TLS misconfiguration surfaced only as "context
// deadline exceeded". The handshake check must name the real cause.
func TestTLSHandshakeErrorUntrustedServer(t *testing.T) {
	srv, _ := tlsServer(t, tls.NoClientCert)
	err := tlsHandshakeError(addrOf(srv), &tls.Config{RootCAs: x509.NewCertPool()}, time.Second)
	if err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Errorf("err = %v, want a certificate error", err)
	}
}

func TestTLSHandshakeErrorMissingClientCert(t *testing.T) {
	srv, pool := tlsServer(t, tls.RequireAnyClientCert)
	err := tlsHandshakeError(addrOf(srv), &tls.Config{RootCAs: pool}, time.Second)
	if err == nil || !strings.Contains(err.Error(), "tls") {
		t.Errorf("err = %v, want the server's certificate-required alert", err)
	}
}

func TestTLSHandshakeErrorHealthy(t *testing.T) {
	srv, pool := tlsServer(t, tls.NoClientCert)
	if err := tlsHandshakeError(addrOf(srv), &tls.Config{RootCAs: pool}, time.Second); err != nil {
		t.Errorf("a good handshake reported %v", err)
	}
}

func TestExplainTLSWrapsOnlyWhenTLS(t *testing.T) {
	base := errTest("context deadline exceeded")
	if got := (&v3Backend{}).explainTLS(base); got != base {
		t.Errorf("non-TLS backend changed the error: %v", got)
	}
	srv, _ := tlsServer(t, tls.NoClientCert)
	b := &v3Backend{tlsCfg: &tls.Config{RootCAs: x509.NewCertPool()}, addr: addrOf(srv), timeout: time.Second}
	got := b.explainTLS(base)
	if !strings.Contains(got.Error(), "context deadline exceeded") || !strings.Contains(got.Error(), "TLS:") {
		t.Errorf("explained = %v", got)
	}
	if b.explainTLS(nil) != nil {
		t.Error("nil must stay nil")
	}
}

type errTest string

func (e errTest) Error() string { return string(e) }
