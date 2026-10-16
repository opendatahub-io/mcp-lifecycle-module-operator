package main

import (
	"reflect"
	"testing"
)

func TestRunnerArgs(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want []string
	}{
		{nil, []string{"-test.timeout=15m"}},
		{[]string{"-test.run=^$"}, []string{"-test.timeout=15m", "-test.run=^$"}},
		{[]string{"exec", "-test.run=^$"}, []string{"exec", "-test.run=^$"}},
	} {
		if got := runnerArgs(tc.args); !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("runnerArgs(%q) = %q, want %q", tc.args, got, tc.want)
		}
	}
}
