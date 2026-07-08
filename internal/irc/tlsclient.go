package irc

import (
	"crypto/tls"
	"errors"
)

// ClientTLS builds the TLS client config for an optional client
// certificate: the ircd identifies the bot by certificate fingerprint
// (oper certfp autologin), so the same cert must be presented on every
// connect. Both paths empty means plain TLS (nil config); half a pair
// is a config error.
func ClientTLS(certFile, keyFile string) (*tls.Config, error) {
	if certFile == "" && keyFile == "" {
		return nil, nil
	}
	if certFile == "" || keyFile == "" {
		return nil, errors.New("irc: client cert needs both cert and key file")
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}}, nil
}
