package updater

import "testing"

func TestIsReleaseVersion(t *testing.T) {
	valid := []string{"v1.7.3", "v1.10.0", "v2.0.0-rc.1"}
	for _, v := range valid {
		if !isReleaseVersion(v) {
			t.Errorf("isReleaseVersion(%q) = false, want true", v)
		}
	}
	invalid := []string{"", "1.7.3", "abc123", "dirty", "deadbeef-dirty"}
	for _, v := range invalid {
		if isReleaseVersion(v) {
			t.Errorf("isReleaseVersion(%q) = true, want false", v)
		}
	}
}

func TestIsNewer(t *testing.T) {
	cases := []struct {
		cur, target string
		want        bool
	}{
		{"v1.7.3", "v1.8.0", true},
		{"v1.9.0", "v1.10.0", true}, // multi-digit ordering
		{"v1.8.0", "v1.8.0", false},
		{"v1.8.0", "v1.7.2", false}, // downgrade is not "newer"
	}
	for _, c := range cases {
		if got := isNewer(c.cur, c.target); got != c.want {
			t.Errorf("isNewer(%q,%q) = %v, want %v", c.cur, c.target, got, c.want)
		}
	}
}
