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
			input:        "gemini-4-argon",
			defaultName:  "gemini-4-hydrogen",
			wantName:     "gemini-4-argon",
			wantMode:     8,
			wantThink:    4,
			wantHeader:   "95a4c0abcba24364",
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
			input:        "gemini-3.5-flash-lite",
			defaultName:  "gemini-4-hydrogen",
			wantName:     "gemini-3.5-flash-lite",
			wantMode:     6,
			wantThink:    4,
			wantHeader:   "8c46e95b1a07cecc",
			wantFallback: false,
		},
		{
			input:        "gemini-4-argon@think=0",
			defaultName:  "gemini-4-hydrogen",
			wantName:     "gemini-4-argon",
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
		// Aliases were removed so /v1/models lists exactly the four backends.
		// These names must now fall back loudly rather than resolve silently.
		{
			input:        "argon",
			defaultName:  "gemini-4-hydrogen",
			wantName:     "gemini-4-hydrogen",
			wantMode:     1,
			wantHeader:   "56fdd199312815e2",
			wantFallback: true,
		},
		{
			input:        "gemini-4",
			defaultName:  "gemini-4-hydrogen",
			wantName:     "gemini-4-hydrogen",
			wantFallback: true,
		},
		{
			input:        "pro",
			defaultName:  "gemini-4-hydrogen",
			wantName:     "gemini-4-hydrogen",
			wantFallback: true,
		},
		{
			input:        "gemini-3.5-flash",
			defaultName:  "gemini-4-hydrogen",
			wantName:     "gemini-4-hydrogen",
			wantFallback: true,
		},
		{
			input:        "flash-lite",
			defaultName:  "gemini-4-hydrogen",
			wantName:     "gemini-4-hydrogen",
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
			if res.Fallback != tt.wantFallback {
				t.Errorf("res.Fallback = %v, want %v", res.Fallback, tt.wantFallback)
			}
			// Only assert the resolved backend for non-fallback cases; a fallback
			// intentionally lands on the default model.
			if tt.wantFallback {
				return
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
		})
	}
}

func TestEveryModelHasHeader(t *testing.T) {
	for name, m := range MODELS {
		if m.Header == "" {
			t.Errorf("model %q has no Header and would collapse onto the default backend", name)
		}
	}
}

func TestEveryModelMapsToDiscoveredBackend(t *testing.T) {
	// Backend ids reported by otAQ7b for a standard account.
	known := map[string]bool{
		"56fdd199312815e2": true, // 4 Hydrogen
		"e6fa609c3fa255c0": true, // 3.1 Pro
		"8c46e95b1a07cecc": true, // 3.5 Flash-Lite
		"95a4c0abcba24364": true, // 4 Argon
	}
	for name, m := range MODELS {
		if !known[m.Header] {
			t.Errorf("model %q routes to unknown backend %q", name, m.Header)
		}
	}
}

func TestRegistryMatchesUiModelCount(t *testing.T) {
	// The Gemini web UI offers exactly four models; /v1/models must not advertise
	// more, or clients think there are models they cannot select.
	if len(MODELS) != 4 {
		names := make([]string, 0, len(MODELS))
		for n := range MODELS {
			names = append(names, n)
		}
		t.Errorf("len(MODELS) = %d, want 4 (got %v)", len(MODELS), names)
	}
}

func TestNoAliasesRemain(t *testing.T) {
	// Guard against aliases creeping back in: every name must be distinct from
	// the others and map to its own backend exactly once.
	seen := make(map[string]string, len(MODELS))
	for name, m := range MODELS {
		if prev, ok := seen[m.Header]; ok {
			t.Errorf("%q and %q share backend %q; one of them is an alias",
				prev, name, m.Header)
		}
		seen[m.Header] = name
	}
}

func TestDefaultModelIsRegistered(t *testing.T) {
	if _, ok := MODELS[DefaultModelName]; !ok {
		t.Errorf("DefaultModelName %q is not in MODELS; every request would fall back",
			DefaultModelName)
	}
}
