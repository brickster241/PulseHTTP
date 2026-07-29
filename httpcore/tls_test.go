package httpcore

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net"
	"testing"
	"time"
)

// selfSigned mints an in-memory certificate for 127.0.0.1 — no files, no
// external tooling, valid only for the test's lifetime.
func selfSigned(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "pulsehttp-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// TestTLSServesSameProtocol: the identical HTTP/1.1 engine must answer over
// a TLS-wrapped listener — transport swaps, protocol layer untouched.
func TestTLSServesSameProtocol(t *testing.T) {
	cert := selfSigned(t)
	srv := NewServer(Config{
		Addr:    "127.0.0.1:0",
		Handler: func(req *Request, w *ResponseWriter) { w.WriteString("secure hello") },
		TLS:     &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
	})
	go srv.ListenAndServe()
	<-srv.Ready()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}()

	// Verify against the test CA properly — no InsecureSkipVerify: the
	// self-signed cert IS the root, so trust exactly it and nothing else.
	roots := x509.NewCertPool()
	parsed, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	roots.AddCert(parsed)
	conn, err := tls.Dial("tcp", srv.Addr(), &tls.Config{RootCAs: roots})
	if err != nil {
		t.Fatalf("tls dial: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))

	fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: x\r\n\r\n")
	status, _, body := readOneResponse(t, bufio.NewReader(conn))
	if status != 200 || body != "secure hello" {
		t.Fatalf("got %d %q over TLS", status, body)
	}
	if conn.ConnectionState().Version < tls.VersionTLS12 {
		t.Fatal("negotiated TLS below the configured minimum")
	}
}
