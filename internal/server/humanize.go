package server

import "strconv"

// humanizeInt returns n formatted with comma thousands-separators on the integer
// value, e.g. humanizeInt(35807303) == "35,807,303". Negative numbers keep their
// leading "-" outside the grouped digits, e.g. humanizeInt(-1234) == "-1,234".
func humanizeInt(n int64) string {
	return groupDigits(strconv.FormatInt(n, 10))
}

// humanizeFloat returns v formatted with `decimals` digits after the decimal point
// (same rounding behavior as strconv.FormatFloat(v, 'f', decimals, 64)) and
// comma thousands-separators applied to the integer part only - the decimal part is
// untouched. E.g. humanizeFloat(1234567.5, 2) == "1,234,567.50".
func humanizeFloat(v float64, decimals int) string {
	s := strconv.FormatFloat(v, 'f', decimals, 64)
	intPart, decPart, hasDec := cutLastDot(s)
	intPart = groupDigits(intPart)
	if hasDec {
		return intPart + "." + decPart
	}
	return intPart
}

// cutLastDot splits s on its last "." into (before, after, true), or (s, "", false)
// if s has no ".".
func cutLastDot(s string) (string, string, bool) {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '.' {
			return s[:i], s[i+1:], true
		}
	}
	return s, "", false
}

// groupDigits inserts comma thousands-separators into a plain decimal digit string,
// preserving a leading "-" sign untouched (not counted in the grouping).
func groupDigits(digits string) string {
	neg := false
	if len(digits) > 0 && digits[0] == '-' {
		neg = true
		digits = digits[1:]
	}

	n := len(digits)
	if n <= 3 {
		if neg {
			return "-" + digits
		}
		return digits
	}

	// Number of comma-separated groups after the first (possibly short) group.
	extraGroups := (n - 1) / 3
	out := make([]byte, n+extraGroups)
	oi := len(out) - 1
	for i, di := n-1, 0; i >= 0; i, di = i-1, di+1 {
		if di > 0 && di%3 == 0 {
			out[oi] = ','
			oi--
		}
		out[oi] = digits[i]
		oi--
	}

	if neg {
		return "-" + string(out)
	}
	return string(out)
}
