package gemini

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

var (
	reCodeExec    = regexp.MustCompile(`(?s)\x60\x60\x60(?:python|javascript|text)\?code_(?:reference|stdout)&code_event_index=\d+\n.*?\x60\x60\x60\n?`)
	reCardContent = regexp.MustCompile(`http://googleusercontent\.com/card_content/\d+\n?`)
	reBardError   = regexp.MustCompile(`BardErrorInfo\s*\[(\d+)\]`)
)

func CleanText(text string, strip bool) string {
	text = reCodeExec.ReplaceAllString(text, "")
	text = reCardContent.ReplaceAllString(text, "")
	if strip {
		return strings.TrimSpace(text)
	}
	return text
}

// ConversationMeta is the handle Google issues for a chat. Sending the raw
// metadata back at payload index 2 continues the conversation server-side, so
// only the new message has to be sent instead of the whole history.
type ConversationMeta struct {
	Cid     string // m_data[0], e.g. "c_16a4abaa84ba4065"
	Rid     string // m_data[1], e.g. "r_a2d56f2278ab7093"
	Context string // inner[25] from a completed turn
	Raw     []any  // the whole m_data array, fed back verbatim

	// Account records which cookie produced this handle. Gemini scopes chats to
	// the account that created them, so a cached conversation may only be
	// replayed on the same account.
	Account string
}

// Payload returns what should be written to payload index 2.
//
// When a turn completes Google hands back a context string, and the reference
// client switches to a flat [nil x9, context] shape. Before that, the
// cid/rid metadata array is what continues the conversation.
func (m *ConversationMeta) Payload() []any {
	if m == nil {
		return nil
	}
	if m.Context != "" {
		out := make([]any, 10)
		out[9] = m.Context
		return out
	}
	if len(m.Raw) > 0 {
		return m.Raw
	}
	return nil
}

// Valid reports whether anything usable was captured.
func (m *ConversationMeta) Valid() bool {
	return m != nil && (m.Context != "" || len(m.Raw) > 0)
}

// ExtractTextsFromLine keeps the original signature for callers that only want
// text.
func ExtractTextsFromLine(line string) []string {
	return ExtractTextsAndMeta(line, nil)
}

// ExtractTextsAndMeta is ExtractTextsFromLine plus conversation bookkeeping.
// A non-nil meta is filled in place when the frame carries conversation ids.
func ExtractTextsAndMeta(line string, meta *ConversationMeta) []string {
	if !strings.Contains(line, `"wrb.fr"`) || len(line) < 200 {
		return nil
	}

	var arr []any
	if err := json.Unmarshal([]byte(line), &arr); err != nil || len(arr) == 0 {
		return nil
	}

	firstElem, ok := arr[0].([]any)
	if !ok || len(firstElem) < 3 {
		return nil
	}

	innerStr, ok := firstElem[2].(string)
	if !ok || len(innerStr) < 50 {
		return nil
	}

	var inner []any
	if err := json.Unmarshal([]byte(innerStr), &inner); err != nil {
		return nil
	}

	if meta != nil {
		captureMeta(inner, meta)
	}

	if len(inner) <= 4 || inner[4] == nil {
		return nil
	}

	parts, ok := inner[4].([]any)
	if !ok {
		return nil
	}

	var texts []string
	for _, part := range parts {
		partList, ok := part.([]any)
		if ok && len(partList) > 1 && partList[1] != nil {
			tList, ok := partList[1].([]any)
			if ok {
				for _, t := range tList {
					if str, ok := t.(string); ok && str != "" {
						texts = append(texts, str)
					}
				}
			}
		}
	}

	return texts
}

// captureMeta pulls the conversation handle out of a decoded frame.
func captureMeta(inner []any, meta *ConversationMeta) {
	if mData, ok := at(inner, 1).([]any); ok && len(mData) > 0 {
		if cid, ok := at(mData, 0).(string); ok && cid != "" {
			meta.Cid = cid
		}
		if rid, ok := at(mData, 1).(string); ok && rid != "" {
			meta.Rid = rid
		}
		meta.Raw = mData
	}
	if ctxStr, ok := at(inner, 25).(string); ok && ctxStr != "" {
		meta.Context = ctxStr
	}
}

func IsBardError(raw string) (string, bool) {
	match := reBardError.FindStringSubmatch(raw)
	if len(match) > 1 {
		return match[1], true
	}
	return "", false
}

func ExtractResponseText(raw string) (string, error) {
	text, err := ExtractResponseTextAndMeta(raw, nil)
	return text, err
}

// ExtractResponseTextAndMeta is ExtractResponseText plus conversation capture.
func ExtractResponseTextAndMeta(raw string, meta *ConversationMeta) (string, error) {
	if code, ok := IsBardError(raw); ok {
		return "", fmt.Errorf("gemini upstream rejected request: BardErrorInfo [%s]", code)
	}

	var lastText string
	lines := strings.Split(raw, "\n")
	for _, line := range lines {
		texts := ExtractTextsAndMeta(line, meta)
		for _, t := range texts {
			if len(t) > len(lastText) {
				lastText = t
			}
		}
	}

	return CleanText(lastText, true), nil
}
