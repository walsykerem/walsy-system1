package app

import "encoding/json"

// soTemplateState is the only context channel on the System One wire: no
// conversation history, tools or credential live outside state.
type soTemplateState struct {
	Intent      string   `json:"intent"`
	Constraints []string `json:"constraints"`
	Evidence    []string `json:"evidence"`
	WorkDone    []string `json:"work_done"`
}

// Criteria is map[string]string for choice and []soScoreCriterion for score;
// score is an array on the wire, choice an object.
type soScoreCriterion struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

type soTemplateQuestion struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions"`
	Criteria     any    `json:"criteria"`
}

// soRequestTemplate returns the ready-to-POST System One decision body. The
// credential stays in the x-api-key header and cannot enter this body.
func soRequestTemplate(model string, state soTemplateState, questions map[string]soTemplateQuestion) ([]byte, error) {
	// The System One envelope carries state as a string, so the structured
	// state is JSON-encoded into it.
	stateJSON, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		Model     string                        `json:"model"`
		State     string                        `json:"state"`
		Questions map[string]soTemplateQuestion `json:"questions"`
	}{Model: model, State: string(stateJSON), Questions: questions})
}
