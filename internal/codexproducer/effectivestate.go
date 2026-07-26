package codexproducer

import (
	"errors"

	"github.com/swemonstro/aurora/internal/instancepresence"
	"github.com/swemonstro/aurora/internal/producerprotocol"
)

// ErrUnsupportedEffectiveState mirrors ErrUnsupportedHookState for the
// instancepresence.EffectiveState enum used by hookadapter.IngressObservation.
var ErrUnsupportedEffectiveState = errors.New("codexproducer: unsupported effective state")

// MapEffectiveState converts instancepresence.EffectiveState (the type
// hookadapter.IngressObservation.EffectiveState carries) to
// producerprotocol.State. The two enums share identical wire values by
// construction; this function exists so no package relies on an unchecked
// string cast between them.
func MapEffectiveState(state instancepresence.EffectiveState) (producerprotocol.State, error) {
	switch state {
	case instancepresence.StateIdle:
		return producerprotocol.StateIdle, nil
	case instancepresence.StateWorking:
		return producerprotocol.StateWorking, nil
	case instancepresence.StateAttention:
		return producerprotocol.StateAttention, nil
	case instancepresence.StateError:
		return producerprotocol.StateError, nil
	default:
		return "", ErrUnsupportedEffectiveState
	}
}
