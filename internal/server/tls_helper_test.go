package server

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
)

// newTLSTestServer serves h over HTTPS with pair and returns a server whose
// client trusts that certificate.
func newTLSTestServer(h http.Handler, pair tls.Certificate) *httptest.Server {
	srv := httptest.NewUnstartedServer(h)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{pair}}
	srv.StartTLS()
	leaf, _ := x509.ParseCertificate(pair.Certificate[0])
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	srv.Client().Transport = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "localhost"}}
	return srv
}
