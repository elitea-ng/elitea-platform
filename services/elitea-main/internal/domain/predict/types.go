package predict

import "time"

type Request struct {
	ProjectID string         `json:"-"`
	VersionID string         `json:"-"`
	Input     string         `json:"input"`
	Variables map[string]any `json:"variables,omitempty"`
	Stream    bool           `json:"stream,omitempty"`
	Mode      string         `json:"mode,omitempty"`
}

type Response struct {
	MessageGroupUID string         `json:"message_group_uid"`
	Content         string         `json:"content,omitempty"`
	IsStreaming     bool           `json:"is_streaming"`
	Usage           *Usage         `json:"usage,omitempty"`
	ToolCalls       []ToolCall     `json:"tool_calls,omitempty"`
	ChildMessages   []ChildMessage `json:"child_messages,omitempty"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type ToolCall struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Input    string `json:"input"`
	Output   string `json:"output,omitempty"`
	Status   string `json:"status"`
	Duration int64  `json:"duration_ms,omitempty"`
}

type ChildMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	AgentID string `json:"agent_id,omitempty"`
}

type StreamEvent struct {
	Type    string `json:"type"`
	Content string `json:"content,omitempty"`
	Done    bool   `json:"done,omitempty"`
}

type LLMRequest struct {
	ProjectID string         `json:"-"`
	Model     string         `json:"model"`
	Messages  []LLMMessage   `json:"messages"`
	Stream    bool           `json:"stream,omitempty"`
	Options   map[string]any `json:"options,omitempty"`
}

type LLMMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type LLMResponse struct {
	Content string `json:"content"`
	Model   string `json:"model"`
	Usage   *Usage `json:"usage,omitempty"`
}

type PipelineRunRequest struct {
	ProjectID string         `json:"-"`
	VersionID string         `json:"version_id"`
	Input     map[string]any `json:"input,omitempty"`
}

type PipelineRunResponse struct {
	TaskID    string    `json:"task_id"`
	Status    string    `json:"status"`
	StartedAt time.Time `json:"started_at"`
}

type PipelineStatus struct {
	TaskID    string `json:"task_id"`
	Status    string `json:"status"`
	Result    any    `json:"result,omitempty"`
	Error     string `json:"error,omitempty"`
	StartedAt string `json:"started_at,omitempty"`
	EndedAt   string `json:"ended_at,omitempty"`
}
