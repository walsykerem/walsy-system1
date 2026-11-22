package main

import (
	"encoding/json"
	"strconv"
)

// relayLog records one relayed JSON-RPC frame in audit.jsonl. Only the method
// name, direction, status and frame size are kept: params and results can hold
// user data and keys, so they never reach disk.
func relayLog(phase string, raw []byte) {
	f, decodeErr := decodeFrame(raw)
	method := f.Method
	if method == "" {
		method = "response"
	}
	status := "ok"
	if decodeErr != nil {
		status = "malformed"
	} else if f.Method == "" && len(f.Result) == 0 {
		status = relayErrorStatus(raw, status)
	}
	routerAudit("relay_frame", "phase", phase, "mode", method, "status", status, "count", strconv.Itoa(len(raw)))
}

// relayErrorStatus maps a JSON-RPC error response to a bounded status label.
func relayErrorStatus(raw []byte, fallback string) string {
	var env struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(raw, &env) != nil || len(env.Error) == 0 {
		return fallback
	}
	var e struct {
		Code int `json:"code"`
	}
	if json.Unmarshal(env.Error, &e) != nil {
		return "error_unknown"
	}
	return "error_" + strconv.Itoa(e.Code)
}
