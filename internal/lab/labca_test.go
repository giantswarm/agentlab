package lab

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/giantswarm/agentlab/internal/config"
)

// The render tests run without a lab's certs/: the lab CA the platform values
// carry (tmplData.LabCA) comes from a throwaway fixture certificate, by an
// absolute path because some tests change the working directory.
func init() {
	abs, err := filepath.Abs("testdata/lab-ca.crt")
	if err != nil {
		panic(err)
	}
	labCAFile = abs
}

// TestPlatformValuesTrustTheLabCAAtTheEgress: Substrate's egress gateway,
// which dials the models Gateway for the actors, gets the lab CA as its extra
// upstream trust, verbatim.
func TestPlatformValuesTrustTheLabCAAtTheEgress(t *testing.T) {
	t.Setenv(GitHubTokenEnv, "")
	out, err := renderTemplate(config.Default(), platformValuesTemplate, nil)
	if err != nil {
		t.Fatal(err)
	}
	var values struct {
		Substrate struct {
			AtenetEgress struct {
				UpstreamTrust struct {
					CABundle string `yaml:"caBundle"`
				} `yaml:"upstreamTrust"`
			} `yaml:"atenetEgress"`
		} `yaml:"substrate"`
	}
	if err := yaml.Unmarshal(out, &values); err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(labCAFile)
	if err != nil {
		t.Fatal(err)
	}
	if got := values.Substrate.AtenetEgress.UpstreamTrust.CABundle; strings.TrimSpace(got) != strings.TrimSpace(string(want)) {
		t.Fatalf("substrate.atenetEgress.upstreamTrust.caBundle = %q, want the lab CA", got)
	}
}
