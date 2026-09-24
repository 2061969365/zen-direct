// nonstream.go: 非流客户端适配。
// FreeTier 门禁实测要求上游请求体 stream:true（否则 403 FreeTierError，
// "can only be used from within OpenCode"）；客户端 stream:false/缺省时，
// 网关仍强制上游流式，读完整 SSE 聚合为最终 JSON 再回写（同 oc2api 思路）。
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
)

// looksLikeSSE 按正文行首 data:/event: 判定（不看 Content-Type：
// 单测 mock 常用 text/event-stream 塞纯 JSON，反之亦有）。
func looksLikeSSE(b []byte) bool {
	for _, line := range bytes.Split(b, []byte("\n")) {
		t := bytes.TrimSpace(line)
		if bytes.HasPrefix(t, []byte("data:")) || bytes.HasPrefix(t, []byte("event:")) {
			return true
		}
	}
	return false
}

// assembleNonStreamFromSSE 把上游 SSE 聚合成非流最终体（按上游格式）。
func assembleNonStreamFromSSE(raw []byte, target Format, model string) (map[string]any, error) {
	if target == FmtChat {
		return sseToChatCompletion(raw, model)
	}
	return sseToResponsesFinal(raw)
}

// sseDataPayload 取 data: 行负载；[DONE]/空行返回 ok=false。
func sseDataPayload(line []byte) (string, bool) {
	t := bytes.TrimSpace(line)
	if !bytes.HasPrefix(t, []byte("data:")) {
		return "", false
	}
	p := string(bytes.TrimSpace(t[len("data:"):]))
	if len(p) == 0 || p == "[DONE]" {
		return "", false
	}
	return p, true
}

// sseToResponsesFinal 取最后一个终态帧（completed/incomplete/failed）的 response。
// 无终态 = 半截流，报错走 502（harness 按 TRANSPORT/TIMEOUT 重试）。
func sseToResponsesFinal(raw []byte) (map[string]any, error) {
	var final map[string]any
	for _, line := range bytes.Split(raw, []byte("\n")) {
		payload, ok := sseDataPayload(line)
		if !ok {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			continue
		}
		typ, _ := ev["type"].(string)
		switch typ {
		case "response.completed", "response.incomplete", "response.failed":
			if resp, ok := ev["response"].(map[string]any); ok {
				final = resp
			}
		}
	}
	if final == nil {
		return nil, errors.New("upstream SSE missing terminal frame")
	}
	return final, nil
}

// sseToChatCompletion 聚合 OpenAI chat 流式 chunk → chat.completion JSON。
func sseToChatCompletion(raw []byte, model string) (map[string]any, error) {
	type tcAcc struct {
		id, typ, name string
		args          strings.Builder
	}
	var id, role, finish string
	var created any
	var content, reasoning strings.Builder
	toolCalls := map[int]*tcAcc{}
	var usage any
	sawChoice := false

	for _, line := range bytes.Split(raw, []byte("\n")) {
		payload, ok := sseDataPayload(line)
		if !ok {
			continue
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		if v, ok := chunk["id"].(string); ok && v != "" {
			id = v
		}
		if v, ok := chunk["created"]; ok {
			created = v
		}
		if v, ok := chunk["usage"]; ok && v != nil {
			usage = v
		}
		choices, _ := chunk["choices"].([]any)
		for _, c := range choices {
			ch, ok := c.(map[string]any)
			if !ok {
				continue
			}
			sawChoice = true
			if f := asStr(ch["finish_reason"]); f != "" {
				finish = f
			}
			delta, _ := ch["delta"].(map[string]any)
			if delta == nil {
				continue
			}
			if r := asStr(delta["role"]); r != "" {
				role = r
			}
			if t := asStr(delta["content"]); t != "" {
				content.WriteString(t)
			}
			if t := asStr(delta["reasoning_content"]); t != "" {
				reasoning.WriteString(t)
			}
			for i, rawTC := range asArr(delta["tool_calls"]) {
				tm, ok := rawTC.(map[string]any)
				if !ok {
					continue
				}
				idx := i
				if n, ok := tm["index"].(float64); ok {
					idx = int(n)
				}
				acc := toolCalls[idx]
				if acc == nil {
					acc = &tcAcc{}
					toolCalls[idx] = acc
				}
				if v := asStr(tm["id"]); v != "" {
					acc.id = v
				}
				if v := asStr(tm["type"]); v != "" {
					acc.typ = v
				}
				if fn, ok := tm["function"].(map[string]any); ok {
					if v := asStr(fn["name"]); v != "" {
						acc.name = v
					}
					acc.args.WriteString(asStr(fn["arguments"]))
				}
			}
		}
	}
	if !sawChoice {
		return nil, errors.New("upstream chat SSE empty")
	}
	if finish == "" {
		return nil, errors.New("upstream chat SSE truncated")
	}
	if role == "" {
		role = "assistant"
	}
	msg := map[string]any{"role": role, "content": content.String()}
	if reasoning.Len() > 0 {
		msg["reasoning_content"] = reasoning.String()
	}
	if len(toolCalls) > 0 {
		// 按 index 升序稳定输出。
		maxIdx := 0
		for i := range toolCalls {
			if i > maxIdx {
				maxIdx = i
			}
		}
		var tcs []any
		for i := 0; i <= maxIdx; i++ {
			acc, ok := toolCalls[i]
			if !ok {
				continue
			}
			typ := acc.typ
			if typ == "" {
				typ = "function"
			}
			tcs = append(tcs, map[string]any{
				"id": acc.id, "type": typ,
				"function": map[string]any{"name": acc.name, "arguments": acc.args.String()},
			})
		}
		if len(tcs) > 0 {
			// 有工具调用时 content 空串按 OpenAI 惯例置 null。
			if content.Len() == 0 {
				msg["content"] = nil
			}
			msg["tool_calls"] = tcs
		}
	}
	out := map[string]any{
		"id": id, "object": "chat.completion",
		"created": created, "model": model,
		"choices": []any{map[string]any{
			"index": 0, "message": msg, "finish_reason": finish,
		}},
	}
	if usage != nil {
		out["usage"] = usage
	}
	return out, nil
}
