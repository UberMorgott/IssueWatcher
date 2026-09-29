package picker

import "testing"

func TestIsNetworkPath(t *testing.T) {
	old := driveType
	t.Cleanup(func() { driveType = old })
	driveType = func(root string) uint32 {
		if root == `Z:\` || root == `z:\` {
			return driveRemote
		}
		return 3 // DRIVE_FIXED
	}
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"", false},
		{`C:\Users\me\src`, false},
		{`c:/dev`, false},
		{`\\server\share\repo`, true},
		{`//server/share`, true},
		{`  \\nas\git  `, true},
		{`\\?\UNC\server\share`, true},
		{`\\?\C:\very\long`, false},
		{`\\.\C:\x`, false},
		{`Z:\mapped\repo`, true},
		{`z:`, true},
		{`\\?\Z:\mapped`, true},
		{`relative\dir`, false},
		{`1:\odd`, false},
	} {
		if got := IsNetworkPath(tc.path); got != tc.want {
			t.Errorf("IsNetworkPath(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}
