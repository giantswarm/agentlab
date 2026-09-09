package lab

import "testing"

func TestBelowFloor(t *testing.T) {
	cases := []struct {
		version, floor string
		want           bool
		wantErr        bool
	}{
		{"v4.2.2", helmFloor, false, false},
		{"v3.19.0", helmFloor, true, false},
		{"v4.0.0-rc.1", helmFloor, false, false}, // a pre-release of the floor reaches it
		{"4.2.2", helmFloor, false, false},       // defensive: no leading v
		{"v0.32.0", kindFloor, false, false},
		{"v0.31.0", kindFloor, false, false},
		{"v0.30.0", kindFloor, true, false},
		{"v0.32.0 go1.26.4 linux/amd64", kindFloor, false, false}, // the first word is the version
		{"", helmFloor, false, true},
		{"garbage", helmFloor, false, true},
	}
	for _, c := range cases {
		got, err := belowFloor(c.version, c.floor)
		if c.wantErr {
			if err == nil {
				t.Errorf("belowFloor(%q, %q): expected error, got %v", c.version, c.floor, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("belowFloor(%q, %q): %v", c.version, c.floor, err)
			continue
		}
		if got != c.want {
			t.Errorf("belowFloor(%q, %q) = %v, want %v", c.version, c.floor, got, c.want)
		}
	}
}
