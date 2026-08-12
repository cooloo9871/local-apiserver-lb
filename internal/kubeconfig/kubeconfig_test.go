package kubeconfig

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// makeCA returns a self-signed CA cert as PEM.
func makeCA(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "kubernetes"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// makeClientPair returns a self-signed client cert + key as PEM.
func makeClientPair(t *testing.T, cn string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}

func writeFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func b64(data []byte) string { return base64.StdEncoding.EncodeToString(data) }

func TestLoadTokenKubeconfig(t *testing.T) {
	caPEM := makeCA(t)
	dir := t.TempDir()
	path := writeFile(t, dir, "kubeconfig", []byte(`
apiVersion: v1
kind: Config
clusters:
- cluster:
    certificate-authority-data: `+b64(caPEM)+`
    server: https://k8s-api.local:6443
  name: topgun
contexts:
- context:
    cluster: topgun
    user: lb-discovery
  name: default
current-context: default
preferences: {}
users:
- name: lb-discovery
  user:
    token: secret-token-abc
`))

	auth, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if auth.CAPool == nil {
		t.Error("CAPool is nil, want parsed CA")
	}
	tok, err := auth.BearerToken()
	if err != nil {
		t.Fatalf("BearerToken returned error: %v", err)
	}
	if tok != "secret-token-abc" {
		t.Errorf("BearerToken = %q, want secret-token-abc", tok)
	}
	if auth.GetClientCertificate != nil {
		t.Error("GetClientCertificate set for token-auth kubeconfig")
	}
	if auth.Server != "https://k8s-api.local:6443" {
		t.Errorf("Server = %q", auth.Server)
	}
}

func TestLoadTokenFileKubeconfig(t *testing.T) {
	caPEM := makeCA(t)
	dir := t.TempDir()
	tokenPath := writeFile(t, dir, "token", []byte("file-token-1\n"))
	path := writeFile(t, dir, "kubeconfig", []byte(`
apiVersion: v1
clusters:
- cluster:
    certificate-authority-data: `+b64(caPEM)+`
    server: https://x:6443
  name: c
contexts:
- context: {cluster: c, user: u}
  name: ctx
current-context: ctx
users:
- name: u
  user:
    tokenFile: `+tokenPath+`
`))

	auth, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	tok, err := auth.BearerToken()
	if err != nil {
		t.Fatal(err)
	}
	if tok != "file-token-1" {
		t.Errorf("BearerToken = %q, want file-token-1 (trimmed)", tok)
	}

	// The token file must be re-read on every call (rotation support).
	os.WriteFile(tokenPath, []byte("file-token-2"), 0o600)
	tok, err = auth.BearerToken()
	if err != nil {
		t.Fatal(err)
	}
	if tok != "file-token-2" {
		t.Errorf("BearerToken after rotation = %q, want file-token-2", tok)
	}
}

func TestLoadClientCertDataKubeconfig(t *testing.T) {
	caPEM := makeCA(t)
	certPEM, keyPEM := makeClientPair(t, "system:node:w1")
	dir := t.TempDir()
	path := writeFile(t, dir, "kubeconfig", []byte(`
apiVersion: v1
clusters:
- cluster:
    certificate-authority-data: `+b64(caPEM)+`
    server: https://x:6443
  name: c
contexts:
- context:
    cluster: c
    user: u
  name: ctx
current-context: ctx
users:
- name: u
  user:
    client-certificate-data: `+b64(certPEM)+`
    client-key-data: `+b64(keyPEM)+`
`))

	auth, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if auth.GetClientCertificate == nil {
		t.Fatal("GetClientCertificate is nil for cert-auth kubeconfig")
	}
	cert, err := auth.GetClientCertificate(&tls.CertificateRequestInfo{})
	if err != nil {
		t.Fatalf("GetClientCertificate returned error: %v", err)
	}
	if len(cert.Certificate) == 0 {
		t.Error("returned certificate is empty")
	}
	if auth.BearerToken != nil {
		t.Error("BearerToken set for cert-auth kubeconfig")
	}
}

func TestLoadClientCertFileKubeconfigReloadsOnRotation(t *testing.T) {
	// kubelet.conf points at rotating cert files; every handshake must
	// pick up the current file content.
	caPEM := makeCA(t)
	cert1, key1 := makeClientPair(t, "system:node:w1")
	dir := t.TempDir()
	certPath := writeFile(t, dir, "client.pem", cert1)
	keyPath := writeFile(t, dir, "client.key", key1)
	caPath := writeFile(t, dir, "ca.crt", caPEM)
	path := writeFile(t, dir, "kubeconfig", []byte(`
apiVersion: v1
clusters:
- cluster:
    certificate-authority: `+caPath+`
    server: https://x:6443
  name: c
contexts:
- context:
    cluster: c
    user: u
  name: ctx
current-context: ctx
users:
- name: u
  user:
    client-certificate: `+certPath+`
    client-key: `+keyPath+`
`))

	auth, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	first, err := auth.GetClientCertificate(&tls.CertificateRequestInfo{})
	if err != nil {
		t.Fatal(err)
	}

	// Rotate the files: a new pair with a different CN.
	cert2, key2 := makeClientPair(t, "system:node:w1-rotated")
	os.WriteFile(certPath, cert2, 0o600)
	os.WriteFile(keyPath, key2, 0o600)

	second, err := auth.GetClientCertificate(&tls.CertificateRequestInfo{})
	if err != nil {
		t.Fatal(err)
	}
	if string(first.Certificate[0]) == string(second.Certificate[0]) {
		t.Error("client certificate not re-read after rotation")
	}
}

func TestLoadSelectsCurrentContext(t *testing.T) {
	caPEM := makeCA(t)
	dir := t.TempDir()
	path := writeFile(t, dir, "kubeconfig", []byte(`
apiVersion: v1
clusters:
- cluster:
    certificate-authority-data: `+b64(caPEM)+`
    server: https://wrong:6443
  name: other
- cluster:
    certificate-authority-data: `+b64(caPEM)+`
    server: https://right:6443
  name: main
contexts:
- context: {cluster: other, user: other-user}
  name: other-ctx
- context: {cluster: main, user: main-user}
  name: main-ctx
current-context: main-ctx
users:
- name: other-user
  user: {token: wrong-token}
- name: main-user
  user: {token: right-token}
`))

	auth, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if auth.Server != "https://right:6443" {
		t.Errorf("Server = %q, want the current-context cluster", auth.Server)
	}
	tok, _ := auth.BearerToken()
	if tok != "right-token" {
		t.Errorf("BearerToken = %q, want right-token", tok)
	}
}

func TestLoadErrors(t *testing.T) {
	caPEM := makeCA(t)
	dir := t.TempDir()

	cases := []struct {
		name    string
		content string
		wantSub string
	}{
		{
			name: "no auth credentials",
			content: `
apiVersion: v1
clusters:
- cluster: {certificate-authority-data: ` + b64(caPEM) + `, server: "https://x:6443"}
  name: c
contexts:
- context: {cluster: c, user: u}
  name: ctx
current-context: ctx
users:
- name: u
  user: {}
`,
			wantSub: "credentials",
		},
		{
			name: "exec plugin unsupported",
			content: `
apiVersion: v1
clusters:
- cluster: {certificate-authority-data: ` + b64(caPEM) + `, server: "https://x:6443"}
  name: c
contexts:
- context: {cluster: c, user: u}
  name: ctx
current-context: ctx
users:
- name: u
  user:
    exec:
      command: aws
`,
			wantSub: "exec",
		},
		{
			name: "missing CA",
			content: `
apiVersion: v1
clusters:
- cluster: {server: "https://x:6443"}
  name: c
contexts:
- context: {cluster: c, user: u}
  name: ctx
current-context: ctx
users:
- name: u
  user: {token: t}
`,
			wantSub: "certificate-authority",
		},
		{
			name: "unknown context",
			content: `
apiVersion: v1
clusters:
- cluster: {certificate-authority-data: ` + b64(caPEM) + `, server: "https://x:6443"}
  name: c
contexts:
- context: {cluster: c, user: u}
  name: ctx
current-context: nonexistent
users:
- name: u
  user: {token: t}
`,
			wantSub: "context",
		},
		{
			name:    "bad base64",
			wantSub: "base64",
			content: `
apiVersion: v1
clusters:
- cluster: {certificate-authority-data: "!!!notbase64!!!", server: "https://x:6443"}
  name: c
contexts:
- context: {cluster: c, user: u}
  name: ctx
current-context: ctx
users:
- name: u
  user: {token: t}
`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeFile(t, t.TempDir(), "kubeconfig", []byte(tc.content))
			_, err := Load(path)
			if err == nil {
				t.Fatalf("Load accepted %s, want error", tc.name)
			}
			if !strings.Contains(strings.ToLower(err.Error()), tc.wantSub) {
				t.Errorf("error %q does not mention %q", err, tc.wantSub)
			}
		})
	}

	if _, err := Load(filepath.Join(dir, "missing")); err == nil {
		t.Error("Load accepted missing file, want error")
	}
}
