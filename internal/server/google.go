package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/ikhsan3adi/gemini-web2api/internal/conv"
	"github.com/ikhsan3adi/gemini-web2api/internal/format"
	"github.com/ikhsan3adi/gemini-web2api/internal/gemini"
	"github.com/ikhsan3adi/gemini-web2api/internal/models"
)

func (a *App) handleGoogleGenerate(w http.ResponseWriter, r *http.Request) {
	target := r.PathValue("target")
	if target == "" {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return
	}

	modelName := target
	stream := false

	if idx := strings.LastIndex(target, ":"); idx != -1 {
		action := target[idx+1:]
		modelName = target[:idx]
		if action == "streamGenerateContent" {
			stream = true
		} else if action != "generateContent" {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
			return
		}
	}

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil || len(bodyBytes) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "invalid JSON"}})
		return
	}

	var req models.GoogleGenerateRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "invalid JSON"}})
		return
	}

	if modelName == "" {
		modelName = a.Cfg.DefaultModel
	}

	resolved, err := models.Resolve(modelName)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": err.Error()}})
		return
	}
	setModelHeaders(w, resolved)

	fcMode := "AUTO"
	if req.ToolConfig != nil && req.ToolConfig.FunctionCallingConfig != nil && req.ToolConfig.FunctionCallingConfig.Mode != "" {
		fcMode = req.ToolConfig.FunctionCallingConfig.Mode
	}

	hasTools := len(req.Tools) > 0 && fcMode != "NONE"

	// Build the conversation prefix from all but the last content entry, mirroring
	// the OpenAI path: the prefix identifies the chat, the tail is the new turn.
	convHistory := googleContentsToConvMsgs(req)
	var convPrefix []conv.Msg
	if len(convHistory) > 0 {
		convPrefix = convHistory[:len(convHistory)-1]
	}
	fullPrompt, images, err := format.GoogleContentsToPrompt(req)
	if err != nil || len(fullPrompt) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "empty content"}})
		return
	}

	lastPrompt := lastGooglePartText(req)
	var convKey string
	var convHit bool
	resolve := func(account string) ([]any, string, string) {
		meta, key, hit := a.resolveConv(modelName, convPrefix, account)
		if !hit {
			return nil, fullPrompt, key
		}
		convKey, convHit = key, true
		return meta, lastPrompt, key
	}

	a.Logf("Google API: model=%s stream=%t tools=%t prompt_len=%d images=%d", resolved.Name, stream, hasTools, len(fullPrompt), len(images))

	gemOpt := gemini.ModelOptions{
		ModelName:   resolved.Name,
		ModelID:     resolved.Mode,
		ThinkMode:   resolved.Think,
		ModelHeader: resolved.Header,
		Extra:       resolved.Extra,
	}

	if stream && !hasTools {
		// Lazy SSE so a pre-first-token failure can fail over or return JSON.
		started := false

		var fullText string
		streamMeta, emitErr := a.Pool.GenerateStreamWithConv(r.Context(), fullPrompt, gemOpt, images, resolve, func(delta string) error {
			if delta == "" {
				return nil
			}
			if !started {
				started = true
				if !startSSE(w) {
					return errClientGone
				}
			}
			fullText += delta
			chunkObj := models.GoogleGenerateResponse{
				Candidates: []models.GoogleCandidate{
					{
						Index: 0,
						Content: &models.GoogleContent{
							Role: "model",
							Parts: []models.GooglePart{
								{Text: delta},
							},
						},
					},
				},
				ModelVersion: resolved.Name,
			}
			return writeSSEData(w, chunkObj)
		})

		if emitErr == errClientGone {
			return
		}

		if emitErr == nil {
			if !started {
				if !startSSE(w) {
					return
				}
			}
			promptTokens := len(fullPrompt) / 4
			candidatesTokens := len(fullText) / 4
			finalChunk := models.GoogleGenerateResponse{
				Candidates: []models.GoogleCandidate{
					{
						Index:        0,
						FinishReason: "STOP",
					},
				},
				UsageMetadata: &models.GoogleUsageMetadata{
					PromptTokenCount:     promptTokens,
					CandidatesTokenCount: candidatesTokens,
					TotalTokenCount:      promptTokens + candidatesTokens,
				},
				ModelVersion: resolved.Name,
			}
			_ = writeSSEData(w, finalChunk)

			if streamMeta != nil && streamMeta.Valid() {
				full := append(convHistory, conv.Msg{Role: "model", Content: fullText})
				a.storeConv(modelName, streamMeta.Account, full, streamMeta, "")
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
		a.Logf("Google stream error: %v", emitErr)
		return
	}

	text, genMeta, err := a.Pool.GenerateWithConv(r.Context(), fullPrompt, gemOpt, images, resolve)
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
		full := append(convHistory, conv.Msg{Role: "model", Content: text})
		a.storeConv(modelName, genMeta.Account, full, genMeta, "")
	}

	var responseParts []models.GooglePart
	if hasTools && text != "" {
		cleanText, fnCalls := format.ParseGoogleFunctionCalls(text)
		if len(fnCalls) > 0 {
			if cleanText != "" {
				responseParts = append(responseParts, models.GooglePart{Text: cleanText})
			}
			for _, fc := range fnCalls {
				responseParts = append(responseParts, models.GooglePart{
					FunctionCall: &models.GoogleFunctionCall{
						Name: fc.Name,
						Args: fc.Args,
					},
				})
			}
		} else {
			responseParts = append(responseParts, models.GooglePart{Text: text})
		}
	} else {
		fallbackText := text
		if fallbackText == "" {
			fallbackText = "I apologize, but I was unable to generate a response. Please try again."
		}
		responseParts = append(responseParts, models.GooglePart{Text: fallbackText})
	}

	promptTokens := len(fullPrompt) / 4
	candidatesTokens := len(text) / 4

	responseObj := models.GoogleGenerateResponse{
		Candidates: []models.GoogleCandidate{
			{
				Index: 0,
				Content: &models.GoogleContent{
					Role:  "model",
					Parts: responseParts,
				},
				FinishReason: "STOP",
			},
		},
		UsageMetadata: &models.GoogleUsageMetadata{
			PromptTokenCount:     promptTokens,
			CandidatesTokenCount: candidatesTokens,
			TotalTokenCount:      promptTokens + candidatesTokens,
		},
		ModelVersion: resolved.Name,
	}

	if stream {
		if !startSSE(w) {
			return
		}
		_ = writeSSEData(w, responseObj)
	} else {
		writeJSON(w, http.StatusOK, responseObj)
	}
}
