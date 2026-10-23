package configurations

import (
	"time"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
)

// CurrentConfigurationDTO is the exact current JSON field projection. It stays
// separate from the prototype Configuration type in handler.go so no mounted
// route can accidentally claim row/DTO parity before the remaining registry,
// secret, event, pin, options, and error behavior has moved.
type CurrentConfigurationDTO struct {
	ID          int32           `json:"id"`
	UUID        string          `json:"uuid"`
	ProjectID   int32           `json:"project_id"`
	EliteaTitle string          `json:"elitea_title"`
	Label       *string         `json:"label"`
	Type        string          `json:"type"`
	Section     string          `json:"section"`
	Data        map[string]any  `json:"data"`
	Meta        map[string]any  `json:"meta"`
	Shared      bool            `json:"shared"`
	StatusOK    bool            `json:"status_ok"`
	StatusLogs  *string         `json:"status_logs"`
	Source      string          `json:"source"`
	AuthorID    *int32          `json:"author_id"`
	CreatedAt   string          `json:"created_at"`
	UpdatedAt   *string         `json:"updated_at"`
	IsPinned    bool            `json:"is_pinned"`
	Options     *map[string]any `json:"options,omitempty"`
}

type CurrentConfigurationPageDTO struct {
	Total  int64                     `json:"total"`
	Items  []CurrentConfigurationDTO `json:"items"`
	Offset int                       `json:"offset"`
	Limit  int                       `json:"limit"`
}

type CurrentConfigurationListDTO struct {
	Total  int64                        `json:"total"`
	Items  []CurrentConfigurationDTO    `json:"items"`
	Offset int                          `json:"offset"`
	Limit  int                          `json:"limit"`
	Shared *CurrentConfigurationPageDTO `json:"shared,omitempty"`
}

func newCurrentConfigurationDTO(configuration configurationapp.CurrentConfiguration) CurrentConfigurationDTO {
	return CurrentConfigurationDTO{
		ID:          configuration.ID,
		UUID:        configuration.UUID,
		ProjectID:   configuration.ProjectID,
		EliteaTitle: configuration.EliteaTitle,
		Label:       configuration.Label,
		Type:        configuration.Type,
		Section:     configuration.Section,
		Data:        nonNilCurrentJSONObject(configuration.Data),
		Meta:        nonNilCurrentJSONObject(configuration.Meta),
		Shared:      configuration.Shared,
		StatusOK:    configuration.StatusOK,
		StatusLogs:  configuration.StatusLogs,
		Source:      configuration.Source,
		AuthorID:    configuration.AuthorID,
		CreatedAt:   currentConfigurationTimestamp(configuration.CreatedAt),
		UpdatedAt:   currentConfigurationOptionalTimestamp(configuration.UpdatedAt),
		IsPinned:    configuration.IsPinned,
		Options:     configuration.Options,
	}
}

func newCurrentConfigurationListDTO(result configurationapp.CurrentConfigurationListResult) CurrentConfigurationListDTO {
	dto := CurrentConfigurationListDTO{
		Total:  result.Total,
		Items:  newCurrentConfigurationItemsDTO(result.Items),
		Offset: result.Offset,
		Limit:  result.Limit,
	}
	if result.Shared != nil {
		dto.Shared = &CurrentConfigurationPageDTO{
			Total:  result.Shared.Total,
			Items:  newCurrentConfigurationItemsDTO(result.Shared.Items),
			Offset: result.Shared.Offset,
			Limit:  result.Shared.Limit,
		}
	}
	return dto
}

func newCurrentConfigurationItemsDTO(configurations []configurationapp.CurrentConfiguration) []CurrentConfigurationDTO {
	items := make([]CurrentConfigurationDTO, 0, len(configurations))
	for _, configuration := range configurations {
		items = append(items, newCurrentConfigurationDTO(configuration))
	}
	return items
}

func nonNilCurrentJSONObject(value map[string]any) map[string]any {
	if value == nil {
		return map[string]any{}
	}
	return value
}

// Current PostgreSQL columns are timestamp without time zone. Pydantic's JSON
// mode serializes those Python datetimes as naive ISO-8601 values: seconds
// only when microseconds are zero, otherwise exactly six fractional digits.
// Formatting the pgx time value explicitly avoids adding a synthetic trailing
// "Z" that is absent from the current HTTP contract.
func currentConfigurationTimestamp(value time.Time) string {
	const secondsLayout = "2006-01-02T15:04:05"
	if value.Nanosecond() == 0 {
		return value.Format(secondsLayout)
	}
	return value.Format(secondsLayout + ".000000")
}

func currentConfigurationOptionalTimestamp(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := currentConfigurationTimestamp(*value)
	return &formatted
}
