package main

// RankingTrace is the separate, reason-coded record of how a ranked decision
// was made. It is NOT an audit or metric field: raw confidence and
// probabilities never reach audit.jsonl or the metric stream.
type RankingTrace struct {
	Strategy   string `json:"strategy"`
	Tier       string `json:"tier"`
	Comparable bool   `json:"comparable"`
	ScoreKind  string `json:"score_kind"`
	DecidedBy  string `json:"decided_by"`
	Winner     string `json:"winner"`
	RunnerUp   string `json:"runner_up"`
	Reason     string `json:"reason"`
}

// rankingTrace builds a trace for a typed SO answer. Scores compare only when
// both sides share a ScoreKind and are marked comparable; anything else fails
// closed with a reason instead of ranking incomparable values.
func rankingTrace(d soDecision, bucket string) RankingTrace {
	t := RankingTrace{
		Strategy:  "canonical",
		Tier:      "exact",
		ScoreKind: "confidence-bucket",
		DecidedBy: "systemone",
		Winner:    d.Choice,
		Reason:    bucket,
	}
	if len(d.Probabilities) < 2 {
		t.Comparable = false
		t.Reason = "insufficient-distribution"
		return t
	}
	t.Comparable = true
	t.RunnerUp = runnerUp(d.Probabilities, d.Choice)
	return t
}

// runnerUp returns the highest-scoring label other than the winner.
func runnerUp(scores map[string]float64, winner string) string {
	best, found := "", false
	for label := range scores {
		if label == winner {
			continue
		}
		if !found || scores[label] > scores[best] {
			best, found = label, true
		}
	}
	return best
}
