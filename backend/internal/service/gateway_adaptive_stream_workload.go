package service

import "github.com/tidwall/gjson"

type adaptiveWorkload uint16

const (
	adaptiveWorkUnknown adaptiveWorkload = 0
	adaptiveWorkText    adaptiveWorkload = 1 << (iota - 1)
	adaptiveWorkDeclaredTools
	adaptiveWorkToolHistory
	adaptiveWorkState
	adaptiveWorkUnexpectedTools
	adaptiveWorkRewritten
	adaptiveWorkInstructions
	adaptiveWorkToolStream
	adaptiveWorkToolStreamTrue
	adaptiveWorkReasoningText
	adaptiveWorkCache
	adaptiveWorkReasoningOpaque
)

const (
	adaptiveWorkSortable = adaptiveWorkText | adaptiveWorkDeclaredTools | adaptiveWorkInstructions |
		adaptiveWorkToolStream | adaptiveWorkToolStreamTrue | adaptiveWorkReasoningText
	adaptiveWorkCompat = adaptiveWorkInstructions | adaptiveWorkToolStream | adaptiveWorkToolStreamTrue |
		adaptiveWorkReasoningText | adaptiveWorkCache | adaptiveWorkReasoningOpaque
	adaptiveWorkKnown = adaptiveWorkSortable | adaptiveWorkToolHistory | adaptiveWorkState |
		adaptiveWorkUnexpectedTools | adaptiveWorkRewritten | adaptiveWorkCache | adaptiveWorkReasoningOpaque
)

type adaptiveWorkloadKey struct{}

type adaptiveChatShape struct {
	work    adaptiveWorkload
	nonText bool
}

// Continue scanning known media even after encountering an unsupported shape.
// Keep only recognized keys in seen, so duplicate tracking is bounded per object.
func adaptiveChatWorkload(payload gjson.Result, nativeSession bool) adaptiveChatShape {
	shape := adaptiveChatShape{work: adaptiveWorkText}
	supported, strictShape := payload.IsObject(), true
	var hasMessages, functionTool bool
	if nativeSession {
		shape.work |= adaptiveWorkState
	}
	seen := make(map[string]struct{})
	payload.ForEach(func(field, value gjson.Result) bool {
		switch field.Str {
		case "messages":
			hasMessages = true
			supported = value.IsArray() && supported
			nonempty := false
			value.ForEach(func(_, message gjson.Result) bool {
				nonempty = true
				messageOK, messageStrict := shape.inspectMessage(message)
				supported = supported && messageOK
				strictShape = strictShape && messageStrict
				return true
			})
			supported = supported && nonempty
		case "modalities":
			strictShape = strictShape && value.IsArray()
			for _, modality := range value.Array() {
				switch modality.Str {
				case "text":
				case "image", "audio", "video", "file":
					shape.nonText = true
				default:
					supported = false
				}
			}
		case "tools", "functions":
			tool, ok, strict := shape.inspectTools(value, field.Str == "tools")
			functionTool = functionTool || tool
			supported = supported && ok
			strictShape = strictShape && strict
		case "tool_choice", "function_call":
			ok, strict := shape.inspectToolChoice(value, field.Str == "tool_choice")
			supported = supported && ok
			strictShape = strictShape && strict
		case "reasoning":
			supported = adaptiveChatUniqueFields(value, "effort") && supported
		case "thinking":
			supported = adaptiveChatUniqueFields(value, "type", "budget_tokens") && supported
		case "stream_options":
			supported = adaptiveChatUniqueFields(value, "include_usage") && supported
		case "model", "stream", "service_tier", "parallel_tool_calls",
			"reasoning_effort", "reasoning_enabled",
			"max_tokens", "max_completion_tokens", "max_output_tokens", "temperature", "top_p", "top_k", "min_p",
			"presence_penalty", "frequency_penalty", "repetition_penalty", "stop", "n", "seed",
			"logit_bias", "logprobs", "top_logprobs", "response_format", "user", "store",
			"safety_identifier", "verbosity", "include_reasoning":
		case "prompt_cache_key", "prompt_cache_retention", "cache_control", "cachedContent", "cached_content",
			"session_id", "conversation_id", "previous_response_id", "conversation", "metadata":
			shape.work |= adaptiveWorkState
		case "file_ids":
			shape.work |= adaptiveWorkState
			shape.nonText = shape.nonText || len(value.Array()) != 0
		case "audio":
			shape.nonText = true
		case "instructions":
			shape.work |= adaptiveWorkInstructions
			supported = supported && value.Type == gjson.String
		case "tool_stream":
			shape.work |= adaptiveWorkToolStream
			if value.Type == gjson.True {
				shape.work |= adaptiveWorkToolStreamTrue
			}
			supported = supported && (value.Type == gjson.True || value.Type == gjson.False)
		case "prompt_cache_options":
			shape.work |= adaptiveWorkCache | adaptiveWorkState
			supported = supported && adaptiveChatCacheHint(value, false)
		default:
			supported = false
			return true
		}
		_, duplicate := seen[field.Str]
		seen[field.Str] = struct{}{}
		supported = supported && !duplicate
		return true
	})
	if !supported || !hasMessages || shape.nonText ||
		(shape.work&adaptiveWorkToolStreamTrue != 0 && !functionTool) ||
		(!strictShape && shape.work&adaptiveWorkCompat != 0) {
		shape.work = adaptiveWorkUnknown
	}
	return shape
}

func (shape *adaptiveChatShape) inspectTools(tools gjson.Result, modern bool) (functionTool, supported, strictShape bool) {
	supported, strictShape = tools.IsArray(), true
	for _, tool := range tools.Array() {
		fn := tool
		if modern {
			shape.inspectToolKind(tool)
			unique, known := adaptiveChatObjectShape(tool, "type", "function")
			supported = supported && unique && tool.Get("type").Str == "function"
			strictShape = strictShape && known
			fn = tool.Get("function")
		}
		unique, known := adaptiveChatObjectShape(fn, "name", "description", "parameters", "strict")
		valid := known && unique && fn.Get("name").Type == gjson.String && fn.Get("name").Str != ""
		supported = supported && unique
		strictShape = strictShape && valid
		functionTool = functionTool || (modern && valid && tool.Get("type").Str == "function")
		shape.work |= adaptiveWorkDeclaredTools
	}
	return functionTool, supported, strictShape
}

func (shape *adaptiveChatShape) inspectToolChoice(choice gjson.Result, modern bool) (supported, strictShape bool) {
	shape.work |= adaptiveWorkDeclaredTools
	if choice.Type == gjson.String {
		return true, true
	}
	if modern {
		shape.inspectToolKind(choice)
		unique, known := adaptiveChatObjectShape(choice, "type", "function")
		fnUnique, fnKnown := adaptiveChatObjectShape(choice.Get("function"), "name")
		return unique && fnUnique, known && fnKnown && choice.Get("type").Str == "function" &&
			choice.Get("function.name").Type == gjson.String
	}
	unique, known := adaptiveChatObjectShape(choice, "name")
	return unique, known && choice.Get("name").Type == gjson.String
}

func (shape *adaptiveChatShape) inspectToolKind(tool gjson.Result) {
	tool.ForEach(func(field, value gjson.Result) bool {
		switch field.Str {
		case "custom":
			shape.nonText = true
		case "type":
			switch value.Str {
			case "custom", "web_search", "web_search_preview", "code_execution", "code_interpreter", "x_search",
				"image_generation", "file_search", "computer_use", "mcp":
				shape.nonText = true
			}
		}
		return !shape.nonText
	})
}

func (shape *adaptiveChatShape) inspectMessage(message gjson.Result) (supported, strictShape bool) {
	supported, strictShape = message.IsObject(), true
	role := message.Get("role").Str
	switch role {
	case "tool", "function":
		shape.work |= adaptiveWorkToolHistory
	case "system", "developer", "user", "assistant":
	default:
		supported = false
	}
	seen := make(map[string]struct{})
	message.ForEach(func(field, value gjson.Result) bool {
		switch field.Str {
		case "role", "name", "reasoning", "reasoning_content", "refusal":
		case "content":
			if value.IsArray() {
				value.ForEach(func(_, part gjson.Result) bool {
					ok, strict := shape.inspectPart(part, true)
					supported = supported && ok
					strictShape = strictShape && strict
					return true
				})
			} else {
				supported = supported && (value.Type == gjson.String || value.Type == gjson.Null)
			}
		case "tool_calls":
			shape.work |= adaptiveWorkToolHistory
			for _, call := range value.Array() {
				shape.inspectToolKind(call)
				supported = adaptiveChatUniqueFields(call, "type", "custom") && supported
			}
		case "tool_call_id", "function_call":
			shape.work |= adaptiveWorkToolHistory
		case "reasoning_details", "encrypted_content", "reasoning_content_encrypted", "cache_control":
			shape.work |= adaptiveWorkState
		case "audio", "audio_id", "file_id", "attachments":
			shape.nonText = true
		case "reasoning_text":
			shape.work |= adaptiveWorkReasoningText
			supported = supported && role == "assistant" && value.Type == gjson.String
		case "reasoning_opaque":
			shape.work |= adaptiveWorkReasoningOpaque | adaptiveWorkState
			supported = supported && role == "assistant" && value.Type == gjson.String
		default:
			supported = false
			return true
		}
		_, duplicate := seen[field.Str]
		seen[field.Str] = struct{}{}
		supported = supported && !duplicate
		return true
	})
	return supported, strictShape
}

func (shape *adaptiveChatShape) inspectPart(part gjson.Result, allowToolResult bool) (supported, strictShape bool) {
	kind := part.Get("type").Str
	breakpoint := part.Get("prompt_cache_breakpoint")
	hasToolResult := false
	part.ForEach(func(field, value gjson.Result) bool {
		switch field.Str {
		case "type":
			switch value.Str {
			case "image", "image_url", "input_image", "output_image", "audio", "input_audio", "output_audio",
				"video", "video_url", "input_video", "file", "input_file", "file_reference":
				shape.nonText = true
			case "tool_result":
				hasToolResult = true
			}
		case "image_url", "image", "input_image", "audio", "input_audio", "video",
			"video_url", "file", "file_id", "file_data", "attachments":
			shape.nonText = true
		}
		return true
	})
	// Scan each result content even when another type value wins a first-value lookup.
	if hasToolResult && allowToolResult {
		shape.work |= adaptiveWorkToolHistory
		unique, known := adaptiveChatObjectShape(part, "type", "content", "tool_use_id", "is_error")
		supported = unique && kind == "tool_result" && !breakpoint.Exists()
		strictShape = known && part.Get("content").Exists()
		part.ForEach(func(field, content gjson.Result) bool {
			if field.Str != "content" {
				return true
			}
			if !content.IsArray() {
				strictShape = strictShape && content.Type == gjson.String
				return true
			}
			content.ForEach(func(_, item gjson.Result) bool {
				ok, strict := shape.inspectPart(item, false)
				strictShape = strictShape && ok && strict
				return true
			})
			return true
		})
		return supported, strictShape
	}
	switch kind {
	case "text", "input_text", "output_text":
		if part.Get("cache_control").Exists() {
			shape.work |= adaptiveWorkState
		}
		if breakpoint.Exists() {
			shape.work |= adaptiveWorkCache | adaptiveWorkState
		}
		unique, known := adaptiveChatObjectShape(part, "type", "text", "cache_control", "prompt_cache_breakpoint")
		return unique && (!breakpoint.Exists() || adaptiveChatCacheHint(breakpoint, true)),
			known && part.Get("text").Type == gjson.String
	}
	return false, false
}

func adaptiveChatCacheHint(value gjson.Result, breakpoint bool) bool {
	if breakpoint {
		unique, known := adaptiveChatObjectShape(value, "mode")
		return unique && known && value.Get("mode").Str == "explicit"
	}
	unique, known := adaptiveChatObjectShape(value, "ttl", "mode")
	ttl, mode := value.Get("ttl"), value.Get("mode")
	return unique && known &&
		(!ttl.Exists() || ttl.Type == gjson.String) && (!mode.Exists() || mode.Str == "explicit")
}

func adaptiveChatUniqueFields(value gjson.Result, fields ...string) bool {
	unique, _ := adaptiveChatObjectShape(value, fields...)
	return unique
}

// Check named members without retaining arbitrary object keys or inspecting opaque values.
func adaptiveChatObjectShape(value gjson.Result, fields ...string) (unique, known bool) {
	if !value.IsObject() {
		return true, false
	}
	var seen uint
	unique, known = true, true
	value.ForEach(func(field, _ gjson.Result) bool {
		for i, name := range fields {
			if field.Str == name {
				bit := uint(1) << i
				unique = seen&bit == 0
				seen |= bit
				return unique
			}
		}
		known = false
		return true
	})
	return unique, known
}
