package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/swemonstro/aurora/internal/status"
)

type JSONSender struct {
	writer io.Writer
}

func NewJSONSender(writer io.Writer) (*JSONSender, error) {
	if writer == nil {
		return nil, fmt.Errorf("writer must not be nil")
	}

	return &JSONSender{writer: writer}, nil
}

func (s *JSONSender) Send(_ context.Context, message status.Message) error {
	encoder := json.NewEncoder(s.writer)
	encoder.SetEscapeHTML(false)

	if err := encoder.Encode(message); err != nil {
		return fmt.Errorf("encode status message: %w", err)
	}

	return nil
}
