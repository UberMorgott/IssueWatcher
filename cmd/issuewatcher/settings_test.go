package main

import "testing"

func TestStartMinimizedFlagWins(t *testing.T) {
	cases := []struct {
		args    []string
		setting bool
		want    bool
	}{
		{nil, false, false},
		{nil, true, true},
		{[]string{"--minimized"}, false, true},
		{[]string{"-minimized"}, false, true},
		{[]string{"/Minimized"}, false, true},
		{[]string{"--minimized=true"}, false, true},
		{[]string{"--minimized=false"}, true, false},
		{[]string{"--other"}, true, true},
	}
	for _, c := range cases {
		if got := startMinimized(c.args, c.setting); got != c.want {
			t.Errorf("startMinimized(%v, %v) = %v, want %v", c.args, c.setting, got, c.want)
		}
	}
}
