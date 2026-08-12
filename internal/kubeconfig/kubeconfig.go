// Package kubeconfig loads the subset of kubeconfig files needed to
// authenticate against the Kubernetes API without any third-party
// dependencies: the cluster CA, and either a bearer token (inline or
// from a file) or a TLS client certificate (inline data or file paths).
//
// File-based credentials are re-read on every use, so rotating
// credentials — such as the kubelet's client certificate — are picked
// up automatically. Exec credential plugins are not supported.
package kubeconfig

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
)

// Auth is the usable result of loading a kubeconfig: the CA to verify
// the apiserver with, plus exactly one client credential mechanism.
type Auth struct {
	// Server is the cluster server URL from the kubeconfig. It is
	// informational; callers may prefer to dial known backends directly.
	Server string

	// CAPool verifies the apiserver's serving certificate.
	CAPool *x509.CertPool

	// BearerToken returns the current bearer token. Nil when the
	// kubeconfig uses client-certificate authentication.
	BearerToken func() (string, error)

	// GetClientCertificate plugs into tls.Config for client-certificate
	// authentication. Nil when the kubeconfig uses token authentication.
	GetClientCertificate func(*tls.CertificateRequestInfo) (*tls.Certificate, error)
}

// Load reads and resolves a kubeconfig file.
func Load(path string) (*Auth, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading kubeconfig: %w", err)
	}
	tree, err := parseYAMLTree(data)
	if err != nil {
		return nil, fmt.Errorf("parsing kubeconfig %s: %w", path, err)
	}
	root, ok := tree.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("kubeconfig %s: top level is not a mapping", path)
	}

	clusterName, userName, err := resolveContext(root)
	if err != nil {
		return nil, fmt.Errorf("kubeconfig %s: %w", path, err)
	}

	cluster, err := findNamed(root, "clusters", "cluster", clusterName)
	if err != nil {
		return nil, fmt.Errorf("kubeconfig %s: %w", path, err)
	}
	user, err := findNamed(root, "users", "user", userName)
	if err != nil {
		return nil, fmt.Errorf("kubeconfig %s: %w", path, err)
	}

	auth := &Auth{Server: str(cluster["server"])}

	if auth.CAPool, err = loadCA(cluster); err != nil {
		return nil, fmt.Errorf("kubeconfig %s: %w", path, err)
	}
	if err := loadCredentials(auth, user); err != nil {
		return nil, fmt.Errorf("kubeconfig %s: user %q: %w", path, userName, err)
	}
	return auth, nil
}

// resolveContext returns the cluster and user names selected by
// current-context.
func resolveContext(root map[string]any) (clusterName, userName string, err error) {
	current := str(root["current-context"])
	if current == "" {
		return "", "", fmt.Errorf("current-context is not set")
	}
	ctx, err := findNamed(root, "contexts", "context", current)
	if err != nil {
		return "", "", fmt.Errorf("current-context %q: %w", current, err)
	}
	clusterName, userName = str(ctx["cluster"]), str(ctx["user"])
	if clusterName == "" || userName == "" {
		return "", "", fmt.Errorf("context %q is missing cluster or user", current)
	}
	return clusterName, userName, nil
}

// findNamed locates the entry with the given name in a kubeconfig list
// section ("clusters", "contexts", "users") and returns its inner
// object (keyed "cluster", "context", or "user").
func findNamed(root map[string]any, section, inner, name string) (map[string]any, error) {
	list, ok := root[section].([]any)
	if !ok {
		return nil, fmt.Errorf("section %q is missing or not a list", section)
	}
	for _, item := range list {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if str(entry["name"]) != name {
			continue
		}
		obj, ok := entry[inner].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s %q has no %q object", section, name, inner)
		}
		return obj, nil
	}
	return nil, fmt.Errorf("no entry named %q in %q", name, section)
}

// loadCA builds the CA pool from certificate-authority-data or
// certificate-authority. A CA is required: probing the apiserver with
// credentials over an unverified channel is not supported.
func loadCA(cluster map[string]any) (*x509.CertPool, error) {
	var pemData []byte
	switch {
	case str(cluster["certificate-authority-data"]) != "":
		decoded, err := base64.StdEncoding.DecodeString(str(cluster["certificate-authority-data"]))
		if err != nil {
			return nil, fmt.Errorf("certificate-authority-data: invalid base64: %w", err)
		}
		pemData = decoded
	case str(cluster["certificate-authority"]) != "":
		data, err := os.ReadFile(str(cluster["certificate-authority"]))
		if err != nil {
			return nil, fmt.Errorf("certificate-authority: %w", err)
		}
		pemData = data
	default:
		return nil, fmt.Errorf("cluster has neither certificate-authority nor certificate-authority-data")
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemData) {
		return nil, fmt.Errorf("certificate-authority contains no valid PEM certificates")
	}
	return pool, nil
}

// loadCredentials wires exactly one authentication mechanism from the
// user object into auth.
func loadCredentials(auth *Auth, user map[string]any) error {
	if _, hasExec := user["exec"]; hasExec {
		return fmt.Errorf("exec credential plugins are not supported; use a token or client certificate")
	}
	if _, hasProvider := user["auth-provider"]; hasProvider {
		return fmt.Errorf("auth-provider is not supported; use a token or client certificate")
	}

	switch {
	case str(user["token"]) != "":
		token := str(user["token"])
		auth.BearerToken = func() (string, error) { return token, nil }
		return nil

	case str(user["tokenFile"]) != "":
		path := str(user["tokenFile"])
		auth.BearerToken = func() (string, error) {
			data, err := os.ReadFile(path)
			if err != nil {
				return "", fmt.Errorf("reading token file: %w", err)
			}
			return strings.TrimSpace(string(data)), nil
		}
		return nil

	case str(user["client-certificate-data"]) != "" || str(user["client-key-data"]) != "":
		certPEM, err := b64Field(user, "client-certificate-data")
		if err != nil {
			return err
		}
		keyPEM, err := b64Field(user, "client-key-data")
		if err != nil {
			return err
		}
		cert, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			return fmt.Errorf("parsing inline client certificate: %w", err)
		}
		auth.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return &cert, nil
		}
		return nil

	case str(user["client-certificate"]) != "" || str(user["client-key"]) != "":
		certPath, keyPath := str(user["client-certificate"]), str(user["client-key"])
		if certPath == "" || keyPath == "" {
			return fmt.Errorf("client-certificate and client-key must both be set")
		}
		// Re-load on every handshake: kubelet rotates these files.
		auth.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			cert, err := tls.LoadX509KeyPair(certPath, keyPath)
			if err != nil {
				return nil, fmt.Errorf("loading client certificate: %w", err)
			}
			return &cert, nil
		}
		return nil
	}

	return fmt.Errorf("no usable credentials (want token, tokenFile, or a client certificate)")
}

func b64Field(m map[string]any, key string) ([]byte, error) {
	raw := str(m[key])
	if raw == "" {
		return nil, fmt.Errorf("%s must be set alongside its pair", key)
	}
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: invalid base64: %w", key, err)
	}
	return decoded, nil
}

// str extracts a string from a parsed YAML value, returning "" for
// anything else.
func str(v any) string {
	s, _ := v.(string)
	return s
}
