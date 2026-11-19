// SPDX-License-Identifier: GPL-3.0-or-later

//go:build linux

package lab

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"openrung/internal/wssbridge"
)

// CAPath is the lab certificate authority. The internet stand-in signs a
// certificate for whatever hostname a client asks for, so any process that
// trusts this file sees a working HTTPS internet.
const CAPath = RunDir + "/ca.pem"

// ServeInternet runs the lab's stand-in for the rest of the internet: an HTTP
// and HTTPS server that answers for every hostname. It blocks until a
// listener fails.
//
//	/ip             the caller's address as this server sees it (the exit IP)
//	/bytes/<n>      n bytes of payload, for throughput checks
//	/generate_204   an empty 204, the usual connectivity-check reply
//	/api/v1/wss-bridge  on the WSS front host, a CDN edge for relay-f
//	anything else   a short text page naming the requested host
func ServeInternet(caPath string) error {
	issuer, err := newCertIssuer()
	if err != nil {
		return err
	}
	if err := os.WriteFile(caPath, issuer.caPEM, 0o644); err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/ip", func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		fmt.Fprintln(w, host)
	})
	mux.HandleFunc("/generate_204", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/bytes/", func(w http.ResponseWriter, r *http.Request) {
		n, err := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/bytes/"), 10, 64)
		if err != nil || n < 0 || n > 1<<30 {
			http.Error(w, "want /bytes/<n> with n up to 1 GiB", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.FormatInt(n, 10))
		chunk := make([]byte, 32<<10)
		for n > 0 {
			size := min(n, int64(len(chunk)))
			if _, err := w.Write(chunk[:size]); err != nil {
				return
			}
			n -= size
		}
	})
	page := func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "lab internet: %s%s\n", r.Host, r.URL.Path)
	}
	mux.HandleFunc("/", page)
	// The WSS front for relay-f (see wss.go).
	mux.HandleFunc(wssbridge.BridgePath, serveFront(page))

	failed := make(chan error, 2)
	go func() { failed <- (&http.Server{Addr: ":80", Handler: mux}).ListenAndServe() }()
	go func() {
		server := &http.Server{
			Addr:      ":443",
			Handler:   mux,
			TLSConfig: &tls.Config{GetCertificate: issuer.certificate},
		}
		failed <- server.ListenAndServeTLS("", "")
	}()
	return <-failed
}

// certIssuer signs a leaf certificate per requested hostname.
type certIssuer struct {
	caCert *x509.Certificate
	caKey  *ecdsa.PrivateKey
	caPEM  []byte

	mu     sync.Mutex
	leaves map[string]*tls.Certificate
}

func newCertIssuer() (*certIssuer, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "OpenRung lab CA (test only)"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(7 * 24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &certIssuer{
		caCert: cert,
		caKey:  key,
		caPEM:  pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		leaves: make(map[string]*tls.Certificate),
	}, nil
}

func (i *certIssuer) certificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	name := hello.ServerName
	if name == "" {
		name = "internet.lab"
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if leaf, ok := i.leaves[name]; ok {
		return leaf, nil
	}
	if len(i.leaves) >= 4096 {
		return nil, errors.New("lab internet: too many distinct hostnames")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(int64(len(i.leaves) + 2)),
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(7 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip := net.ParseIP(name); ip != nil {
		template.IPAddresses = []net.IP{ip}
	} else {
		template.DNSNames = []string{name}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, i.caCert, &key.PublicKey, i.caKey)
	if err != nil {
		return nil, err
	}
	leaf := &tls.Certificate{Certificate: [][]byte{der, i.caCert.Raw}, PrivateKey: key}
	i.leaves[name] = leaf
	return leaf, nil
}
