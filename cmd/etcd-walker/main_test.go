package main

import "testing"

// stringFlag/boolFlag track whether a flag was given on the command line, so
// "flag explicitly set to the default value" still overrides the config file.

func TestStringFlagTracksSet(t *testing.T) {
	var f stringFlag
	if f.set {
		t.Error("fresh flag must not be marked set")
	}
	if err := f.Set("host1"); err != nil {
		t.Fatal(err)
	}
	if !f.set || f.value != "host1" || f.String() != "host1" {
		t.Errorf("after Set: %+v", f)
	}

	// Explicit empty string still counts as set.
	var empty stringFlag
	if err := empty.Set(""); err != nil {
		t.Fatal(err)
	}
	if !empty.set {
		t.Error("explicit empty value must be marked set")
	}
}

func TestBoolFlagParsesAndTracksSet(t *testing.T) {
	cases := map[string]bool{
		"true":  true,
		"1":     true,
		"false": false,
		"0":     false,
	}
	for in, want := range cases {
		var f boolFlag
		if err := f.Set(in); err != nil {
			t.Errorf("Set(%q) errored: %v", in, err)
			continue
		}
		if !f.set || f.value != want {
			t.Errorf("Set(%q) = %+v, want value=%t set=true", in, f, want)
		}
	}

	var f boolFlag
	if err := f.Set("banana"); err == nil {
		t.Error("invalid bool must error")
	}
	if f.set {
		t.Error("failed parse must not mark the flag as set")
	}
	if f.String() != "false" {
		t.Errorf("zero boolFlag String() = %q, want false", f.String())
	}
}
