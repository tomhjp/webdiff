package main

import "testing"

// A restart size is used by the next start of the session only.
func TestRestartSize(t *testing.T) {
	want := rexTermSize{
		Columns: 120,
		Rows:    40,
	}
	setRestartSize("a-wd", want)
	if _, ok := takeRestartSize("b-wd"); ok {
		t.Error("got a size for another session")
	}
	if got, ok := takeRestartSize("a-wd"); !ok || got != want {
		t.Errorf("takeRestartSize = %+v, %v; want %+v", got, ok, want)
	}
	if _, ok := takeRestartSize("a-wd"); ok {
		t.Error("size used twice")
	}
}
