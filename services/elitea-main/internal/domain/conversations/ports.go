package conversations

import (
	"context"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/predict"
)

type ChatService interface {
	SendMessage(ctx context.Context, req SendMessageRequest) (SendMessageResponse, error)
	SendMessageStream(ctx context.Context, req SendMessageRequest, send func(predict.StreamEvent) error) error
}
