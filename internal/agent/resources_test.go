package agent

import "testing"

func TestAutoConcurrency(t *testing.T) {
	cases := []struct {
		available int64
		want      int
	}{
		{0, 8},
		{32 << 20, 4},
		{8 << 30, 64},
		{64 << 20, 4},
		{512 << 20, 16},
	}
	for _, tc := range cases {
		if got := autoConcurrency(tc.available); got != tc.want {
			t.Errorf("autoConcurrency(%d) = %d, want %d", tc.available, got, tc.want)
		}
	}
}

func TestAutoMemoryLimit(t *testing.T) {
	if got := autoMemoryLimit(0); got != 0 {
		t.Fatalf("expected no limit when memory is unknown, got %d", got)
	}
	if got, want := autoMemoryLimit(512<<20), int64(512<<20)*65/100; got != want {
		t.Fatalf("expected 65%% of 512 MiB (%d), got %d", want, got)
	}
	if got := autoMemoryLimit(40 << 20); got != 32<<20 {
		t.Fatalf("expected a 32 MiB floor, got %d", got)
	}
	if got := autoMemoryLimit(16 << 20); got != 16<<20 {
		t.Fatalf("limit must never exceed available memory, got %d", got)
	}
	if got := autoMemoryLimit(32 << 20); got > 32<<20 {
		t.Fatalf("limit must never exceed available memory, got %d", got)
	}
}
