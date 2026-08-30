package server

import "testing"

func TestHumanizeInt(t *testing.T) {
	tests := []struct {
		name string
		n    int64
		want string
	}{
		{"zero", 0, "0"},
		{"small negative, no comma", -42, "-42"},
		{"under 1000, no comma", 999, "999"},
		{"exactly 1000", 1000, "1,000"},
		{"large multi-comma value", 35807303, "35,807,303"},
		{"negative large value", -1234567, "-1,234,567"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := humanizeInt(tt.n); got != tt.want {
				t.Errorf("humanizeInt(%d) = %q, want %q", tt.n, got, tt.want)
			}
		})
	}
}

func TestHumanizeFloat(t *testing.T) {
	tests := []struct {
		name     string
		v        float64
		decimals int
		want     string
	}{
		{"zero", 0, 2, "0.00"},
		{"under 10000, comma applied", 1234.5, 2, "1,234.50"},
		{"rounds up into new group", 999.999, 2, "1,000.00"},
		{"large value with decimals", 35807303.4, 2, "35,807,303.40"},
		{"negative float", -1234.5, 2, "-1,234.50"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := humanizeFloat(tt.v, tt.decimals); got != tt.want {
				t.Errorf("humanizeFloat(%v, %d) = %q, want %q", tt.v, tt.decimals, got, tt.want)
			}
		})
	}
}
