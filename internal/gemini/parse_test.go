package gemini

import (
	"encoding/json"
	"testing"
)

func TestCleanText(t *testing.T) {
	input := "Hello world\n```python?code_reference&code_event_index=1\nprint('hi')```\nhttp://googleusercontent.com/card_content/123\nExtra text"
	cleaned := CleanText(input, true)
	expected := "Hello world\nExtra text"
	if cleaned != expected {
		t.Errorf("CleanText got %q, want %q", cleaned, expected)
	}
}

func TestIsBardError(t *testing.T) {
	raw := "something BardErrorInfo [1024] happened"
	code, ok := IsBardError(raw)
	if !ok || code != "1024" {
		t.Errorf("IsBardError got (%q, %t), want (\"1024\", true)", code, ok)
	}
}

// metaFrame builds a batchexecute frame carrying both text and conversation ids.
func metaFrame(t *testing.T, text, cid, rid, contextStr string) string {
	t.Helper()

	mData := []any{cid, rid}
	part := []any{"rc_1", []string{text}, nil, nil, nil, nil, nil, nil, []int{1}, "en"}
	inner := make([]any, 26)
	inner[1] = mData
	inner[4] = []any{part}
	if contextStr != "" {
		inner[25] = contextStr
	}

	innerJSON, err := json.Marshal(inner)
	if err != nil {
		t.Fatalf("marshal inner: %v", err)
	}
	line := []any{[]any{"wrb.fr", nil, string(innerJSON)}}
	out, err := json.Marshal(line)
	if err != nil {
		t.Fatalf("marshal line: %v", err)
	}
	return string(out)
}

func TestExtractTextsAndMetaCapturesIds(t *testing.T) {
	line := metaFrame(t, "hello", "c_abc123", "r_def456", "")
	var meta ConversationMeta

	texts := ExtractTextsAndMeta(line, &meta)
	if len(texts) != 1 || texts[0] != "hello" {
		t.Fatalf("texts = %v", texts)
	}
	if meta.Cid != "c_abc123" {
		t.Errorf("Cid = %q", meta.Cid)
	}
	if meta.Rid != "r_def456" {
		t.Errorf("Rid = %q", meta.Rid)
	}
	if !meta.Valid() {
		t.Error("meta should be valid after capturing ids")
	}

	// Without a context string the raw metadata array is what gets replayed.
	payload := meta.Payload()
	if len(payload) != 2 || payload[0] != "c_abc123" || payload[1] != "r_def456" {
		t.Errorf("Payload = %v, want the raw cid/rid array", payload)
	}
}

func TestExtractTextsAndMetaContextWins(t *testing.T) {
	line := metaFrame(t, "done", "c_abc", "r_def", "CONTEXT_TOKEN")
	var meta ConversationMeta
	ExtractTextsAndMeta(line, &meta)

	if meta.Context != "CONTEXT_TOKEN" {
		t.Fatalf("Context = %q", meta.Context)
	}

	// Once a turn completes Google hands back a context string, and the reference
	// client switches to a flat ten-slot array with the context in slot 9.
	payload := meta.Payload()
	if len(payload) != 10 {
		t.Fatalf("Payload len = %d, want 10", len(payload))
	}
	for i := 0; i < 9; i++ {
		if payload[i] != nil {
			t.Errorf("Payload[%d] = %v, want nil", i, payload[i])
		}
	}
	if payload[9] != "CONTEXT_TOKEN" {
		t.Errorf("Payload[9] = %v", payload[9])
	}
}

func TestExtractTextsAndMetaNilMeta(t *testing.T) {
	line := metaFrame(t, "text", "c_1", "r_1", "")
	if texts := ExtractTextsAndMeta(line, nil); len(texts) != 1 || texts[0] != "text" {
		t.Errorf("texts = %v", texts)
	}
}

func TestExtractTextsFromLineStillWorks(t *testing.T) {
	line := metaFrame(t, "legacy", "c_1", "r_1", "")
	if texts := ExtractTextsFromLine(line); len(texts) != 1 || texts[0] != "legacy" {
		t.Errorf("texts = %v", texts)
	}
}

func TestExtractResponseTextAndMeta(t *testing.T) {
	line := metaFrame(t, "answer", "c_xyz", "r_xyz", "CTX")
	var meta ConversationMeta

	text, err := ExtractResponseTextAndMeta(line, &meta)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if text != "answer" {
		t.Errorf("text = %q", text)
	}
	if meta.Cid != "c_xyz" || meta.Context != "CTX" {
		t.Errorf("meta = %+v", meta)
	}
}

func TestConversationMetaNilSafety(t *testing.T) {
	var m *ConversationMeta
	if m.Valid() {
		t.Error("nil meta should not be valid")
	}
	if m.Payload() != nil {
		t.Error("nil meta should have no payload")
	}
}
