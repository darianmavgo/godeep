package sh

import (
	"strings"
	"testing"
	"time"
)

func TestCoreLine(t *testing.T) {
	s := time.Second
	// over 1 s: core 0 idle 0 (busy), core 1 idle 0.5 s, core 2 idle all of it,
	// core 3 "idle" more than the frame (clamped to 0% busy)
	got := coreLine([]time.Duration{0, 0, 0, 0}, []time.Duration{0, s / 2, s, 2 * s}, s)
	for _, want := range []string{"cores: 2 busy (1.5 of 4)", "cpu0 100%", "cpu1  50%", "cpu2   0%", "cpu3   0%"} {
		if !strings.Contains(got, want) {
			t.Errorf("coreLine = %q, missing %q", got, want)
		}
	}
	if coreLine([]time.Duration{0}, []time.Duration{0, 0}, s) != "" || coreLine(nil, nil, 0) != "" {
		t.Error("mismatched or empty samples should give no line")
	}
}
