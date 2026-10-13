package lab

import (
	"crypto/x509"
	"testing"
	"time"
)

// TestLabTransportClosesIdleConnections: the shared transport closes a pooled
// connection before an installation's load balancer silently drops it (Azure's
// four minutes), so a call after a long probe dials afresh instead of reading
// EOF.
func TestLabTransportClosesIdleConnections(t *testing.T) {
	tr := newLabTransport(x509.NewCertPool())
	if tr.IdleConnTimeout <= 0 || tr.IdleConnTimeout >= 4*time.Minute {
		t.Errorf("IdleConnTimeout is %s, want above zero and below four minutes", tr.IdleConnTimeout)
	}
	if tr.TLSClientConfig == nil || tr.TLSClientConfig.RootCAs == nil || tr.DialContext == nil {
		t.Errorf("the transport lost the lab's trust or dialler: %+v", tr)
	}
}
