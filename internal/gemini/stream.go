package gemini

import (
	"errors"
	"fmt"
	"strings"
)

type StreamParser struct {
	prevText string
	buf      string

	// emitted counts how much text has already been handed to the client. It
	// gates retries: once a reply is partially delivered, retrying would create
	// a second upstream turn that the local cache never learns about.
	emitted int

	// Meta collects the conversation ids Google issues alongside the text so a
	// follow-up request can continue this chat server-side.
	Meta *ConversationMeta
}

func NewStreamParser() *StreamParser {
	return &StreamParser{Meta: &ConversationMeta{}}
}

// Emitted reports whether any text has already been delivered downstream.
func (p *StreamParser) Emitted() bool { return p.emitted > 0 }

// EmittedLen is the number of characters already delivered, for logging.
func (p *StreamParser) EmittedLen() int { return p.emitted }

// ResetBuffer clears the line buffer before a retry attempt while preserving prevText across retries.
func (p *StreamParser) ResetBuffer() {
	p.buf = ""
}

func (p *StreamParser) Feed(chunk string) ([]string, error) {
	p.buf += chunk

	if strings.Contains(p.buf, "BardErrorInfo") {
		if code, ok := IsBardError(p.buf); ok {
			return nil, fmt.Errorf("gemini upstream rejected request: BardErrorInfo [%s]", code)
		}
	}

	var deltas []string
	for strings.Contains(p.buf, "\n") {
		idx := strings.Index(p.buf, "\n")
		line := p.buf[:idx]
		p.buf = p.buf[idx+1:]

		texts := ExtractTextsAndMeta(line, p.Meta)
		for _, t := range texts {
			// Skip if this text segment has already been fully processed or is identical to the last one.
			if t == p.prevText || strings.HasPrefix(p.prevText, t) {
				continue
			}

			// If the new text doesn't start with the previously processed text, it means
			// the server sent a completely different text blob, breaking the delta continuation assumption.
			if !strings.HasPrefix(t, p.prevText) {
				return nil, errors.New("gemini stream content changed during retry")
			}

			delta := CleanText(t[len(p.prevText):], false)
			p.prevText = t
			if delta != "" {
				p.emitted += len(delta)
				deltas = append(deltas, delta)
			}
		}
	}

	return deltas, nil
}
