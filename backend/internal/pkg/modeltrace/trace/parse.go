package trace

import (
	"math"
	"unicode"
	"unicode/utf8"
)

const (
	valueMin = 1
	valueMax = 355
	minRatio = 0.55 // fixed setting, same constant as pipeline/parsing.py (not read from the bank,
	// so the rounding matches Python exactly)
)

// ParseNumbers returns the longest run of integers in [1, 355] in text. ASCII digits only;
// a letter between two numbers starts a new run (mirrors pipeline/parsing.py).
func ParseNumbers(text string) []int {
	var best, current []int
	letterSince := false
	for i := 0; i < len(text); {
		c := text[i]
		if c >= '0' && c <= '9' {
			j, value := i, 0
			for j < len(text) && text[j] >= '0' && text[j] <= '9' {
				if value <= 1_000_000 { // larger values are out of range anyway
					value = value*10 + int(text[j]-'0')
				}
				j++
			}
			if len(current) > 0 && letterSince {
				if len(current) > len(best) {
					best = current
				}
				current = nil
			}
			if value >= valueMin && value <= valueMax {
				current = append(current, value)
			}
			letterSince = false
			i = j
			continue
		}
		r, size := utf8.DecodeRuneInString(text[i:])
		if unicode.IsLetter(r) {
			letterSince = true
		}
		i += size
	}
	if len(current) > len(best) {
		best = current
	}
	return best
}

// MinimumNumbers is how many numbers a reply needs to count: max(80, ceil(0.55 * expected)).
func (b *Bank) MinimumNumbers(expected int) int {
	if expected <= 0 {
		return b.minNumbers
	}
	need := int(math.Ceil(float64(expected) * minRatio))
	if need < b.minNumbers {
		return b.minNumbers
	}
	return need
}
