package config

import (
	"reflect"
	"testing"
)

// substrateNodes is 0 to MaxSubstrateNodes and needs Substrate, i.e. the
// agents; kind names the workers <cluster>-worker, <cluster>-worker2, ….
func TestSubstrateNodes(t *testing.T) {
	for _, tc := range []struct {
		n      int
		agents bool
		ok     bool
	}{
		{0, true, true},
		{2, true, true},
		{MaxSubstrateNodes, true, true},
		{MaxSubstrateNodes + 1, true, false},
		{-1, true, false},
		{1, false, false},
		{0, false, true},
	} {
		cfg := Default()
		cfg.SubstrateNodes = tc.n
		cfg.Platform.Agents = tc.agents
		if err := cfg.Validate(); tc.ok != (err == nil) {
			t.Errorf("substrateNodes %d, agents %v: err = %v, want ok=%v", tc.n, tc.agents, err, tc.ok)
		}
	}
	cfg := Default()
	if got := cfg.SubstrateNodeNames(); len(got) != 0 {
		t.Errorf("the default names workers %v", got)
	}
	cfg.SubstrateNodes = 3
	if got, want := cfg.SubstrateNodeNames(), []string{"agentlab-worker", "agentlab-worker2", "agentlab-worker3"}; !reflect.DeepEqual(got, want) {
		t.Errorf("SubstrateNodeNames = %v, want %v", got, want)
	}
}
