package models

import (
	"fmt"
	"log"
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
// Aliases map several friendly names onto the same Header; they are genuinely
// the same backend and cost nothing extra.
var MODELS = map[string]Model{
	// num 8 — 4 Argon, the code/analysis tier.
	"gemini-4-argon": {
		Mode:   8,
		Think:  4,
		Header: "95a4c0abcba24364",
		Desc:   "Frontier model for code and analysis (Gemini 4 Argon)",
	},
	"argon": {
		Mode:   8,
		Think:  4,
		Header: "95a4c0abcba24364",
		Desc:   "Alias for gemini-4-argon",
	},
	"gemini-4": {
		Mode:   8,
		Think:  4,
		Header: "95a4c0abcba24364",
		Desc:   "Alias for gemini-4-argon",
	},

	// num 3 — 3.1 Pro, focused research answers.
	"gemini-3.1-pro": {
		Mode:   3,
		Think:  4,
		Header: "e6fa609c3fa255c0",
		Desc:   "Pro model with detailed research answers",
	},
	"gemini-3.1-pro-enhanced": {
		Mode:   3,
		Think:  4,
		Header: "e6fa609c3fa255c0",
		Desc:   "Alias for gemini-3.1-pro",
		Extra:  map[int]any{31: 2, 80: 3},
	},
	"pro": {
		Mode:   3,
		Think:  4,
		Header: "e6fa609c3fa255c0",
		Desc:   "Alias for gemini-3.1-pro",
	},

	// 4 Hydrogen — the default everyday tier.
	"gemini-4-hydrogen": {
		Mode:   1,
		Think:  4,
		Header: "56fdd199312815e2",
		Desc:   "Everyday general-purpose model (Gemini 4 Hydrogen)",
	},
	"hydrogen": {
		Mode:   1,
		Think:  4,
		Header: "56fdd199312815e2",
		Desc:   "Alias for gemini-4-hydrogen",
	},
	"gemini-3.5-flash": {
		Mode:   1,
		Think:  4,
		Header: "56fdd199312815e2",
		Desc:   "Alias for gemini-4-hydrogen",
	},

	// 3.5 Flash-Lite — fastest, cheapest.
	"gemini-3.5-flash-lite": {
		Mode:   6,
		Think:  4,
		Header: "8c46e95b1a07cecc",
		Desc:   "Lightweight fast model for quick answers (3.5 Flash-Lite)",
	},
	"gemini-flash-lite": {
		Mode:   6,
		Think:  4,
		Header: "8c46e95b1a07cecc",
		Desc:   "Alias for gemini-3.5-flash-lite",
	},
	"flash-lite": {
		Mode:   6,
		Think:  4,
		Header: "8c46e95b1a07cecc",
		Desc:   "Alias for gemini-3.5-flash-lite",
	},
}

type Resolved struct {
	Name     string
	Mode     int
	Think    int
	Header   string
	Fallback bool
	Extra    map[int]any
}

const DefaultModelName = "gemini-4-hydrogen"

// Resolve maps a requested model string to its corresponding backend configuration.
// It supports model name suffixes like "@think=N" (e.g. "gemini-3.6-flash@think=0")
// to dynamically override the model's default thinking depth without modifying the model mapping.
func Resolve(modelName, defaultName string) (Resolved, error) {
	if defaultName == "" {
		defaultName = DefaultModelName
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
	fallback := false
	if !exists {
		log.Printf("Unknown model '%s', falling back to '%s'", modelName, defaultName)
		modelName = defaultName
		cfg = MODELS[defaultName]
		fallback = true
	}

	thinkMode := cfg.Think
	if thinkOverride != nil {
		thinkMode = *thinkOverride
	}

	return Resolved{
		Name:     modelName,
		Mode:     cfg.Mode,
		Think:    thinkMode,
		Header:   cfg.Header,
		Fallback: fallback,
		Extra:    cfg.Extra,
	}, nil
}
