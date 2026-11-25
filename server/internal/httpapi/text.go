package httpapi

import "unicode/utf8"

// truncateRunes caps s at n characters, not n bytes.
//
// len(s) and s[:n] are byte operations. On a UTF-8 string they happily cut a
// multi-byte rune in half and hand back an invalid byte sequence — and Postgres
// refuses such a value outright ("invalid byte sequence for encoding UTF8")
// rather than storing a mangled one. That turned a defensive length cap into a
// 500: a shop sending a long Chinese label lost the order, while the same label
// in ASCII went through.
//
// Every column these caps guard is TEXT, so the cap is not doing schema work —
// it exists to bound an absurd payload. Counting it the way a human counts is
// both correct and what the callers already assumed.
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	// range over a string yields the byte offset of each rune's first byte, so
	// the first offset past the nth rune is exactly the cut point.
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}
