package conv

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// Msg is the minimal, normalised view of one chat message used for hashing.
type Msg struct {
	Role    string `json:"r"`
	Content string `json:"c"`
}

// HistoryKey fingerprints a conversation prefix.
//
// The model is part of the key on purpose. Gemini rejects a chat continued under
// a different model (MODEL_INCONSISTENT), so a model switch must miss and open a
// fresh conversation with the full history instead. Because the key includes the
// model, the old record survives and switching back reuses it.
//
// Hashing the *prefix* rather than the whole request is what makes chaining
// work: after a turn finishes we store the key of the full history we just
// produced, and the next turn's prefix is exactly that history.
func HistoryKey(model string, msgs []Msg) string {
	h := sha256.New()

	fmt.Fprintf(h, "model=%s\n", NormalizeModel(model))
	for _, m := range msgs {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		if role == "" {
			role = "user"
		}
		fmt.Fprintf(h, "%s\x00%s\x00", role, NormalizeContent(m.Content))
	}

	return hex.EncodeToString(h.Sum(nil))
}

// NormalizeModel strips decorations that do not affect upstream routing, such as
// the "@think=N" suffix: the same conversation continued at a different thinking
// depth is still the same backend conversation.
func NormalizeModel(model string) string {
	m := strings.TrimSpace(strings.ToLower(model))
	if idx := strings.Index(m, "@think="); idx >= 0 {
		m = m[:idx]
	}
	return m
}

// NormalizeContent collapses whitespace so a client that re-sends the same text
// with different trailing spaces still hits the cache.
func NormalizeContent(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// KeyForMessages builds the key from already-normalised messages.
func KeyForMessages(model string, msgs []Msg) string {
	return HistoryKey(model, msgs)
}

// AppendAssistant returns a new slice with the assistant reply appended. Used to
// compute the "after sending" key that the next turn will look up.
func AppendAssistant(msgs []Msg, reply string) []Msg {
	out := make([]Msg, len(msgs), len(msgs)+1)
	copy(out, msgs)
	out = append(out, Msg{Role: "assistant", Content: reply})
	return out
}

// Fingerprint is a short key prefix for logging without dumping hashes in full.
func Fingerprint(key string) string {
	if len(key) <= 12 {
		return key
	}
	return key[:12]
}

// MarshalMessages is a small helper for tests and debugging.
func MarshalMessages(msgs []Msg) string {
	b, _ := json.Marshal(msgs)
	return string(b)
}
