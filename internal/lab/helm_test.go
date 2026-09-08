package lab

import "testing"

func TestHelmMajor(t *testing.T) {
	cases := []struct {
		version string
		want    int
		wantErr bool
	}{
		{"v4.2.2", 4, false},
		{"v3.19.0", 3, false},
		{"v4.0.0-rc.1", 4, false},
		{"4.2.2", 4, false}, // defensive: no leading v
		{"", 0, true},
		{"garbage", 0, true},
	}
	for _, c := range cases {
		got, err := helmMajor(c.version)
		if c.wantErr {
			if err == nil {
				t.Errorf("helmMajor(%q): expected error, got %d", c.version, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("helmMajor(%q): %v", c.version, err)
			continue
		}
		if got != c.want {
			t.Errorf("helmMajor(%q) = %d, want %d", c.version, got, c.want)
		}
	}
}
