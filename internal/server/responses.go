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

func (a *App) handleResponses(w http.ResponseWriter, r *http.Request) {
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil || len(bodyBytes) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "invalid JSON"}})
		return
	}

	var req map[string]any
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "invalid JSON"}})
		return
	}

	modelStr, _ := req["model"].(string)
	if modelStr == "" {
		modelStr = a.Cfg.DefaultModel
	}

	resolved, err := models.Resolve(modelStr)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": err.Error()}})
		return
	}
	setModelHeaders(w, resolved)

	inputRaw := req["input"]
	instructions, _ := req["instructions"].(string)

	messages, err := format.ResponsesInputToMessages(inputRaw, instructions)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "invalid input structure"}})
		return
	}

	var reqMessages []models.OpenAIMessage
	for _, m := range messages {
		role, _ := m["role"].(string)
		content := m["content"]
		reqMessages = append(reqMessages, models.OpenAIMessage{Role: role, Content: content})
	}

	var reqTools []models.OpenAITool
	if toolsRaw, ok := req["tools"].([]any); ok {
		for _, t := range toolsRaw {
			if tm, ok := t.(map[string]any); ok {
				var fn models.OpenAIFunction
				if f, ok := tm["function"].(map[string]any); ok {
					fn.Name, _ = f["name"].(string)
					fn.Description, _ = f["description"].(string)
					fn.Parameters = f["parameters"]
				} else {
					fn.Name, _ = tm["name"].(string)
					fn.Description, _ = tm["description"].(string)
					fn.Parameters = tm["parameters"]
				}
				reqTools = append(reqTools, models.OpenAITool{Function: fn})
			}
		}
	}

	toolChoice := req["tool_choice"]
	if toolChoice == nil {
		toolChoice = "auto"
	}

	chatReq := models.OpenAIChatRequest{
		Messages:   reqMessages,
		Tools:      reqTools,
		ToolChoice: toolChoice,
	}

	// Conversation continuation. The Responses API gives us previous_response_id,
	// which is the most precise signal; fall back to the history prefix.
	convHistory := chatMessagesToConvMsgs(reqMessages)
	var convPrefix []conv.Msg
	if len(convHistory) > 0 {
		convPrefix = convHistory[:len(convHistory)-1]
	}

	fullPrompt, images, err := format.MessagesToPrompt(chatReq)
	if err != nil || strings.TrimSpace(fullPrompt) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "empty input"}})
		return
	}
	prevID, _ := req["previous_response_id"].(string)
	lastPrompt := lastMessageText(reqMessages)
	var convKey string
	var convHit bool

	// Resolved once an account is chosen: a conversation id may only be replayed
	// on the cookie that created it.
	resolve := func(account string) ([]any, string, string) {
		if prevID != "" && a.Pool.convs != nil {
			if rec, ok := a.Pool.convs.GetByResponseID(prevID); ok && len(rec.Metadata) > 0 &&
				rec.Model == conv.NormalizeModel(modelStr) && rec.Account == account {
				convHit = true
				a.Logf("conv hit via previous_response_id %s (account %s)", prevID, account)
				return rec.Metadata, lastPrompt, rec.Key
			}
		}
		meta, key, hit := a.resolveConv(modelStr, convPrefix, account)
		if !hit {
			return nil, fullPrompt, key
		}
		convKey, convHit = key, true
		return meta, lastPrompt, key
	}

	// Image upload is performed inside the pool against the account that
	// actually serves the request.
	gemOpt := gemini.ModelOptions{
		ModelName:   resolved.Name,
		ModelID:     resolved.Mode,
		ThinkMode:   resolved.Think,
		ModelHeader: resolved.Header,
		Extra:       resolved.Extra,
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

	strChoice, isStr := toolChoice.(string)
	isToolNone := isStr && strChoice == "none"

	// Token accounting reads the untouched reply: parsing a tool call moves it
	// out of text, and billing the remaining empty string reports zero output
	// tokens for the replies that cost the most to produce.
	rawText := text

	var toolCalls []models.OpenAIToolCall
	if len(reqTools) > 0 && text != "" && !isToolNone {
		text, toolCalls = format.ParseToolCalls(text)
	}

	rid := fmt.Sprintf("resp_%s", format.RandHex(16))
	mid := fmt.Sprintf("msg_%s", format.RandHex(12))

	// Cache the conversation handle keyed by this response id, so the next turn
	// can resume with previous_response_id.
	if genMeta != nil && genMeta.Valid() {
		full := append(convHistory, conv.Msg{Role: "assistant", Content: text})
		a.storeConv(modelStr, genMeta.Account, full, genMeta, rid)
	}

	outputItems := format.BuildResponseOutput(text, toolCalls, mid)

	stream, _ := req["stream"].(bool)
	promptTokens := format.EstimateTokens(fullPrompt)
	outputTokens := format.CompletionTokens(rawText, text)

	if stream {
		if !startSSE(w) {
			return
		}

		seqNum := 0
		emit := func(eventType string, fields map[string]any) {
			seqNum++
			event := map[string]any{
				"type":            eventType,
				"sequence_number": seqNum,
			}
			for k, v := range fields {
				event[k] = v
			}
			_ = writeSSEEvent(w, eventType, event)
		}

		usage := map[string]any{
			"input_tokens":  promptTokens,
			"output_tokens": outputTokens,
			"total_tokens":  promptTokens + outputTokens,
		}
		baseResp := map[string]any{
			"id":         rid,
			"object":     "response",
			"created_at": time.Now().Unix(),
			"model":      resolved.Name,
		}
		withStatus := func(status string, output any, use map[string]any) map[string]any {
			r := map[string]any{
				"id":         baseResp["id"],
				"object":     baseResp["object"],
				"created_at": baseResp["created_at"],
				"model":      baseResp["model"],
				"status":     status,
				"output":     output,
				"usage":      use,
			}
			return r
		}
		emit("response.created", map[string]any{
			"response": withStatus("in_progress", []any{}, nil),
		})
		emit("response.in_progress", map[string]any{
			"response": withStatus("in_progress", []any{}, nil),
		})

		for itemIdx, item := range outputItems {
			iType, _ := item["type"].(string)
			itemID, _ := item["id"].(string)

			switch iType {
			case "function_call":
				pending := map[string]any{
					"type":      "function_call",
					"id":        item["id"],
					"call_id":   item["call_id"],
					"name":      item["name"],
					"arguments": "",
					"status":    "in_progress",
				}
				emit("response.output_item.added", map[string]any{
					"output_index": itemIdx,
					"item":         pending,
				})

				args, _ := item["arguments"].(string)
				emit("response.function_call_arguments.delta", map[string]any{
					"item_id":      itemID,
					"output_index": itemIdx,
					"delta":        args,
				})
				emit("response.function_call_arguments.done", map[string]any{
					"item_id":      itemID,
					"output_index": itemIdx,
					"arguments":    args,
				})
				emit("response.output_item.done", map[string]any{
					"output_index": itemIdx,
					"item":         item,
				})
			case "message":
				pending := map[string]any{
					"type":    "message",
					"id":      item["id"],
					"role":    "assistant",
					"status":  "in_progress",
					"content": []any{},
				}
				emit("response.output_item.added", map[string]any{
					"output_index": itemIdx,
					"item":         pending,
				})

				if content, ok := item["content"].([]map[string]any); ok {
					for ci, cp := range content {
						eventFields := map[string]any{
							"item_id":       itemID,
							"output_index":  itemIdx,
							"content_index": ci,
						}
						emit("response.content_part.added", map[string]any{
							"item_id":       eventFields["item_id"],
							"output_index":  eventFields["output_index"],
							"content_index": eventFields["content_index"],
							"part": map[string]any{
								"type":        "output_text",
								"text":        "",
								"annotations": []any{},
							},
						})
						cpText, _ := cp["text"].(string)
						emit("response.output_text.delta", map[string]any{
							"item_id":       eventFields["item_id"],
							"output_index":  eventFields["output_index"],
							"content_index": eventFields["content_index"],
							"delta":         cpText,
						})
						emit("response.output_text.done", map[string]any{
							"item_id":       eventFields["item_id"],
							"output_index":  eventFields["output_index"],
							"content_index": eventFields["content_index"],
							"text":          cpText,
						})
						emit("response.content_part.done", map[string]any{
							"item_id":       eventFields["item_id"],
							"output_index":  eventFields["output_index"],
							"content_index": eventFields["content_index"],
							"part":          cp,
						})
					}
				}

				emit("response.output_item.done", map[string]any{
					"output_index": itemIdx,
					"item":         item,
				})
			}
		}

		emit("response.completed", map[string]any{
			"response": withStatus("completed", outputItems, usage),
		})
	} else {
		writeJSON(w, http.StatusOK, map[string]any{
			"id":         rid,
			"object":     "response",
			"created_at": time.Now().Unix(),
			"status":     "completed",
			"model":      resolved.Name,
			"output":     outputItems,
			"usage": map[string]any{
				"input_tokens":  promptTokens,
				"output_tokens": outputTokens,
				"total_tokens":  promptTokens + outputTokens,
			},
		})
		a.usageLogger("/v1/responses", stream, false, promptTokens, outputTokens)
	}
}
