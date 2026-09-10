package server

import "testing"

// TestFormatMicroMinotari covers formatMicroMinotari's existing behavior (trailing-zero
// trim, "X.0" floor for a whole-number result, " XTM" suffix) plus its comma
// thousands-grouping of the integer part, added for consistency with humanizeFloat's
// grouping convention (internal/server/humanize.go).
func TestFormatMicroMinotari(t *testing.T) {
	tests := []struct {
		name          string
		microMinotari uint64
		want          string
	}{
		{"zero floors to X.0", 0, "0.0 XTM"},
		{"whole number floors to X.0", 5_000_000, "5.0 XTM"},
		{"trailing zeros trimmed", 1_500_000, "1.5 XTM"},
		{"sub-XTM value, no leading group needed", 123, "0.000123 XTM"},
		{"large value gets comma-grouped integer part", 2_635_444_672_596, "2,635,444.672596 XTM"},
		{"large whole-number value still comma-grouped and floored", 6_467_830_000_000, "6,467,830.0 XTM"},
		{"exactly 1000 XTM, comma boundary", 1_000_000_000, "1,000.0 XTM"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatMicroMinotari(tt.microMinotari); got != tt.want {
				t.Errorf("formatMicroMinotari(%d) = %q, want %q", tt.microMinotari, got, tt.want)
			}
		})
	}
}
