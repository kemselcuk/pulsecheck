package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeTestCA generates a self-signed certificate and writes its PEM to a file,
// returning the path.
func writeTestCA(t *testing.T) string {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "pulsecheck-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})

	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatalf("write ca file: %v", err)
	}
	return path
}

// quote renders s as a JSON string literal (test paths only).
func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TestParseTLSOptions covers the per-target tls object (D42): a valid CA file
// resolves into a pool (additive to the system roots), the skip flag resolves,
// and every rejection class fails the load with a config: error.
func TestParseTLSOptions(t *testing.T) {
	caPath := writeTestCA(t)

	parse := func(t *testing.T, tlsBody string) (*Config, error) {
		t.Helper()
		return Parse(strings.NewReader(`{"targets":[{"name":"a","url":"https://example.com"` + tlsBody + `}]}`))
	}

	t.Run("ca file resolves into a pool", func(t *testing.T) {
		cfg, err := parse(t, `,"tls":{"ca_file":`+quote(caPath)+`}`)
		if err != nil {
			t.Fatalf("Parse() error = %v", err)
		}
		tls := cfg.Targets[0].TLS
		if tls.CAFile != caPath {
			t.Errorf("CAFile = %q, want %q", tls.CAFile, caPath)
		}
		if tls.CAPool == nil {
			t.Error("CAPool is nil, want the parsed certificates")
		}
		if tls.InsecureSkipVerify {
			t.Error("InsecureSkipVerify = true, want the default false")
		}
	})

	t.Run("insecure flag resolves without a ca file", func(t *testing.T) {
		cfg, err := parse(t, `,"tls":{"insecure_skip_verify":true}`)
		if err != nil {
			t.Fatalf("Parse() error = %v", err)
		}
		if got := cfg.Targets[0].TLS; !got.InsecureSkipVerify || got.CAPool != nil || got.CAFile != "" {
			t.Errorf("TLS = %+v, want insecure with no CA file", got)
		}
	})

	t.Run("missing file is rejected", func(t *testing.T) {
		_, err := parse(t, `,"tls":{"ca_file":"/definitely/missing/ca.pem"}`)
		if err == nil || !strings.Contains(err.Error(), "ca_file") {
			t.Fatalf("Parse() error = %v, want a ca_file error", err)
		}
	})

	t.Run("certificate-less file is rejected", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "junk.pem")
		if err := os.WriteFile(path, []byte("not a certificate"), 0o600); err != nil {
			t.Fatalf("write junk: %v", err)
		}
		_, err := parse(t, `,"tls":{"ca_file":`+quote(path)+`}`)
		if err == nil || !strings.Contains(err.Error(), "no PEM certificates") {
			t.Fatalf("Parse() error = %v, want a no-certificates error", err)
		}
	})

	t.Run("empty path is rejected", func(t *testing.T) {
		_, err := parse(t, `,"tls":{"ca_file":""}`)
		if err == nil || !strings.Contains(err.Error(), "must not be empty") {
			t.Fatalf("Parse() error = %v, want an empty-path error", err)
		}
	})

	t.Run("no tls object stays zero", func(t *testing.T) {
		cfg, err := parse(t, ``)
		if err != nil {
			t.Fatalf("Parse() error = %v", err)
		}
		if got := cfg.Targets[0].TLS; got != (TLSConfig{}) {
			t.Errorf("TLS = %+v, want the zero value", got)
		}
	})
}

// TestTargetEqual pins the reload-comparison normalization (D42): two parses of
// the same TLS configuration are equivalent even though their pools are
// distinct objects, while a changed path or flag is a difference.
func TestTargetEqual(t *testing.T) {
	caPath := writeTestCA(t)
	body := `{"targets":[{"name":"a","url":"https://example.com","tls":{"ca_file":` + quote(caPath) + `,"insecure_skip_verify":true}}]}`

	parsed := func() Target {
		t.Helper()
		cfg, err := Parse(strings.NewReader(body))
		if err != nil {
			t.Fatalf("Parse() error = %v", err)
		}
		return cfg.Targets[0]
	}

	a, b := parsed(), parsed()
	if a.TLS.CAPool == b.TLS.CAPool {
		t.Fatal("expected two distinct pool objects from two parses")
	}
	if !TargetEqual(a, b) {
		t.Error("TargetEqual = false for two parses of the same configuration")
	}

	c := b
	c.TLS.InsecureSkipVerify = false
	if TargetEqual(a, c) {
		t.Error("TargetEqual = true after a flag change")
	}

	d := b
	d.TLS.CAFile = "other.pem"
	if TargetEqual(a, d) {
		t.Error("TargetEqual = true after a path change")
	}

	if !TargetsEqual([]Target{a}, []Target{b}) {
		t.Error("TargetsEqual = false for equivalent single-element lists")
	}
	if TargetsEqual([]Target{a}, nil) || TargetsEqual([]Target{a}, []Target{a, b}) {
		t.Error("TargetsEqual must reject different lengths")
	}
}
