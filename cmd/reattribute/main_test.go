package main

import (
	"testing"

	"github.com/Snipa22/go-tari-explorer/internal/poolattr"
)

func strPtr(s string) *string { return &s }

func TestPoolTagChanged(t *testing.T) {
	tests := []struct {
		name string
		old  *string
		new  *string
		want bool
	}{
		{name: "nil vs nil", old: nil, new: nil, want: false},
		{name: "nil vs value", old: nil, new: strPtr("Jagtech"), want: true},
		{name: "value vs nil", old: strPtr("Jagtech"), new: nil, want: true},
		{name: "value vs same value", old: strPtr("Jagtech"), new: strPtr("Jagtech"), want: false},
		{name: "value vs different value", old: strPtr("Jagtech"), new: strPtr("OtherPool"), want: true},
		{name: "empty string vs empty string", old: strPtr(""), new: strPtr(""), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := poolTagChanged(tt.old, tt.new)
			if got != tt.want {
				t.Errorf("poolTagChanged(%v, %v) = %v, want %v", tt.old, tt.new, got, tt.want)
			}
		})
	}
}

func TestPoolTagFor(t *testing.T) {
	tests := []struct {
		name    string
		poolTag string
		want    *string
	}{
		{name: "empty pool tag means unattributed", poolTag: "", want: nil},
		{name: "non-empty pool tag becomes pointer", poolTag: "Jagtech", want: strPtr("Jagtech")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := poolTagFor(poolattr.BlockAttribution{PoolTag: tt.poolTag})
			if (got == nil) != (tt.want == nil) {
				t.Fatalf("poolTagFor(%q) = %v, want %v", tt.poolTag, got, tt.want)
			}
			if got != nil && *got != *tt.want {
				t.Errorf("poolTagFor(%q) = %q, want %q", tt.poolTag, *got, *tt.want)
			}
		})
	}
}

func TestFormatPoolTag(t *testing.T) {
	tests := []struct {
		name string
		tag  *string
		want string
	}{
		{name: "nil", tag: nil, want: "<nil>"},
		{name: "non-nil", tag: strPtr("Jagtech"), want: `"Jagtech"`},
		{name: "empty string is not nil", tag: strPtr(""), want: `""`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatPoolTag(tt.tag)
			if got != tt.want {
				t.Errorf("formatPoolTag(%v) = %q, want %q", tt.tag, got, tt.want)
			}
		})
	}
}
