package config

import (
	"crypto/x509"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
)

// maxCAPEMBytes bounds how much of a configured CA file is read, so a
// pathological input cannot blow up memory at load time.
const maxCAPEMBytes = 1 << 20

// TLSConfig is the per-target TLS trust configuration (D42).
//
// CAFile is the configured PEM path as written (kept for logs and reload
// comparison); CAPool is the parsed result the checker uses — the extra roots
// are ADDITIVE to the system roots. Both are empty/nil when no CA file is
// configured. InsecureSkipVerify disables certificate verification for the
// target's requests; it exists for lab and self-signed environments and is
// deliberately loud in the documentation.
type TLSConfig struct {
	CAFile             string
	CAPool             *x509.CertPool
	InsecureSkipVerify bool
}

// rawTLS mirrors the on-disk "tls" object before defaults are applied.
type rawTLS struct {
	CAFile             *string `json:"ca_file"`
	InsecureSkipVerify *bool   `json:"insecure_skip_verify"`
}

// parseTLS resolves an optional per-target "tls" object. The CA file is read
// here, once, with a size cap: every later stage can rely on a valid pool, and
// a missing or certificate-less file fails the load with a config: error
// (D42). The pool is additive to the system roots, so a custom CA extends
// trust rather than replacing it.
func parseTLS(field string, raw *rawTLS) (TLSConfig, error) {
	if raw == nil {
		return TLSConfig{}, nil
	}

	var tlsConfig TLSConfig
	if raw.InsecureSkipVerify != nil {
		tlsConfig.InsecureSkipVerify = *raw.InsecureSkipVerify
	}

	if raw.CAFile != nil {
		path := strings.TrimSpace(*raw.CAFile)
		if path == "" {
			return TLSConfig{}, fmt.Errorf("config: %s.ca_file must not be empty", field)
		}

		f, err := os.Open(path)
		if err != nil {
			return TLSConfig{}, fmt.Errorf("config: %s.ca_file: %w", field, err)
		}
		defer func() { _ = f.Close() }()

		pemBytes, err := io.ReadAll(io.LimitReader(f, maxCAPEMBytes+1))
		if err != nil {
			return TLSConfig{}, fmt.Errorf("config: %s.ca_file: %w", field, err)
		}
		if len(pemBytes) > maxCAPEMBytes {
			return TLSConfig{}, fmt.Errorf("config: %s.ca_file %q exceeds the %d-byte cap", field, path, maxCAPEMBytes)
		}

		pool, err := x509.SystemCertPool()
		if err != nil {
			// A missing system pool is unusual but not fatal: the custom
			// certificates still form a usable pool.
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pemBytes) {
			return TLSConfig{}, fmt.Errorf("config: %s.ca_file %q contains no PEM certificates", field, path)
		}

		tlsConfig.CAFile = path
		tlsConfig.CAPool = pool
	}

	return tlsConfig, nil
}

// TargetEqual reports whether two resolved targets are equivalent for
// scheduling and reload purposes. It compares the TLS configuration by its
// INPUTS — the CA path and the skip flag — rather than by the parsed pool:
// two parses of the same file produce distinct pools whose internal state
// (lazy parsing) is not a meaningful difference. A consequence, documented in
// D42: changing a CA file's CONTENT in place does not count as a change; the
// path is what a reload compares, and content changes need a restart.
func TargetEqual(a, b Target) bool {
	a.TLS.CAPool = nil
	b.TLS.CAPool = nil
	return reflect.DeepEqual(a, b)
}

// TargetsEqual applies TargetEqual over a whole list.
func TargetsEqual(a, b []Target) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !TargetEqual(a[i], b[i]) {
			return false
		}
	}
	return true
}
