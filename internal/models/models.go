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
// The entries below are the four backends a Gemini web account actually
// exposes, as reported by the otAQ7b model-discovery RPC:
//
//	id                category     number
//	56fdd199312815e2  4 Hydrogen     1
//	e6fa609c3fa255c0  Pro (3.1)      3
//	8c46e95b1a07cecc  Flash-Lite     6
//	95a4c0abcba24364  4 Carbon       8
//
// Header carries the backend id and is what actually routes a request: without
// it the upstream ignores Mode and silently answers with the default model.
// Earlier revisions listed names such as gemini-3.6-flash or gemini-auto that
// had no Header and no matching backend, so they all collapsed onto the same
// default model while appearing to be distinct.
//
// Aliases map several friendly names onto the same Header; they are genuinely
// the same backend and cost nothing extra.
var MODELS = map[string]Model{
	// 4 Carbon — the frontier tier, longest and most expensive output.
	"gemini-4-carbon": {
		Mode:   8,
		Think:  4,
		Header: "95a4c0abcba24364",
		Desc:   "Frontier model for complex tasks (Gemini 4 Carbon, requires a paid cookie)",
	},
	"gemini-4-argon": {
		Mode:   8,
		Think:  4,
		Header: "95a4c0abcba24364",
		Desc:   "Alias for gemini-4-carbon",
	},
	"carbon": {
		Mode:   8,
		Think:  4,
		Header: "95a4c0abcba24364",
		Desc:   "Alias for gemini-4-carbon",
	},
	"gemini-4": {
		Mode:   8,
		Think:  4,
		Header: "95a4c0abcba24364",
		Desc:   "Alias for gemini-4-carbon",
	},

	// 3.1 Pro — focused, concise answers.
	"gemini-3.1-pro": {
		Mode:   3,
		Think:  4,
		Header: "e6fa609c3fa255c0",
		Desc:   "Pro model with detailed research answers (requires a paid cookie)",
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
