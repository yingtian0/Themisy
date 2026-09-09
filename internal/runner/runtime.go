package runner

import (
	"errors"
	"sync/atomic"

	"themisy/pkg/protocol"
)

var (
	ErrRunnerDraining   = errors.New("runner is draining")
	ErrRunnerFrozen     = errors.New("runner is frozen")
	ErrRunnerAtCapacity = errors.New("runner is at capacity")
)

type AdmissionGate interface {
	Begin() error
	End()
}

// RuntimeState is shared by heartbeat reporting and the final write gate. A
// Control Plane mode update therefore takes effect before credentials are
// acquired even if a Grant was already delivered.
type RuntimeState struct {
	mode     atomic.Int32
	capacity atomic.Int64
	inFlight atomic.Int64
}

func NewRuntimeState(capacity int) *RuntimeState {
	state := &RuntimeState{}
	state.SetCapacity(capacity)
	return state
}

func (s *RuntimeState) SetCapacity(capacity int) {
	if s == nil {
		return
	}
	if capacity < 0 {
		capacity = 0
	}
	s.capacity.Store(int64(capacity))
}

func (s *RuntimeState) SetMode(mode protocol.RunnerMode) {
	if s == nil {
		return
	}
	switch mode {
	case protocol.RunnerModeActive:
		s.mode.Store(0)
	case protocol.RunnerModeDraining:
		s.mode.Store(1)
	default:
		s.mode.Store(2)
	}
}

func (s *RuntimeState) Mode() protocol.RunnerMode {
	if s == nil {
		return protocol.RunnerModeFrozen
	}
	switch s.mode.Load() {
	case 1:
		return protocol.RunnerModeDraining
	case 2:
		return protocol.RunnerModeFrozen
	default:
		return protocol.RunnerModeActive
	}
}

func (s *RuntimeState) Accepting() bool {
	return s != nil && s.Mode() == protocol.RunnerModeActive && s.inFlight.Load() < s.capacity.Load()
}

func (s *RuntimeState) Snapshot() (capacity, inFlight int) {
	if s == nil {
		return 0, 0
	}
	return int(s.capacity.Load()), int(s.inFlight.Load())
}

func (s *RuntimeState) Begin() error {
	if s == nil {
		return ErrRunnerFrozen
	}
	for {
		switch s.Mode() {
		case protocol.RunnerModeDraining:
			return ErrRunnerDraining
		case protocol.RunnerModeFrozen:
			return ErrRunnerFrozen
		}
		current := s.inFlight.Load()
		if current >= s.capacity.Load() {
			return ErrRunnerAtCapacity
		}
		if s.inFlight.CompareAndSwap(current, current+1) {
			return nil
		}
	}
}

func (s *RuntimeState) End() {
	if s == nil {
		return
	}
	for {
		current := s.inFlight.Load()
		if current == 0 || s.inFlight.CompareAndSwap(current, current-1) {
			return
		}
	}
}
