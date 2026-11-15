package models

import (
	"strings"
	"testing"
)

func TestResolveKnownModels(t *testing.T) {
	tests := []struct {
		input      string
		wantName   string
		wantMode   int
		wantThink  int
		wantHeader string
	}{
		{
			input:      "gemini-4-hydrogen",
			wantName:   "gemini-4-hydrogen",
			wantMode:   1,
			wantThink:  4,
			wantHeader: "56fdd199312815e2",
		},
		{
			input:      "gemini-4-argon",
			wantName:   "gemini-4-argon",
			wantMode:   8,
			wantThink:  4,
			wantHeader: "95a4c0abcba24364",
		},
		{
			input:      "gemini-3.1-pro",
			wantName:   "gemini-3.1-pro",
			wantMode:   3,
			wantThink:  4,
			wantHeader: "e6fa609c3fa255c0",
		},
		{
			input:      "gemini-3.5-flash-lite",
			wantName:   "gemini-3.5-flash-lite",
			wantMode:   6,
			wantThink:  4,
			wantHeader: "8c46e95b1a07cecc",
		},
		{
			input:      "gemini-4-argon@think=0",
			wantName:   "gemini-4-argon",
			wantMode:   8,
			wantThink:  0,
			wantHeader: "95a4c0abcba24364",
		},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			res, err := Resolve(tt.input)
			if err != nil {
				t.Fatalf("Resolve(%q): %v", tt.input, err)
			}
			if res.Name != tt.wantName {
				t.Errorf("Name = %q, want %q", res.Name, tt.wantName)
			}
			if res.Mode != tt.wantMode {
				t.Errorf("Mode = %d, want %d", res.Mode, tt.wantMode)
			}
			if res.Think != tt.wantThink {
				t.Errorf("Think = %d, want %d", res.Think, tt.wantThink)
			}
			if res.Header != tt.wantHeader {
				t.Errorf("Header = %q, want %q", res.Header, tt.wantHeader)
			}
		})
	}
}

// An unknown model must be rejected, never silently replaced by a different one:
// answering with another model makes the response untrustworthy and hides caller
// mistakes.
func TestResolveUnknownModelFails(t *testing.T) {
	unknown := []string{
		"gemini-9.9-fake",
		"argon",           // removed alias
		"gemini-4",        // removed alias
		"pro",             // removed alias
		"gemini-3.5-flash", // removed alias
		"flash-lite",      // removed alias
		"gemini-auto",     // never existed in this build
		"gemini-3.6-flash",
		"",
	}

	for _, name := range unknown {
		t.Run(name, func(t *testing.T) {
			if name == "" {
				// Empty means "use the default"; covered separately.
				return
			}
			_, err := Resolve(name)
			if err == nil {
				t.Fatalf("Resolve(%q) should fail, got no error", name)
			}
			if !strings.Contains(err.Error(), "does not exist") {
				t.Errorf("error %q should say the model does not exist", err)
			}
			// The message must list what is actually available so callers can fix
			// their configuration without reading the source.
			if !strings.Contains(err.Error(), "gemini-4-hydrogen") {
				t.Errorf("error %q should list available models", err)
			}
		})
	}
}

func TestResolveEmptyUsesDefault(t *testing.T) {
	res, err := Resolve("")
	if err != nil {
		t.Fatalf("Resolve(\"\"): %v", err)
	}
	if res.Name != DefaultModelName {
		t.Errorf("Name = %q, want the default %q", res.Name, DefaultModelName)
	}
}

func TestResolveInvalidThinkSuffix(t *testing.T) {
	if _, err := Resolve("gemini-4-hydrogen@think=abc"); err == nil {
		t.Error("a non-numeric think level should fail")
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
	if len(MODELS) != 4 {
		t.Errorf("len(MODELS) = %d, want 4 (%s)", len(MODELS), AvailableModels())
	}
}

func TestNoAliasesRemain(t *testing.T) {
	seen := make(map[string]string, len(MODELS))
	for name, m := range MODELS {
		if prev, ok := seen[m.Header]; ok {
			t.Errorf("%q and %q share backend %q; one of them is an alias", prev, name, m.Header)
		}
		seen[m.Header] = name
	}
}

func TestDefaultModelIsRegistered(t *testing.T) {
	if _, ok := MODELS[DefaultModelName]; !ok {
		t.Errorf("DefaultModelName %q is not in MODELS", DefaultModelName)
	}
}

func TestAvailableModelsSorted(t *testing.T) {
	got := AvailableModels()
	parts := strings.Split(got, ", ")
	if len(parts) != len(MODELS) {
		t.Fatalf("AvailableModels() = %q, want %d entries", got, len(MODELS))
	}
	for i := 1; i < len(parts); i++ {
		if parts[i-1] > parts[i] {
			t.Errorf("not sorted: %q before %q", parts[i-1], parts[i])
		}
	}
}
