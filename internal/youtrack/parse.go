package youtrack

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// YouTrack telemetry returns most values as human formatted strings ("5.0 MB", "93%", "0.25"), the helpers below
// turn them into plain numbers. Both "." and "," are accepted as the decimal separator.

var numberRe = regexp.MustCompile(`[-+]?[0-9][0-9.,]*`)

// byteUnits are binary multiples: YouTrack formats sizes with 1024-based units.
var byteUnits = map[string]float64{
	"": 1, "b": 1, "byte": 1, "bytes": 1,
	"k": 1 << 10, "kb": 1 << 10, "kib": 1 << 10,
	"m": 1 << 20, "mb": 1 << 20, "mib": 1 << 20,
	"g": 1 << 30, "gb": 1 << 30, "gib": 1 << 30,
	"t": 1 << 40, "tb": 1 << 40, "tib": 1 << 40,
	"p": 1 << 50, "pb": 1 << 50, "pib": 1 << 50,
}

// ParseNumber returns the first number in s, e.g. 0.25 for "0.25" or 1.5 for "1,5/s".
func ParseNumber(s string) (float64, error) {
	v, _, err := splitNumber(s)
	return v, err
}

// ParseBytes converts a size such as "5.0 MB" or "512 bytes" to bytes.
func ParseBytes(s string) (float64, error) {
	v, rest, err := splitNumber(s)
	if err != nil {
		return 0, err
	}
	unit := rest
	if i := strings.IndexFunc(rest, func(r rune) bool { return !unicode.IsLetter(r) }); i >= 0 {
		unit = rest[:i]
	}
	mult, ok := byteUnits[strings.ToLower(unit)]
	if !ok {
		return 0, fmt.Errorf("unknown size unit in %q", s)
	}
	return v * mult, nil
}

// ParseRatio converts a percentage ("93.5%", "93.5") or a ratio ("0.935") to a 0..1 ratio.
func ParseRatio(s string) (float64, error) {
	v, rest, err := splitNumber(s)
	if err != nil {
		return 0, err
	}
	if strings.HasPrefix(rest, "%") || v > 1 {
		return v / 100, nil
	}
	return v, nil
}

// splitNumber returns the first number in s and the trimmed text that follows it.
func splitNumber(s string) (float64, string, error) {
	loc := numberRe.FindStringIndex(s)
	if loc == nil {
		return 0, "", fmt.Errorf("no number in %q", s)
	}
	num := normalizeNumber(strings.TrimRight(s[loc[0]:loc[1]], ".,"))
	v, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return 0, "", fmt.Errorf("parse %q: %w", s, err)
	}
	return v, strings.TrimSpace(s[loc[1]:]), nil
}

// normalizeNumber turns "1,234.5", "1.234,5" and "1,5" into a strconv.ParseFloat compatible form. A single comma
// without a dot is a decimal separator, repeated separators of one kind are thousands separators.
func normalizeNumber(n string) string {
	dot, comma := strings.LastIndex(n, "."), strings.LastIndex(n, ",")
	switch {
	case dot >= 0 && comma >= 0:
		if comma > dot {
			return strings.Replace(strings.ReplaceAll(n, ".", ""), ",", ".", 1)
		}
		return strings.ReplaceAll(n, ",", "")
	case comma >= 0:
		if strings.Count(n, ",") == 1 {
			return strings.Replace(n, ",", ".", 1)
		}
		return strings.ReplaceAll(n, ",", "")
	case strings.Count(n, ".") > 1:
		return strings.ReplaceAll(n, ".", "")
	}
	return n
}
