package youtrack

import "testing"

func TestParse(t *testing.T) {
	tests := []struct {
		name  string
		parse func(string) (float64, error)
		in    string
		want  float64
	}{
		{"number", ParseNumber, "0.25", 0.25},
		{"number with comma", ParseNumber, "1,5", 1.5},
		{"number with unit", ParseNumber, "12.5/s", 12.5},
		{"number thousands", ParseNumber, "1,234.5", 1234.5},
		{"number thousands european", ParseNumber, "1.234,5", 1234.5},
		{"number integer", ParseNumber, "1610613083628", 1610613083628},
		{"bytes MB", ParseBytes, "5.0 MB", 5 << 20},
		{"bytes GB", ParseBytes, "1.5 GB", 1.5 * (1 << 30)},
		{"bytes KB no space", ParseBytes, "512KB", 512 << 10},
		{"bytes plain", ParseBytes, "100 bytes", 100},
		{"bytes no unit", ParseBytes, "100", 100},
		{"bytes comma", ParseBytes, "0,1 MB", 0.1 * (1 << 20)},
		{"ratio percent", ParseRatio, "93.5%", 0.935},
		{"ratio percent space", ParseRatio, "40 %", 0.4},
		{"ratio as percent number", ParseRatio, "93.5", 0.935},
		{"ratio fraction", ParseRatio, "0.5", 0.5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.parse(tt.in)
			if err != nil {
				t.Fatalf("parse(%q): %v", tt.in, err)
			}
			if diff := got - tt.want; diff > 1e-9 || diff < -1e-9 {
				t.Errorf("parse(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseErrors(t *testing.T) {
	for _, in := range []string{"", "n/a", "unknown"} {
		if _, err := ParseNumber(in); err == nil {
			t.Errorf("ParseNumber(%q): expected an error", in)
		}
	}
	if _, err := ParseBytes("5 parsecs"); err == nil {
		t.Error("ParseBytes with an unknown unit: expected an error")
	}
}
