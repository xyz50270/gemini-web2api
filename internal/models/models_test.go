package models

import (
	"testing"
)

func TestResolve(t *testing.T) {
	tests := []struct {
		input        string
		defaultName  string
		wantName     string
		wantMode     int
		wantThink    int
		wantHeader   string
		wantFallback bool
		wantErr      bool
	}{
		{
			input:        "gemini-4-hydrogen",
			defaultName:  "gemini-4-hydrogen",
			wantName:     "gemini-4-hydrogen",
			wantMode:     1,
			wantThink:    4,
			wantHeader:   "56fdd199312815e2",
			wantFallback: false,
		},
		{
			input:        "gemini-3.5-flash",
			defaultName:  "gemini-4-hydrogen",
			wantName:     "gemini-3.5-flash",
			wantMode:     1,
			wantThink:    4,
			wantHeader:   "56fdd199312815e2",
			wantFallback: false,
		},
		{
			input:        "gemini-4-carbon",
			defaultName:  "gemini-4-hydrogen",
			wantName:     "gemini-4-carbon",
			wantMode:     8,
			wantThink:    4,
			wantHeader:   "95a4c0abcba24364",
			wantFallback: false,
		},
		{
			input:        "carbon",
			defaultName:  "gemini-4-hydrogen",
			wantName:     "carbon",
			wantMode:     8,
			wantThink:    4,
			wantHeader:   "95a4c0abcba24364",
			wantFallback: false,
		},
		{
			input:        "gemini-4-argon",
			defaultName:  "gemini-4-hydrogen",
			wantName:     "gemini-4-argon",
			wantMode:     8,
			wantThink:    4,
			wantHeader:   "95a4c0abcba24364",
			wantFallback: false,
		},
		{
			input:        "gemini-4",
			defaultName:  "gemini-4-hydrogen",
			wantName:     "gemini-4",
			wantMode:     8,
			wantThink:    4,
			wantHeader:   "95a4c0abcba24364",
			wantFallback: false,
		},
		{
			input:        "pro",
			defaultName:  "gemini-4-hydrogen",
			wantName:     "pro",
			wantMode:     3,
			wantThink:    4,
			wantHeader:   "e6fa609c3fa255c0",
			wantFallback: false,
		},
		{
			input:        "gemini-3.1-pro",
			defaultName:  "gemini-4-hydrogen",
			wantName:     "gemini-3.1-pro",
			wantMode:     3,
			wantThink:    4,
			wantHeader:   "e6fa609c3fa255c0",
			wantFallback: false,
		},
		{
			input:        "flash-lite",
			defaultName:  "gemini-4-hydrogen",
			wantName:     "flash-lite",
			wantMode:     6,
			wantThink:    4,
			wantHeader:   "8c46e95b1a07cecc",
			wantFallback: false,
		},
		{
			input:        "carbon@think=0",
			defaultName:  "gemini-4-hydrogen",
			wantName:     "carbon",
			wantMode:     8,
			wantThink:    0,
			wantHeader:   "95a4c0abcba24364",
			wantFallback: false,
		},
		{
			input:        "gemini-4-hydrogen@think=0",
			defaultName:  "gemini-4-hydrogen",
			wantName:     "gemini-4-hydrogen",
			wantMode:     1,
			wantThink:    0,
			wantHeader:   "56fdd199312815e2",
			wantFallback: false,
		},
		{
			input:        "unknown-model",
			defaultName:  "gemini-4-hydrogen",
			wantName:     "gemini-4-hydrogen",
			wantMode:     1,
			wantThink:    4,
			wantHeader:   "56fdd199312815e2",
			wantFallback: true,
		},
		{
			// Removed names must not resurrect a silent fallback to the default.
			input:        "gemini-auto",
			defaultName:  "gemini-4-hydrogen",
			wantName:     "gemini-4-hydrogen",
			wantMode:     1,
			wantThink:    4,
			wantHeader:   "56fdd199312815e2",
			wantFallback: true,
		},
		{
			input:       "gemini-4-hydrogen@think=abc",
			defaultName: "gemini-4-hydrogen",
			wantErr:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			res, err := Resolve(tt.input, tt.defaultName)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Resolve(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if res.Name != tt.wantName {
				t.Errorf("res.Name = %q, want %q", res.Name, tt.wantName)
			}
			if res.Mode != tt.wantMode {
				t.Errorf("res.Mode = %d, want %d", res.Mode, tt.wantMode)
			}
			if res.Think != tt.wantThink {
				t.Errorf("res.Think = %d, want %d", res.Think, tt.wantThink)
			}
			if res.Header != tt.wantHeader {
				t.Errorf("res.Header = %q, want %q", res.Header, tt.wantHeader)
			}
			if res.Fallback != tt.wantFallback {
				t.Errorf("res.Fallback = %v, want %v", res.Fallback, tt.wantFallback)
			}
		})
	}
}

func TestResolveEnhanced(t *testing.T) {
	res, err := Resolve("gemini-3.1-pro-enhanced", "gemini-4-hydrogen")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if res.Extra == nil || res.Extra[31] != 2 || res.Extra[80] != 3 {
		t.Errorf("Expected extra fields {31:2, 80:3}, got %v", res.Extra)
	}
	// The enhanced variant must still route to the real Pro backend.
	if res.Header != "e6fa609c3fa255c0" {
		t.Errorf("res.Header = %q, want the Pro backend id", res.Header)
	}
}

func TestEveryModelHasHeader(t *testing.T) {
	// A model without a Header never sends the routing header, so upstream
	// answers with its default model. Every advertised name must route.
	for name, m := range MODELS {
		if m.Header == "" {
			t.Errorf("model %q has no Header and would collapse onto the default backend", name)
		}
	}
}

func TestEveryModelMapsToDiscoveredBackend(t *testing.T) {
	// Backend ids reported by otAQ7b for a standard account. Anything outside
	// this set cannot be routed and must not be advertised.
	known := map[string]bool{
		"56fdd199312815e2": true, // 4 Hydrogen
		"e6fa609c3fa255c0": true, // 3.1 Pro
		"8c46e95b1a07cecc": true, // 3.5 Flash-Lite
		"95a4c0abcba24364": true, // 4 Carbon
	}
	for name, m := range MODELS {
		if !known[m.Header] {
			t.Errorf("model %q routes to unknown backend %q", name, m.Header)
		}
	}
}

func TestDefaultModelIsRegistered(t *testing.T) {
	if _, ok := MODELS[DefaultModelName]; !ok {
		t.Errorf("DefaultModelName %q is not in MODELS; every request would fall back",
			DefaultModelName)
	}
}

func TestBackendAliasesShareHeader(t *testing.T) {
	// Aliases are only legitimate when they resolve to the same backend id.
	carbon := []string{"gemini-4-carbon", "gemini-4-argon", "carbon", "gemini-4"}
	for _, n := range carbon {
		if MODELS[n].Header != "95a4c0abcba24364" {
			t.Errorf("%q should alias the Carbon backend", n)
		}
	}
	hydrogen := []string{"gemini-4-hydrogen", "hydrogen", "gemini-3.5-flash"}
	for _, n := range hydrogen {
		if MODELS[n].Header != "56fdd199312815e2" {
			t.Errorf("%q should alias the Hydrogen backend", n)
		}
	}
	pro := []string{"gemini-3.1-pro", "gemini-3.1-pro-enhanced", "pro"}
	for _, n := range pro {
		if MODELS[n].Header != "e6fa609c3fa255c0" {
			t.Errorf("%q should alias the Pro backend", n)
		}
	}
	lite := []string{"gemini-3.5-flash-lite", "gemini-flash-lite", "flash-lite"}
	for _, n := range lite {
		if MODELS[n].Header != "8c46e95b1a07cecc" {
			t.Errorf("%q should alias the Flash-Lite backend", n)
		}
	}
}
