package lab

import (
	"context"
	"strings"
	"testing"
)

// A cluster without the certificates.k8s.io/v1beta1 API is refused before
// anything installs, with the only fix — a new cluster — spelled out.
func TestSubstratePreflightWithoutTheAPI(t *testing.T) {
	newFakeLab(t)
	err := preflightPodCertificateAPI(context.Background())
	if err == nil || !strings.Contains(err.Error(), "agentlab down && agentlab up") {
		t.Errorf("want the recreate hint, got %v", err)
	}
}
