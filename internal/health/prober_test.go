package health

import (
	"context"
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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestTCPProberSuccess(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	p := NewTCPProber()
	if err := p.Probe(testCtx(t), ln.Addr().String()); err != nil {
		t.Errorf("Probe against live listener failed: %v", err)
	}
}

func TestTCPProberConnectionRefused(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close() // free the port so the dial is refused

	p := NewTCPProber()
	if err := p.Probe(testCtx(t), addr); err == nil {
		t.Error("Probe against closed port succeeded, want error")
	}
}

func readyzHandler(status int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/readyz" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
	})
}

func TestReadyzProberInsecure(t *testing.T) {
	srv := httptest.NewTLSServer(readyzHandler(http.StatusOK))
	defer srv.Close()

	p, err := NewReadyzProber("")
	if err != nil {
		t.Fatal(err)
	}
	addr := strings.TrimPrefix(srv.URL, "https://")
	if err := p.Probe(testCtx(t), addr); err != nil {
		t.Errorf("Probe failed against 200 server: %v", err)
	}
}

func TestReadyzProberNon200(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusInternalServerError} {
		srv := httptest.NewTLSServer(readyzHandler(status))
		p, err := NewReadyzProber("")
		if err != nil {
			t.Fatal(err)
		}
		addr := strings.TrimPrefix(srv.URL, "https://")
		err = p.Probe(testCtx(t), addr)
		if err == nil {
			t.Errorf("Probe succeeded against %d server, want error", status)
		} else if status == http.StatusUnauthorized && !strings.Contains(err.Error(), "401") {
			t.Errorf("401 error %q does not include the status code", err)
		}
		srv.Close()
	}
}

func TestReadyzProberConnectionRefused(t *testing.T) {
	srv := httptest.NewTLSServer(readyzHandler(http.StatusOK))
	addr := strings.TrimPrefix(srv.URL, "https://")
	srv.Close()

	p, err := NewReadyzProber("")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Probe(testCtx(t), addr); err == nil {
		t.Error("Probe against closed server succeeded, want error")
	}
}

// makeCertPair generates a CA and a server certificate signed by it. The
// server certificate's SANs deliberately do NOT include 127.0.0.1, which
// mirrors real kubeadm apiserver certificates that lack per-node IPs.
func makeCertPair(t *testing.T) (caPEM []byte, serverCert tls.Certificate) {
	t.Helper()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-kubernetes-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}

	srvKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	srvTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "kube-apiserver"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"kubernetes", "kubernetes.default"},
	}
	srvDER, err := x509.CreateCertificate(rand.Reader, srvTmpl, caCert, &srvKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}

	caPEM = pemEncodeCert(t, caDER)
	serverCert = tls.Certificate{
		Certificate: [][]byte{srvDER},
		PrivateKey:  srvKey,
	}
	return caPEM, serverCert
}

func pemEncodeCert(t *testing.T, der []byte) []byte {
	t.Helper()
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func startTLSServerWithCert(t *testing.T, cert tls.Certificate) string {
	t.Helper()
	srv := httptest.NewUnstartedServer(readyzHandler(http.StatusOK))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "https://")
}

func writeCAFile(t *testing.T, pem []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(path, pem, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadyzProberCAVerifiesChainButNotHostname(t *testing.T) {
	// The server cert has no IP SANs at all; we dial by 127.0.0.1. With
	// chain verification against the right CA the probe must still
	// succeed, because hostname verification is intentionally skipped.
	caPEM, serverCert := makeCertPair(t)
	addr := startTLSServerWithCert(t, serverCert)

	p, err := NewReadyzProber(writeCAFile(t, caPEM))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Probe(testCtx(t), addr); err != nil {
		t.Errorf("Probe with correct CA failed: %v", err)
	}
}

func TestReadyzProberCARejectsWrongChain(t *testing.T) {
	// Server presents a cert from a different CA: chain verification
	// must fail even though we skip hostname verification.
	_, serverCert := makeCertPair(t)
	otherCAPEM, _ := makeCertPair(t)
	addr := startTLSServerWithCert(t, serverCert)

	p, err := NewReadyzProber(writeCAFile(t, otherCAPEM))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Probe(testCtx(t), addr); err == nil {
		t.Error("Probe with wrong CA succeeded, want chain verification failure")
	}
}

func TestNewReadyzProberBadCAFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(path, []byte("not a certificate"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewReadyzProber(path); err == nil {
		t.Error("garbage CA file accepted, want error")
	}
}

func TestNewProberSelectsMode(t *testing.T) {
	p, err := NewProber("tcp", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.(*TCPProber); !ok {
		t.Errorf("NewProber(tcp) = %T, want *TCPProber", p)
	}
	p, err = NewProber("readyz", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.(*ReadyzProber); !ok {
		t.Errorf("NewProber(readyz) = %T, want *ReadyzProber", p)
	}
	if _, err := NewProber("icmp", ""); err == nil {
		t.Error("unknown mode accepted, want error")
	}
}
