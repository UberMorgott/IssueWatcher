package main

import "testing"

func TestParseLaunch(t *testing.T) {
	l := parseLaunch([]string{"--minimized", "--after-update=1234"})
	if l.afterUpdate != 1234 || l.rolledBack != 0 || !l.quiet() {
		t.Fatalf("after update: %+v", l)
	}
	l = parseLaunch([]string{"--rolled-back=77"})
	if l.rolledBack != 77 || l.afterUpdate != 0 || l.quiet() {
		t.Fatalf("rolled back: %+v", l)
	}
	if l := parseLaunch([]string{"--after-update=x", "--after-update=-3"}); l.afterUpdate != 0 {
		t.Fatalf("bad pid accepted: %+v", l)
	}
}
