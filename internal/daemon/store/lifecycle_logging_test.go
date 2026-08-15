package store

import "testing"

func TestLifecycleReason(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{name: "source", in: "session_collector", want: "session_collector"},
		{name: "empty", in: "", want: "unknown"},
		{name: "whitespace", in: "  ", want: "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := lifecycleReason(tc.in); got != tc.want {
				t.Fatalf("lifecycleReason(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
