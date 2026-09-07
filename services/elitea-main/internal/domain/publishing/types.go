package publishing

import "time"

type PublishedApplication struct {
	ID            string    `json:"id"`
	ApplicationID string    `json:"application_id"`
	VersionID     string    `json:"version_id"`
	VersionName   string    `json:"version_name"`
	Name          string    `json:"name"`
	Description   string    `json:"description,omitempty"`
	AuthorID      string    `json:"author_id"`
	AuthorName    string    `json:"author_name"`
	Icon          string    `json:"icon,omitempty"`
	Tags          []string  `json:"tags,omitempty"`
	Category      string    `json:"category,omitempty"`
	ForkCount     int       `json:"fork_count"`
	PublishedAt   time.Time `json:"published_at"`
}

type Author struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Email        string `json:"email,omitempty"`
	PublishCount int    `json:"publish_count"`
	ForkCount    int    `json:"fork_count"`
}

type PublishRequest struct {
	ProjectID string `json:"-"`
	VersionID string `json:"-"`
}

type UnpublishRequest struct {
	ProjectID string `json:"-"`
	VersionID string `json:"-"`
}

type ForkRequest struct {
	ProjectID     string `json:"-"`
	ApplicationID string `json:"application_id"`
	VersionName   string `json:"version_name,omitempty"`
}

type ForkResponse struct {
	ApplicationID string `json:"application_id"`
	VersionID     string `json:"version_id"`
}

type ListRequest struct {
	Page     int    `json:"page,omitempty"`
	PageSize int    `json:"page_size,omitempty"`
	Search   string `json:"search,omitempty"`
	Category string `json:"category,omitempty"`
	Tags     string `json:"tags,omitempty"`
}

type ListResponse struct {
	Items      []PublishedApplication `json:"items"`
	Total      int                    `json:"total"`
	Page       int                    `json:"page"`
	PageSize   int                    `json:"page_size"`
	TotalPages int                    `json:"total_pages"`
}

type Recommendation struct {
	Application PublishedApplication `json:"application"`
	Score       float64              `json:"score"`
	Reason      string               `json:"reason,omitempty"`
}
