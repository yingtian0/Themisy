package protocol

import "time"

const RunnerTransportVersionV1Alpha1 = "themisy.runner-transport/v1alpha1"

type RunnerMode string

const (
	RunnerModeActive   RunnerMode = "ACTIVE"
	RunnerModeDraining RunnerMode = "DRAINING"
	RunnerModeFrozen   RunnerMode = "FROZEN"
)

type RunnerHeartbeat struct {
	ProtocolVersion string    `json:"protocol_version"`
	ObservedAt      time.Time `json:"observed_at"`
	Capacity        int       `json:"capacity"`
	InFlight        int       `json:"in_flight"`
}

type RunnerControl struct {
	ProtocolVersion   string     `json:"protocol_version"`
	Mode              RunnerMode `json:"mode"`
	HeartbeatInterval string     `json:"heartbeat_interval"`
}
