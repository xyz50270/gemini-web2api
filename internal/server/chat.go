package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ikhsan3adi/gemini-web2api/internal/conv"
	"github.com/ikhsan3adi/gemini-web2api/internal/format"
	"github.com/ikhsan3adi/gemini-web2api/internal/gemini"
	"github.com/ikhsan3adi/gemini-web2api/internal/models"
)

func (a *App) handleChat(w http.ResponseWriter, r *http.Request) {
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil || len(bodyBytes) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "invalid JSON"}})
		return
	}

	var req models.OpenAIChatRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "invalid JSON"}})
		return
	}

	modelStr := req.Model
	if modelStr == "" {
		modelStr = a.Cfg.DefaultModel
	}

	resolved, err := models.Resolve(modelStr, a.Cfg.DefaultModel)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": err.Error()}})
		return
	}
	setModelHeaders(w, resolved)

	if req.ToolChoice == nil {
		req.ToolChoice = "auto"
	}

	// Split the history: everything before the last message is the prefix that
	// identifies the conversation, the last message is what we actually send.
	convHistory := chatMessagesToConvMsgs(req.Messages)
	var convPrefix []conv.Msg
	if len(convHistory) > 0 {
		convPrefix = convHistory[:len(convHistory)-1]
	}

	fullPrompt, images, err := format.MessagesToPrompt(req)
	if err != nil || strings.TrimSpace(fullPrompt) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "empty prompt"}})
		return
	}

	// The account is only known once the pool picks one, and a conversation id is
	// only reusable on the account that created it, so the cache lookup happens
	// inside the pool callback rather than here.
	lastPrompt := lastMessageText(req.Messages)
	var convKey string
	var convHit bool
	resolve := func(account string) ([]any, string, string) {
		meta, key, hit := a.resolveConv(modelStr, convPrefix, account)
		if !hit {
			return nil, fullPrompt, key
		}
		convKey, convHit = key, true
		// Upstream already holds the history, so only the new message is needed.
		return meta, lastPrompt, key
	}

	// Image upload happens inside the pool, against whichever account serves
	// the request, so file references stay bound to that account's session.
	promptImages := images

	gemOpt := gemini.ModelOptions{
		ModelName:   resolved.Name,
		ModelID:     resolved.Mode,
		ThinkMode:   resolved.Think,
		ModelHeader: resolved.Header,
		Extra:       resolved.Extra,
	}

	cid := fmt.Sprintf("chatcmpl-%s", format.RandHex(12))

	strChoice, isStr := req.ToolChoice.(string)
	isToolNone := isStr && strChoice == "none"

	if req.Stream && (len(req.Tools) == 0 || isToolNone) {
		// SSE is started lazily on the first real delta. Until anything has
		// been written the request can still fail over to another account and,
		// failing that, answer with a proper JSON error status.
		started := false

		firstChunk := models.OpenAIChatResponse{
			ID:      cid,
			Object:  "chat.completion.chunk",
			Created: time.Now().Unix(),
			Model:   resolved.Name,
			Choices: []models.OpenAIChoice{
				{
					Index: 0,
					Delta: &models.OpenAIMessage{
						Role: "assistant",
					},
					FinishReason: nil,
				},
			},
		}

		var fullText string
		streamMeta, emitErr := a.Pool.GenerateStreamWithConv(r.Context(), fullPrompt, gemOpt, promptImages, resolve, func(delta string) error {
			if !started {
				started = true
				if !startSSE(w) {
					return errClientGone
				}
				_ = writeSSEData(w, firstChunk)
			}
			chunk := models.OpenAIChatResponse{
				ID:      cid,
				Object:  "chat.completion.chunk",
				Created: time.Now().Unix(),
				Model:   resolved.Name,
				Choices: []models.OpenAIChoice{
					{
						Index: 0,
						Delta: &models.OpenAIMessage{
							Content: delta,
						},
						FinishReason: nil,
					},
				},
			}
			fullText += delta
			return writeSSEData(w, chunk)
		})

		if emitErr == errClientGone {
			return
		}

		if emitErr != nil && convHit && gemini.IsCredentialError(emitErr) {
			a.invalidateConv(convKey)
		}

		if emitErr == nil {
			if !started {
				// Upstream returned no text; still produce a valid stream.
				if !startSSE(w) {
					return
				}
				_ = writeSSEData(w, firstChunk)
			}
			stopReason := "stop"
			endChunk := models.OpenAIChatResponse{
				ID:      cid,
				Object:  "chat.completion.chunk",
				Created: time.Now().Unix(),
				Model:   resolved.Name,
				Choices: []models.OpenAIChoice{
					{
						Index:        0,
						Delta:        &models.OpenAIMessage{},
						FinishReason: &stopReason,
					},
				},
			}
			_ = writeSSEData(w, endChunk)
			_ = writeSSEDone(w)

			if streamMeta != nil && streamMeta.Valid() {
				full := append(convHistory, conv.Msg{Role: "assistant", Content: fullText})
				a.storeConv(modelStr, streamMeta.Account, full, streamMeta, cid)
			}
			return
		}

		if !started {
			if writePoolError(w, emitErr, a.Pool.QueueTimeout()) {
				return
			}
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": map[string]any{"message": fmt.Sprintf("upstream error: %v", emitErr)}})
			return
		}
		a.Logf("Chat stream error: %v", emitErr)
		return
	}

	text, genMeta, err := a.Pool.GenerateWithConv(r.Context(), fullPrompt, gemOpt, promptImages, resolve)
	if err != nil {
		if convHit && gemini.IsCredentialError(err) {
			a.invalidateConv(convKey)
		}
		if writePoolError(w, err, a.Pool.QueueTimeout()) {
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": map[string]any{"message": fmt.Sprintf("upstream error: %v", err)}})
		return
	}
	if genMeta != nil && genMeta.Valid() {
		full := append(convHistory, conv.Msg{Role: "assistant", Content: text})
		a.storeConv(modelStr, genMeta.Account, full, genMeta, cid)
	}

	var toolCalls []models.OpenAIToolCall
	if len(req.Tools) > 0 && text != "" && !isToolNone {
		text, toolCalls = format.ParseToolCalls(text)
	}

	msg := models.OpenAIMessage{
		Role:    "assistant",
		Content: text,
	}
	if text == "" {
		msg.Content = nil
	}

	if len(toolCalls) > 0 {
		msg.ToolCalls = toolCalls
	}

	finish := "stop"
	if len(toolCalls) > 0 {
		finish = "tool_calls"
	}

	if req.Stream {
		if !startSSE(w) {
			return
		}
		chunk := models.OpenAIChatResponse{
			ID:      cid,
			Object:  "chat.completion.chunk",
			Created: time.Now().Unix(),
			Model:   resolved.Name,
			Choices: []models.OpenAIChoice{
				{
					Index:        0,
					Delta:        &msg,
					FinishReason: &finish,
				},
			},
		}
		_ = writeSSEData(w, chunk)
		_ = writeSSEDone(w)
	} else {
		promptTokens := len(fullPrompt) / 4
		completionTokens := len(text) / 4
		resp := models.OpenAIChatResponse{
			ID:      cid,
			Object:  "chat.completion",
			Created: time.Now().Unix(),
			Model:   resolved.Name,
			Choices: []models.OpenAIChoice{
				{
					Index:        0,
					Message:      &msg,
					FinishReason: &finish,
				},
			},
			Usage: &models.OpenAIUsage{
				PromptTokens:     promptTokens,
				CompletionTokens: completionTokens,
				TotalTokens:      promptTokens + completionTokens,
			},
		}
		writeJSON(w, http.StatusOK, resp)
	}
}
