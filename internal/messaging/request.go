package messaging

import (
	"context"
	"errors"
	"time"
)

const MaxRequestDuration = 30 * time.Second

var ErrRequestTimeout = errors.New("message request timed out")

// RequestReply is implemented by the RabbitMQ adapter; callers cannot exceed the shared deadline.
type RequestReply interface {
	Request(context.Context, string, []byte) ([]byte, error)
}

func BoundedRequest(ctx context.Context, transport RequestReply, queue string, payload []byte, timeout time.Duration) ([]byte, error) {
	if transport == nil || queue == "" || timeout <= 0 || timeout > MaxRequestDuration {
		return nil, errors.New("transport, queue, and timeout up to 30s are required")
	}
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	response, err := transport.Request(requestCtx, queue, payload)
	if errors.Is(err, context.DeadlineExceeded) {
		return nil, ErrRequestTimeout
	}
	return response, err
}
