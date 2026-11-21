package main

import (
	"math"
	"sort"
)

// defaultThreshold is the cold-start fallback when calibration data is absent.
func defaultThreshold() float64 { return 0.8 }

// calibratedConfidence pins a parse failure to the strong-model value: absence
// of a confidence is not evidence of a low one.
func calibratedConfidence(parsed float64, parseOK bool) float64 {
	if !parseOK || math.IsNaN(parsed) {
		return 1
	}
	return parsed
}

// ece is expected calibration error over 10 equal-width confidence bins.
func ece(scores, labels []float64) float64 {
	n := len(scores)
	if n == 0 || n != len(labels) {
		return 0
	}
	const bins = 10
	var total float64
	for b := 0; b < bins; b++ {
		lo, hi := float64(b)/bins, float64(b+1)/bins
		var s, l, count float64
		for i := range scores {
			in := scores[i] >= lo && scores[i] < hi
			if b == bins-1 && scores[i] == hi {
				in = true
			}
			if !in {
				continue
			}
			s += scores[i]
			l += labels[i]
			count++
		}
		if count > 0 {
			total += count / float64(n) * math.Abs(l/count-s/count)
		}
	}
	return total
}

// isotonicFit is pool-adjacent-violators, sorted by score; returned values
// align with the input order.
func isotonicFit(scores, labels []float64) []float64 {
	n := len(scores)
	fitted := make([]float64, n)
	if n == 0 {
		return fitted
	}
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(i, j int) bool { return scores[order[i]] < scores[order[j]] })

	sums := make([]float64, 0, n)
	counts := make([]int, 0, n)
	for _, k := range order {
		sums = append(sums, labels[k])
		counts = append(counts, 1)
		for len(sums) > 1 {
			a := len(sums) - 2
			if sums[a]/float64(counts[a]) <= sums[a+1]/float64(counts[a+1]) {
				break
			}
			sums[a] += sums[a+1]
			counts[a] += counts[a+1]
			sums = sums[:a+1]
			counts = counts[:a+1]
		}
	}
	pos := 0
	for b, c := range counts {
		v := sums[b] / float64(c)
		for j := 0; j < c; j++ {
			fitted[order[pos]] = v
			pos++
		}
	}
	return fitted
}

// quantileThreshold admits the top strongPct of scores: threshold is the
// (1-strongPct) nearest-rank quantile.
func quantileThreshold(scores []float64, strongPct float64) float64 {
	if len(scores) == 0 {
		return defaultThreshold()
	}
	q := 1 - strongPct
	if q < 0 {
		q = 0
	}
	if q > 1 {
		q = 1
	}
	sorted := append([]float64(nil), scores...)
	sort.Float64s(sorted)
	idx := int(math.Ceil(q*float64(len(sorted))-1e-9)) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// rZero is the mean height under the lower convex hull of a monotone cost
// curve (AIQ baseline). Insufficient data falls back to the cold threshold.
func rZero(costs []float64) float64 {
	if len(costs) < 2 {
		return defaultThreshold()
	}
	type point struct{ x, y float64 }
	pts := make([]point, len(costs))
	for i, c := range costs {
		pts[i] = point{float64(i), c}
	}
	hull := make([]point, 0, len(pts))
	for _, p := range pts {
		for len(hull) >= 2 {
			a, b := hull[len(hull)-2], hull[len(hull)-1]
			cross := (b.x-a.x)*(p.y-a.y) - (b.y-a.y)*(p.x-a.x)
			if cross > 0 {
				break
			}
			hull = hull[:len(hull)-1]
		}
		hull = append(hull, p)
	}
	var area float64
	for i := 1; i < len(hull); i++ {
		area += (hull[i].x - hull[i-1].x) * (hull[i].y + hull[i-1].y) / 2
	}
	return area / (float64(len(costs)) - 1)
}
