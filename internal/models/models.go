package models

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Model represents a Gemini backend routing configuration.
// Mode corresponds to the Gemini upstream mode ID, Think specifies the default reasoning depth,
// and Extra contains index-based overrides for Gemini's sparse RPC request array.
type Model struct {
	Mode   int         `json:"mode"`
	Think  int         `json:"think"`
	Desc   string      `json:"desc"`
	Header string      `json:"header,omitempty"`
	Extra  map[int]any `json:"extra,omitempty"`
}

// MODELS lists the model names clients may request.
//
// The Gemini web UI offers exactly four choices for a standard account. They
// come from two different places in the otAQ7b payload, which is why counting
// the discovery list alone is misleading:
//
//	UI selector entry                        backend id
//	6  3.5 Flash-Lite  "Quick answers"        8c46e95b1a07cecc
//	1  4 Hydrogen      "Everyday help"        56fdd199312815e2
//	3  3.1 Pro         "Detailed research"    e6fa609c3fa255c0
//	8  4 Argon         "Code & analysis"      95a4c0abcba24364
//
// The model list at payload[15] advertises "4 Carbon" (Carbon4p0) against the
// same backend id, but Carbon is gated by the carbon_multi_ultra_pro_policy
// entitlement and is not selectable on a normal account. Argon is the name such
// an account actually gets, so that is what is exposed here.
//
// Header carries the backend id and is what actually routes a request: without
// it upstream ignores Mode and silently answers with the default model. Earlier
// revisions listed names such as gemini-3.6-flash or gemini-auto that had no
// Header and no matching backend, so they all collapsed onto the same default
// while appearing to be distinct.
//
// Only the four canonical names are registered. Aliases were removed on
// purpose: /v1/models is a discovery endpoint, so it must list what an account
// can actually select in the web UI. Extra names made the list look like twelve
// models when there are four backends.
//
// A request naming anything else is rejected with 400 rather than quietly
// answered by a different model: a silent substitution makes the response
// untrustworthy and hides caller mistakes.
var MODELS = map[string]Model{
	// num 8 — 4 Argon, the code/analysis tier.
	"gemini-4-argon": {
		Mode:   8,
		Think:  4,
		Header: "95a4c0abcba24364",
		Desc:   "Frontier model for code and analysis (Gemini 4 Argon)",
	},

	// num 3 — 3.1 Pro, focused research answers.
	"gemini-3.1-pro": {
		Mode:   3,
		Think:  4,
		Header: "e6fa609c3fa255c0",
		Desc:   "Pro model with detailed research answers (Gemini 3.1 Pro)",
	},

	// num 1 — 4 Hydrogen, the default everyday tier.
	"gemini-4-hydrogen": {
		Mode:   1,
		Think:  4,
		Header: "56fdd199312815e2",
		Desc:   "Everyday general-purpose model (Gemini 4 Hydrogen)",
	},

	// num 6 — 3.5 Flash-Lite, fastest and cheapest.
	"gemini-3.5-flash-lite": {
		Mode:   6,
		Think:  4,
		Header: "8c46e95b1a07cecc",
		Desc:   "Lightweight fast model for quick answers (Gemini 3.5 Flash-Lite)",
	},
}

type Resolved struct {
	Name   string
	Mode   int
	Think  int
	Header string
	Extra  map[int]any
}

const DefaultModelName = "gemini-4-hydrogen"

// Resolve maps a requested model string to its backend configuration.
//
// A "@think=N" suffix (e.g. "gemini-4-argon@think=0") overrides the thinking
// depth without changing the model mapping.
//
// An empty model name resolves to DefaultModelName. Any other unknown name is an
// error: substituting a different model would silently change what the caller
// asked for.
func Resolve(modelName string) (Resolved, error) {
	if modelName == "" {
		modelName = DefaultModelName
	}

	var thinkOverride *int
	if idx := strings.LastIndex(modelName, "@think="); idx != -1 {
		thinkStr := modelName[idx+len("@think="):]
		modelName = modelName[:idx]
		val, err := strconv.Atoi(thinkStr)
		if err != nil {
			return Resolved{}, fmt.Errorf("invalid think level: %s", thinkStr)
		}
		thinkOverride = &val
	}

	cfg, exists := MODELS[modelName]
	if !exists {
		// No silent fallback: answering with a different model than the caller
		// asked for hides configuration mistakes and makes the response
		// untrustworthy. Reject the request so the error is visible.
		return Resolved{}, fmt.Errorf("model %q does not exist; available models: %s",
			modelName, AvailableModels())
	}

	thinkMode := cfg.Think
	if thinkOverride != nil {
		thinkMode = *thinkOverride
	}

	return Resolved{
		Name:   modelName,
		Mode:   cfg.Mode,
		Think:  thinkMode,
		Header: cfg.Header,
		Extra:  cfg.Extra,
	}, nil
}

// AvailableModels returns the registered model names, sorted, for error messages
// and the discovery endpoint.
func AvailableModels() string {
	names := make([]string, 0, len(MODELS))
	for name := range MODELS {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
