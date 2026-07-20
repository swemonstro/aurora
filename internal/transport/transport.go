package transport

import (
	"context"

	"github.com/swemonstro/aurora/internal/status"
)

type Sender interface {
	Send(context.Context, status.Message) error
}
