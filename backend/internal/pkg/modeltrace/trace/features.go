package trace

import "math"

const (
	orderedChunks = 4
	orderedBins   = 16
	orderedDim    = orderedChunks*orderedBins + 10
)

// hellingerFeature: square root of the smoothed distribution of values 1..355.
func hellingerFeature(numbers []int, alpha float64) []float64 {
	out := make([]float64, valueMax)
	for _, n := range numbers {
		out[n-valueMin]++
	}
	total := 0.0
	for i := range out {
		out[i] += alpha
		total += out[i]
	}
	for i := range out {
		out[i] = math.Sqrt(out[i] / total)
	}
	return out
}

// orderedFeature: 16-bin value histograms for 4 position chunks plus the last-digit distribution.
// Chunks follow numpy.array_split: the first len%4 chunks get one extra item.
func orderedFeature(numbers []int) []float64 {
	out := make([]float64, 0, orderedDim)
	n := len(numbers)
	start := 0
	for c := 0; c < orderedChunks; c++ {
		size := n / orderedChunks
		if c < n%orderedChunks {
			size++
		}
		var bins [orderedBins]float64
		for _, v := range numbers[start : start+size] {
			bins[int(float64(v-valueMin)*orderedBins/float64(valueMax))]++
		}
		start += size
		out = appendSmoothedSqrt(out, bins[:])
	}
	var digits [10]float64
	for _, v := range numbers {
		digits[v%10]++
	}
	return appendSmoothedSqrt(out, digits[:])
}

func appendSmoothedSqrt(out, counts []float64) []float64 {
	total := 0.0
	for _, c := range counts {
		total += c + 0.5
	}
	for _, c := range counts {
		out = append(out, math.Sqrt((c+0.5)/total))
	}
	return out
}
