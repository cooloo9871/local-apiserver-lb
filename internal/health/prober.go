// Package health probes backend apiservers and drives the backend
// fall/rise state machine.
package health

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
)

// Prober checks whether a single backend is healthy. Implementations must
// be safe for concurrent use and must honor ctx cancellation/deadlines.
type Prober interface {
	Probe(ctx context.Context, addr string) error
}

// NewProber builds a prober for the configured health check mode
// ("readyz" or "tcp"). caFile is only used by readyz mode.
func NewProber(mode, caFile string) (Prober, error) {
	switch mode {
	case "tcp":
		return NewTCPProber(), nil
	case "readyz":
		return NewReadyzProber(caFile)
	default:
		return nil, fmt.Errorf("unknown health check mode %q (want \"readyz\" or \"tcp\")", mode)
	}
}

// TCPProber considers a backend healthy if a TCP connection can be
// established. It is the fallback for clusters with anonymous auth
// disabled, where /readyz returns 401.
type TCPProber struct {
	dialer net.Dialer
}

// NewTCPProber returns a plain TCP dial prober.
func NewTCPProber() *TCPProber {
	return &TCPProber{}
}

// Probe dials the backend and immediately closes the connection.
func (p *TCPProber) Probe(ctx context.Context, addr string) error {
	conn, err := p.dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	return conn.Close()
}

// ReadyzProber considers a backend healthy if GET /readyz over HTTPS
// returns 200. Keep-alives are disabled so probe connections never linger
// and are not mistaken for real load on the backend.
type ReadyzProber struct {
	client *http.Client
}

// NewReadyzProber builds a readyz prober. With an empty caFile the TLS
// handshake is not verified at all (the probe only proves the apiserver
// is alive). With a caFile, the presented chain is verified against that
// CA — but the hostname is NOT verified, because backends are dialed by
// IP and kubeadm apiserver certificates do not necessarily include every
// control plane node IP in their SANs.
func NewReadyzProber(caFile string) (*ReadyzProber, error) {
	tlsCfg := &tls.Config{
		// Disable the standard verification unconditionally; when a CA
		// is configured, VerifyPeerCertificate below re-implements chain
		// verification without the hostname check.
		InsecureSkipVerify: true,
	}

	if caFile != "" {
		roots, err := loadCAPool(caFile)
		if err != nil {
			return nil, err
		}
		tlsCfg.VerifyPeerCertificate = func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			return verifyChainWithoutHostname(rawCerts, roots)
		}
	}

	return &ReadyzProber{
		client: &http.Client{
			Transport: &http.Transport{
				TLSClientConfig:   tlsCfg,
				DisableKeepAlives: true,
			},
		},
	}, nil
}

// Probe performs one GET /readyz request against the backend.
func (p *ReadyzProber) Probe(ctx context.Context, addr string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+addr+"/readyz", nil)
	if err != nil {
		return err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// Drain a bounded amount so the connection can terminate cleanly.
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET /readyz returned status %d", resp.StatusCode)
	}
	return nil
}

// loadCAPool reads a PEM CA bundle into a cert pool.
func loadCAPool(path string) (*x509.CertPool, error) {
	pemData, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading CA file: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemData) {
		return nil, fmt.Errorf("CA file %s contains no valid PEM certificates", path)
	}
	return pool, nil
}

// verifyChainWithoutHostname verifies the peer's certificate chain
// against roots, deliberately skipping hostname verification (no DNSName
// in VerifyOptions).
func verifyChainWithoutHostname(rawCerts [][]byte, roots *x509.CertPool) error {
	if len(rawCerts) == 0 {
		return fmt.Errorf("server presented no certificates")
	}
	certs := make([]*x509.Certificate, 0, len(rawCerts))
	for _, raw := range rawCerts {
		cert, err := x509.ParseCertificate(raw)
		if err != nil {
			return fmt.Errorf("parsing server certificate: %w", err)
		}
		certs = append(certs, cert)
	}

	intermediates := x509.NewCertPool()
	for _, cert := range certs[1:] {
		intermediates.AddCert(cert)
	}

	_, err := certs[0].Verify(x509.VerifyOptions{
		Roots:         roots,
		Intermediates: intermediates,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	return err
}
