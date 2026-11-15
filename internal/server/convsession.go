package server

import (
	"strings"

	"github.com/ikhsan3adi/gemini-web2api/internal/conv"
	"github.com/ikhsan3adi/gemini-web2api/internal/gemini"
	"github.com/ikhsan3adi/gemini-web2api/internal/models"
)

// The conversation cache works on a "prefix key" convention:
//
//   lookup  -> HistoryKey(model, all-but-last message)
//   store   -> HistoryKey(model, all messages + this turn's reply)
//
// OpenAI clients resend the full history every turn, so the next request's
// prefix is exactly the history we stored, and the two line up. Storing the
// *post-send* history is what makes chaining work; storing the pre-send prefix
// would never match on the following turn.

// chatMessagesToConvMsgs flattens OpenAI messages to the normalised form used for
// hashing. It ignores content that does not affect upstream routing (image data
// is reduced to a marker) so the key stays stable across turns.
func chatMessagesToConvMsgs(msgs []models.OpenAIMessage) []conv.Msg {
	out := make([]conv.Msg, 0, len(msgs))
	for _, m := range msgs {
		role := m.Role
		if role == "" {
			role = "user"
		}
		out = append(out, conv.Msg{Role: role, Content: flattenContent(m.Content)})
	}
	return out
}

// flattenContent renders message content to text for hashing purposes.
func flattenContent(content any) string {
	switch c := content.(type) {
	case nil:
		return ""
	case string:
		return c
	case []any:
		var parts []string
		for _, item := range c {
			switch it := item.(type) {
			case string:
				parts = append(parts, it)
			case map[string]any:
				if t, ok := it["text"].(string); ok && t != "" {
					parts = append(parts, t)
					continue
				}
				if _, ok := it["image_url"]; ok {
					parts = append(parts, "[image]")
				}
			}
		}
		return strings.Join(parts, " ")
	default:
		return ""
	}
}

// resolveConv looks up a cached conversation handle for one account.
//
// Two things must match:
//
//  1. Model. Gemini rejects a chat continued under a different model, so the
//     model is part of the key.
//  2. Account. A conversation id is scoped to the cookie that created it. The
//     pool moves between accounts as quota shifts, so replaying a handle on a
//     different account would fail upstream (or, worse, be silently refused).
//     Matching on account keeps the chat pinned to whoever created it.
func (a *App) resolveConv(model string, history []conv.Msg, account string) (meta []any, key string, hit bool) {
	if a.Pool == nil || a.Pool.convs == nil || len(history) == 0 {
		return nil, "", false
	}

	key = conv.HistoryKey(model, history)
	rec, ok := a.Pool.convs.Get(key)
	if !ok {
		return nil, key, false
	}
	if rec.Model != conv.NormalizeModel(model) {
		return nil, key, false
	}
	if rec.Account != account {
		a.Logf("conv skip %s: cached on account %q, serving account %q",
			conv.Fingerprint(key), rec.Account, account)
		return nil, key, false
	}

	payload := rec.Metadata
	if len(payload) == 0 {
		return nil, key, false
	}

	a.Logf("conv hit %s model=%s account=%s history=%d",
		conv.Fingerprint(key), model, rec.Account, len(history))
	return payload, key, true
}

// storeConv records the handle for the turn that just completed.
//
// fullHistory must be the history *including* the reply we returned, because the
// next request's prefix will be exactly that.
func (a *App) storeConv(model, account string, fullHistory []conv.Msg, meta *gemini.ConversationMeta, responseID string) {
	if a.Pool == nil || a.Pool.convs == nil || meta == nil {
		return
	}
	payload := meta.Payload()
	if len(payload) == 0 || len(fullHistory) == 0 {
		return
	}

	key := conv.HistoryKey(model, fullHistory)
	a.Pool.convs.Put(&conv.Record{
		Key:        key,
		Model:      conv.NormalizeModel(model),
		Account:    account,
		Metadata:   payload,
		ResponseID: responseID,
	})
	a.Logf("conv store %s model=%s account=%s turn=%d",
		conv.Fingerprint(key), model, account, len(fullHistory))
}

// invalidateConv drops a handle that upstream rejected.
func (a *App) invalidateConv(key string) {
	if key == "" || a.Pool == nil || a.Pool.convs == nil {
		return
	}
	a.Pool.convs.Delete(key)
	a.Logf("conv invalidated %s (upstream rejected the handle)", conv.Fingerprint(key))
}

// dropLastUserMessage returns the text of the final message only. Used when a
// cached conversation is being continued: upstream already has the history.
func lastMessageText(msgs []models.OpenAIMessage) string {
	if len(msgs) == 0 {
		return ""
	}
	m := msgs[len(msgs)-1]
	text := flattenContent(m.Content)
	if text != "" {
		return text
	}
	// Tool results carry their payload under other shapes; fall back to the full
	// flattening of that single message.
	return flattenContent(m.Content)
}

// googleContentsToConvMsgs flattens Google contents for hashing.
func googleContentsToConvMsgs(req models.GoogleGenerateRequest) []conv.Msg {
	out := make([]conv.Msg, 0, len(req.Contents)+1)
	if req.SystemInstruction != nil {
		var parts []string
		for _, p := range req.SystemInstruction.Parts {
			if p.Text != "" {
				parts = append(parts, p.Text)
			}
		}
		if len(parts) > 0 {
			out = append(out, conv.Msg{Role: "system", Content: strings.Join(parts, " ")})
		}
	}
	for _, c := range req.Contents {
		role := c.Role
		if role == "" {
			role = "user"
		}
		var parts []string
		for _, p := range c.Parts {
			if p.Text != "" {
				parts = append(parts, p.Text)
			}
			if p.InlineData != nil {
				parts = append(parts, "[image]")
			}
			if p.FunctionResponse != nil {
				parts = append(parts, p.FunctionResponse.Name)
			}
		}
		out = append(out, conv.Msg{Role: role, Content: strings.Join(parts, " ")})
	}
	return out
}

// lastGooglePartText returns only the final content entry's text.
func lastGooglePartText(req models.GoogleGenerateRequest) string {
	if len(req.Contents) == 0 {
		return ""
	}
	var parts []string
	for _, p := range req.Contents[len(req.Contents)-1].Parts {
		if p.Text != "" {
			parts = append(parts, p.Text)
		}
	}
	return strings.Join(parts, " ")
}
