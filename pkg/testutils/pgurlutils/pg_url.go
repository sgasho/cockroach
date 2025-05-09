// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package pgurlutils

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/security/certnames"
	"github.com/cockroachdb/cockroach/pkg/security/securitytest"
	"github.com/cockroachdb/cockroach/pkg/util/fileutil"
)

// PGUrl is like PGUrlE, but uses t.Fatal to handle errors.
func PGUrl(t testing.TB, servingAddr, prefix string, user *url.Userinfo) (url.URL, func()) {
	return PGUrlWithOptionalClientCerts(t, servingAddr, prefix, user, true, "" /* withCerts */)
}

// PGUrlE returns a postgres connection url which connects to this server with the given user, and a
// cleanup function which must be called after all connections created using the connection url have
// been closed.
//
// In order to connect securely using postgres, this method will create temporary on-disk copies of
// certain embedded security certificates. The certificates will be created in a new temporary
// directory. The returned cleanup function will delete this temporary directory.
// Note that two calls to this function for the same `user` will generate different
// copies of the certificates, so the cleanup function must always be called.
//
// Args:
//
//	prefix: A prefix to be prepended to the temp file names generated, for debugging.
func PGUrlE(servingAddr, prefix string, user *url.Userinfo) (url.URL, func(), error) {
	return PGUrlWithOptionalClientCertsE(servingAddr, prefix, user, true, "" /* withCerts */)
}

// PGUrlWithOptionalClientCerts is like PGUrlWithOptionalClientCertsE, but uses t.Fatal to handle
// errors.
func PGUrlWithOptionalClientCerts(
	t testing.TB,
	servingAddr, prefix string,
	user *url.Userinfo,
	withClientCerts bool,
	certName string,
) (url.URL, func()) {
	u, f, err := PGUrlWithOptionalClientCertsE(servingAddr, prefix, user, withClientCerts, certName)
	if err != nil {
		t.Fatal(err)
	}
	return u, f
}

// PGUrlWithOptionalClientCertsE is like PGUrlE but the caller can
// customize whether the client certificates are loaded on-disk and in the URL.
// certName is name of the certificate to use. If empty, the username is used.
func PGUrlWithOptionalClientCertsE(
	servingAddr, prefix string, user *url.Userinfo, withClientCerts bool, certName string,
) (url.URL, func(), error) {
	host, port, err := net.SplitHostPort(servingAddr)
	if err != nil {
		return url.URL{}, func() {}, err
	}

	// TODO(benesch): Audit usage of prefix and replace the following line with
	// `testutils.TempDir(t)` if prefix can always be `t.Name()`.
	tempDir, err := os.MkdirTemp("", fileutil.EscapeFilename(prefix))
	if err != nil {
		return url.URL{}, func() {}, err
	}

	// This CA is the one used by the SQL client driver to authenticate KV nodes on the host cluster.
	caPath := filepath.Join(certnames.EmbeddedCertsDir, certnames.EmbeddedCACert)
	tempCAPath, err := securitytest.RestrictedCopy(caPath, tempDir, "ca")
	if err != nil {
		return url.URL{}, func() {}, err
	}
	// This CA is the one used by the SQL client driver to authenticate SQL tenant servers.
	tenantCAPath := filepath.Join(certnames.EmbeddedCertsDir, certnames.EmbeddedTenantCACert)
	if err := securitytest.AppendFile(tenantCAPath, tempCAPath); err != nil {
		return url.URL{}, func() {}, err
	}
	options := url.Values{}
	options.Add("sslrootcert", tempCAPath)

	if withClientCerts {
		if certName == "" {
			certName = user.Username()
		}
		certPath := filepath.Join(certnames.EmbeddedCertsDir, fmt.Sprintf("client.%s.crt", certName))
		keyPath := filepath.Join(certnames.EmbeddedCertsDir, fmt.Sprintf("client.%s.key", certName))

		// Copy these assets to disk from embedded strings, so this test can
		// run from a standalone binary.
		tempCertPath, err := securitytest.RestrictedCopy(certPath, tempDir, "cert")
		if err != nil {
			return url.URL{}, func() {}, err
		}
		tempKeyPath, err := securitytest.RestrictedCopy(keyPath, tempDir, "key")
		if err != nil {
			return url.URL{}, func() {}, err
		}
		options.Add("sslcert", tempCertPath)
		options.Add("sslkey", tempKeyPath)
		options.Add("sslmode", "verify-full")
	} else {
		options.Add("sslmode", "verify-ca")
	}

	return url.URL{
		Scheme:   "postgres",
		User:     user,
		Host:     net.JoinHostPort(host, port),
		RawQuery: options.Encode(),
	}, func() { _ = os.RemoveAll(tempDir) }, nil
}
