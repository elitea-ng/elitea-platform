package conversations

import "time"

type Conversation struct {
	ID           string    `json:"id"`
	UUID         string    `json:"uuid"`
	ProjectID    string    `json:"project_id"`
	Name         string    `json:"name"`
	Description  string    `json:"description,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	CreatedBy    string    `json:"created_by"`
	MessageCount int       `json:"message_count"`
}

type Message struct {
	ID             string         `json:"id"`
	GroupUID       string         `json:"group_uid"`
	ConversationID string         `json:"conversation_id"`
	Role           string         `json:"role"`
	Content        string         `json:"content"`
	ContentType    string         `json:"content_type,omitempty"`
	IsStreaming    bool           `json:"is_streaming"`
	ParticipantID  string         `json:"participant_id,omitempty"`
	Metadata       map[string]any `json:"metadata,omitempty"`
	CreatedAt      time.Time      `json:"created_at"`
}

type Participant struct {
	ID             string `json:"id"`
	ConversationID string `json:"conversation_id"`
	EntityType     string `json:"entity_type"`
	EntityID       string `json:"entity_id"`
	VersionID      string `json:"version_id,omitempty"`
	Name           string `json:"name"`
	Icon           string `json:"icon,omitempty"`
}

type Canvas struct {
	ID             string    `json:"id"`
	UUID           string    `json:"uuid"`
	ConversationID string    `json:"conversation_id"`
	Title          string    `json:"title"`
	Content        string    `json:"content"`
	Language       string    `json:"language,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type ListRequest struct {
	ProjectID string `json:"-"`
	Page      int    `json:"page,omitempty"`
	PageSize  int    `json:"page_size,omitempty"`
}

type ListResponse struct {
	Items      []Conversation `json:"items"`
	Total      int            `json:"total"`
	Page       int            `json:"page"`
	PageSize   int            `json:"page_size"`
	TotalPages int            `json:"total_pages"`
}

type CreateRequest struct {
	ProjectID    string   `json:"-"`
	Name         string   `json:"name"`
	Description  string   `json:"description,omitempty"`
	Participants []string `json:"participants,omitempty"`
}

type SendMessageRequest struct {
	ProjectID      string         `json:"-"`
	ConversationID string         `json:"-"`
	Content        string         `json:"content"`
	ContentType    string         `json:"content_type,omitempty"`
	Variables      map[string]any `json:"variables,omitempty"`
	Stream         bool           `json:"stream,omitempty"`
}

type SendMessageResponse struct {
	MessageGroupUID string `json:"message_group_uid"`
	IsStreaming     bool   `json:"is_streaming"`
}

type MessagesListRequest struct {
	ProjectID      string `json:"-"`
	ConversationID string `json:"-"`
	Page           int    `json:"page,omitempty"`
	PageSize       int    `json:"page_size,omitempty"`
}

type MessagesListResponse struct {
	Items      []Message `json:"items"`
	Total      int       `json:"total"`
	Page       int       `json:"page"`
	PageSize   int       `json:"page_size"`
	TotalPages int       `json:"total_pages"`
}
