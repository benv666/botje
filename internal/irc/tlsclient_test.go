package irc

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeSelfSigned writes a self-signed cert+key pair and returns their
// paths, mimicking what `botje keeper` will be pointed at for ircd
// oper certfp auth.
func writeSelfSigned(t *testing.T) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "Meretrix"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")
	kder, _ := x509.MarshalECPrivateKey(key)
	os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), 0o600)
	return certFile, keyFile
}

func TestClientTLS(t *testing.T) {
	certFile, keyFile := writeSelfSigned(t)

	// empty = plain TLS, nil config
	if cfg, err := ClientTLS("", ""); err != nil || cfg != nil {
		t.Fatalf("empty = %v, %v; want nil, nil", cfg, err)
	}
	// half a pair is a config error
	if _, err := ClientTLS(certFile, ""); err == nil {
		t.Fatal("cert without key accepted")
	}
	// missing files error
	if _, err := ClientTLS(certFile+".nope", keyFile); err == nil {
		t.Fatal("missing cert file accepted")
	}
	cfg, err := ClientTLS(certFile, keyFile)
	if err != nil || len(cfg.Certificates) != 1 {
		t.Fatalf("ClientTLS = %+v, %v", cfg, err)
	}
}

// the config from ClientTLS must actually present the certificate: a
// server requiring client certs sees the Meretrix CN.
func TestClientTLSHandshake(t *testing.T) {
	certFile, keyFile := writeSelfSigned(t)
	serverCert, serverKey := writeSelfSigned(t)
	srvPair, err := tls.LoadX509KeyPair(serverCert, serverKey)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{srvPair},
		ClientAuth:   tls.RequireAnyClientCert,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	got := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			got <- "accept: " + err.Error()
			return
		}
		defer conn.Close()
		tc := conn.(*tls.Conn)
		if err := tc.Handshake(); err != nil {
			got <- "handshake: " + err.Error()
			return
		}
		certs := tc.ConnectionState().PeerCertificates
		if len(certs) == 0 {
			got <- "no client cert"
			return
		}
		got <- certs[0].Subject.CommonName
	}()

	cfg, err := ClientTLS(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	cfg.InsecureSkipVerify = true // test server is self-signed
	conn, err := tls.Dial("tcp", ln.Addr().String(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.Handshake(); err != nil {
		t.Fatal(err)
	}
	if cn := <-got; cn != "Meretrix" {
		t.Fatalf("server saw %q, want Meretrix", cn)
	}
	_ = net.Conn(conn)
}
