package secret

import (
	"errors"
	"path/filepath"
	"testing"
)

type sample struct {
	A string `json:"a"`
	B int    `json:"b"`
}

func TestRoundTripAndRemove(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets", "x.json")
	var got sample
	if err := ReadJSON(path, &got); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing file: err %v, want ErrNotFound", err)
	}
	if err := WriteJSON(path, sample{A: "v", B: 2}); err != nil {
		t.Fatal(err)
	}
	if err := WriteJSON(path, sample{A: "w", B: 3}); err != nil { // overwrite
		t.Fatal(err)
	}
	if err := ReadJSON(path, &got); err != nil || got != (sample{A: "w", B: 3}) {
		t.Fatalf("read back %+v err %v", got, err)
	}
	if err := Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := Remove(path); err != nil {
		t.Fatalf("second remove: %v", err)
	}
}
