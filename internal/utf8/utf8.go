package utf8

import stdutf8 "unicode/utf8"

// Truncate cuts s to at most n bytes without splitting a UTF-8 sequence.
func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !stdutf8.RuneStart(s[n]) {
		n--
	}

	return s[:n]
}
