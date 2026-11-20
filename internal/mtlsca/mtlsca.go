// Package mtlsca is the private certificate authority behind gitmoot's
// mutually authenticated TLS listeners: the credential gateway's sandbox
// listener, whose authority lives in memory for one process, and the hub API
// (#2710), whose authority is persisted under the gitmoot home so the client
// certificates it issues to other machines stay valid across daemon restarts.
//
// Every key is Ed25519. Leaves carry only the key usage their side of the
// handshake needs: server leaves ServerAuth, client leaves ClientAuth.
package mtlsca

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"slices"
	"strings"
	"time"
)

// Authority signs server and client leaves.
type Authority struct {
	certificate    *x509.Certificate
	privateKey     ed25519.PrivateKey
	certificatePEM []byte
}

// Leaf is an issued certificate with its private key, both PEM encoded.
// Fingerprint is the SHA-256 of the certificate's DER bytes, which is what a
// listener compares against when it authorizes a peer.
type Leaf struct {
	CertificatePEM []byte
	PrivateKeyPEM  []byte
	Fingerprint    [sha256.Size]byte
	NotAfter       time.Time
}

// New creates an authority valid for validity from now.
func New(commonName string, validity time.Duration) (*Authority, error) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate %s key: %w", commonName, err)
	}
	serial, err := newSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	template := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: commonName},
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(validity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true, IsCA: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, publicKey, privateKey)
	if err != nil {
		return nil, fmt.Errorf("create %s certificate: %w", commonName, err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("parse %s certificate: %w", commonName, err)
	}
	return &Authority{certificate: certificate, privateKey: privateKey,
		certificatePEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}, nil
}

// Parse loads an authority from the PEM pair PrivateKeyPEM and CertificatePEM
// wrote. It refuses a certificate that is not a CA, has expired, or does not
// match the key.
func Parse(certificatePEM, privateKeyPEM []byte) (*Authority, error) {
	certificateBlock, _ := pem.Decode(certificatePEM)
	if certificateBlock == nil || certificateBlock.Type != "CERTIFICATE" {
		return nil, errors.New("authority certificate is not a PEM CERTIFICATE")
	}
	certificate, err := x509.ParseCertificate(certificateBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse authority certificate: %w", err)
	}
	if !certificate.IsCA {
		return nil, errors.New("authority certificate is not a CA")
	}
	if time.Now().After(certificate.NotAfter) {
		return nil, fmt.Errorf("authority certificate expired at %s", certificate.NotAfter.UTC().Format(time.RFC3339))
	}
	keyBlock, _ := pem.Decode(privateKeyPEM)
	if keyBlock == nil || keyBlock.Type != "PRIVATE KEY" {
		return nil, errors.New("authority key is not a PEM PRIVATE KEY")
	}
	parsedKey, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse authority key: %w", err)
	}
	privateKey, ok := parsedKey.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("authority key is not Ed25519")
	}
	publicKey, ok := certificate.PublicKey.(ed25519.PublicKey)
	if !ok || !publicKey.Equal(privateKey.Public()) {
		return nil, errors.New("authority key does not match its certificate")
	}
	return &Authority{certificate: certificate, privateKey: privateKey,
		certificatePEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw})}, nil
}

// CertificatePEM returns a copy of the authority certificate in PEM.
func (a *Authority) CertificatePEM() []byte { return slices.Clone(a.certificatePEM) }

// PrivateKeyPEM returns the authority key in PKCS #8 PEM, for persisting it.
func (a *Authority) PrivateKeyPEM() ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(a.privateKey)
	if err != nil {
		return nil, fmt.Errorf("encode authority key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// Pool returns a certificate pool holding only this authority, for
// tls.Config.ClientCAs or RootCAs.
func (a *Authority) Pool() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(a.certificate)
	return pool
}

// IssueServer issues one leaf covering every host: an IP SAN for addresses
// and a DNS SAN otherwise. The first host is the subject name.
func (a *Authority) IssueServer(hosts []string, validity time.Duration) (tls.Certificate, error) {
	if len(hosts) == 0 {
		return tls.Certificate{}, errors.New("server certificate needs at least one host")
	}
	template := &x509.Certificate{
		Subject:     pkix.Name{CommonName: hosts[0]},
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, host := range hosts {
		if ip := net.ParseIP(host); ip != nil {
			if !slices.ContainsFunc(template.IPAddresses, ip.Equal) {
				template.IPAddresses = append(template.IPAddresses, ip)
			}
		} else if !slices.ContainsFunc(template.DNSNames, func(name string) bool { return strings.EqualFold(name, host) }) {
			template.DNSNames = append(template.DNSNames, host)
		}
	}
	leaf, err := a.issue(template, time.Now().UTC().Add(validity))
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("issue server certificate: %w", err)
	}
	certificate, err := tls.X509KeyPair(leaf.CertificatePEM, leaf.PrivateKeyPEM)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("load server certificate: %w", err)
	}
	return certificate, nil
}

// IssueClient issues a client leaf named commonName that expires at notAfter.
// identity, when set, becomes the leaf's URI SAN.
func (a *Authority) IssueClient(commonName string, identity *url.URL, notAfter time.Time) (Leaf, error) {
	template := &x509.Certificate{
		Subject:     pkix.Name{CommonName: commonName},
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	if identity != nil {
		template.URIs = []*url.URL{identity}
	}
	leaf, err := a.issue(template, notAfter.UTC())
	if err != nil {
		return Leaf{}, fmt.Errorf("issue client certificate: %w", err)
	}
	return leaf, nil
}

func (a *Authority) issue(template *x509.Certificate, notAfter time.Time) (Leaf, error) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return Leaf{}, fmt.Errorf("generate key: %w", err)
	}
	serial, err := newSerial()
	if err != nil {
		return Leaf{}, err
	}
	template.SerialNumber = serial
	template.NotBefore = time.Now().UTC().Add(-time.Minute)
	template.NotAfter = notAfter
	der, err := x509.CreateCertificate(rand.Reader, template, a.certificate, publicKey, a.privateKey)
	if err != nil {
		return Leaf{}, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return Leaf{}, fmt.Errorf("encode key: %w", err)
	}
	return Leaf{
		CertificatePEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		PrivateKeyPEM:  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		Fingerprint:    sha256.Sum256(der),
		NotAfter:       notAfter,
	}, nil
}

func newSerial() (*big.Int, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("generate certificate serial: %w", err)
	}
	return serial, nil
}
