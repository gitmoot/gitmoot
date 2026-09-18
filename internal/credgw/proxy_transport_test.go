package credgw

import "testing"

// TestProxyTransportAdvertisesOnlyHTTP11 is the regression for the defect that
// stopped the model gateway reaching api.anthropic.com at all.
//
// The transport deliberately speaks HTTP/1.1 so response trailers are not
// silently protocol-dependent. But DISABLING HTTP/2 DOES NOT UN-ADVERTISE IT:
// cloning http.DefaultTransport carries TLSClientConfig.NextProtos
// ["h2","http/1.1"], and neither ForceAttemptHTTP2=false nor an empty
// TLSNextProto rewrites that list. An upstream that supports HTTP/2 then selects
// it, and the HTTP/1-only client fails parsing the SETTINGS frame — so every
// forwarded request became a 502.
//
// MUTATION: remove the NextProtos assignment in proxyHTTPTransport and this goes
// red with ["h2" "http/1.1"].
func TestProxyTransportAdvertisesOnlyHTTP11(t *testing.T) {
	transport := proxyHTTPTransport()
	if transport.TLSClientConfig == nil {
		t.Fatal("TLSClientConfig = nil, want an explicit config pinning ALPN")
	}
	got := transport.TLSClientConfig.NextProtos
	if len(got) != 1 || got[0] != "http/1.1" {
		t.Fatalf("ALPN NextProtos = %v, want exactly [http/1.1]: advertising h2 while parsing HTTP/1 breaks every HTTP/2 upstream", got)
	}
	if transport.ForceAttemptHTTP2 {
		t.Fatal("ForceAttemptHTTP2 = true, want false")
	}
	if transport.TLSNextProto == nil {
		t.Fatal("TLSNextProto = nil, want a non-nil empty map so the transport cannot upgrade")
	}
}
