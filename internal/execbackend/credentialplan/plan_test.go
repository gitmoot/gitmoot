package credentialplan

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/credgw"
)

func TestPlanClientTLSConfigIsolatesCertificateState(t *testing.T) {
	lease := testNetworkLease(t)
	source := lease.ClientCertificate()
	sourceLeaf := source.Leaf
	sourceKey := source.PrivateKey.(*ecdsa.PrivateKey)
	originalCommonName := sourceLeaf.Subject.CommonName
	originalRaw := append([]byte(nil), sourceLeaf.Raw...)
	originalD := new(big.Int).Set(sourceKey.D)

	plan, err := newFromNetworkLease(lease)
	if err != nil {
		t.Fatalf("newFromNetworkLease: %v", err)
	}

	config, err := plan.ClientTLSConfig()
	if err != nil {
		t.Fatalf("ClientTLSConfig: %v", err)
	}

	t.Run("ingress", func(t *testing.T) {
		// Mutate the lease-side certificate after plan construction. The frozen
		// plan must retain the original leaf and key.
		sourceLeaf.Subject.CommonName = "lease-side-marker"
		sourceLeaf.Raw[0] ^= 0xff
		sourceKey.D.SetInt64(1)

		certificate := getClientCertificate(t, config)
		if certificate.Leaf == sourceLeaf {
			t.Error("TLS client certificate Leaf shares the ingress pointer")
		}
		if certificate.PrivateKey.(*ecdsa.PrivateKey) == sourceKey {
			t.Error("TLS client certificate PrivateKey shares the ingress pointer")
		}
		assertCertificateState(t, certificate, originalCommonName, originalRaw, originalD)
	})

	t.Run("pointer_identity", func(t *testing.T) {
		first := getClientCertificate(t, config)
		second := getClientCertificate(t, config)
		if second.Leaf == first.Leaf {
			t.Error("successive TLS client certificates share a Leaf pointer")
		}
		if second.PrivateKey.(*ecdsa.PrivateKey) == first.PrivateKey.(*ecdsa.PrivateKey) {
			t.Error("successive TLS client certificates share a PrivateKey pointer")
		}
	})

	t.Run("consumer_mutation", func(t *testing.T) {
		first := getClientCertificate(t, config)
		first.Leaf.Subject.CommonName = "consumer-side-marker"
		first.Leaf.Raw[0] ^= 0xff
		first.PrivateKey.(*ecdsa.PrivateKey).D.SetInt64(2)

		second := getClientCertificate(t, config)
		assertCertificateState(t, second, originalCommonName, originalRaw, originalD)
	})
}

func getClientCertificate(t *testing.T, config *tls.Config) *tls.Certificate {
	t.Helper()
	certificate, err := config.GetClientCertificate(nil)
	if err != nil {
		t.Fatalf("GetClientCertificate: %v", err)
	}
	return certificate
}

func assertCertificateState(t *testing.T, certificate *tls.Certificate, commonName string, raw []byte, d *big.Int) {
	t.Helper()
	if certificate.Leaf.Subject.CommonName != commonName {
		t.Fatalf("leaf common name = %q, want %q", certificate.Leaf.Subject.CommonName, commonName)
	}
	if string(certificate.Leaf.Raw) != string(raw) {
		t.Fatal("leaf raw bytes changed through shared state")
	}
	key := certificate.PrivateKey.(*ecdsa.PrivateKey)
	if key.D.Cmp(d) != 0 {
		t.Fatalf("private key D = %s, want %s", key.D, d)
	}
}

func testNetworkLease(t *testing.T) *credgw.NetworkLease {
	t.Helper()
	now := time.Now().UTC()
	serverCA, serverCAKey := testCertificateAuthority(t, now, "server-ca")
	clientCA, clientCAKey := testCertificateAuthority(t, now, "client-ca")
	serverCertificate := testServerCertificate(t, now, serverCA, serverCAKey)
	gateway, err := credgw.Start(nil)
	if err != nil {
		t.Fatalf("start credential gateway: %v", err)
	}
	network, err := gateway.StartNetworkProxy(credgw.NetworkProxyConfig{
		Address:             "127.0.0.1:0",
		AdvertisedAuthority: "127.0.0.1",
		ServerCertificate:   serverCertificate,
		ClientCA:            clientCA,
		ClientCAKey:         clientCAKey,
	})
	if err != nil {
		_ = gateway.Close(context.Background())
		t.Fatalf("start network credential gateway: %v", err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(func() {
		upstream.Close()
		_ = network.Close(context.Background())
		_ = gateway.Close(context.Background())
	})
	policy := credgw.ProxyPolicy{
		Upstream:          upstream.URL,
		AuthKind:          credgw.ProxyAuthBearer,
		AllowLoopbackHTTP: true,
	}
	lease, err := network.RegisterProxy("certificate-isolation-job", now.Add(10*time.Minute), policy, func(context.Context) (credgw.ResolvedCredential, error) {
		return credgw.ResolvedCredential{Value: "unused", Upstream: policy.Upstream, AuthKind: policy.AuthKind}, nil
	})
	if err != nil {
		t.Fatalf("register network credential lease: %v", err)
	}
	return lease
}

func testCertificateAuthority(t *testing.T, now time.Time, commonName string) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create CA certificate: %v", err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse CA certificate: %v", err)
	}
	return certificate, key
}

func testServerCertificate(t *testing.T, now time.Time, ca *x509.Certificate, caKey *ecdsa.PrivateKey) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate server key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create server certificate: %v", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse server certificate: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der, ca.Raw}, PrivateKey: key, Leaf: leaf}
}
