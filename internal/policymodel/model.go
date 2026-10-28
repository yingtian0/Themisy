package policymodel

import (
	"errors"
	"themisy/pkg/protocol"
	"time"
)

type Layer string

const (
	LayerPlatform    Layer = "platform_mandatory"
	LayerEnvironment Layer = "environment"
	LayerTeam        Layer = "team"
	LayerService     Layer = "service"
)

type Module struct {
	Name   string `json:"name"`
	Layer  Layer  `json:"layer"`
	Source string `json:"source"`
}

type Bundle struct {
	SigningVersion string             `json:"signing_version,omitempty"`
	Version        string             `json:"version"`
	Issuer         string             `json:"issuer"`
	Compatibility  []string           `json:"compatibility"`
	IssuedAt       time.Time          `json:"issued_at"`
	ExpiresAt      time.Time          `json:"expires_at"`
	Modules        []Module           `json:"modules"`
	Hash           string             `json:"hash"`
	Signature      protocol.Signature `json:"signature"`
}

type Assignment struct {
	TenantID     string    `json:"tenant_id"`
	RunnerGroup  string    `json:"runner_group"`
	Revision     int64     `json:"revision"`
	BundleHash   string    `json:"bundle_hash"`
	PreviousHash string    `json:"previous_hash,omitempty"`
	UpdatedAt    time.Time `json:"updated_at"`
}
type Delivery struct {
	Assignment Assignment `json:"assignment"`
	Bundle     Bundle     `json:"bundle"`
}
type Acknowledgement struct {
	RunnerID   string    `json:"runner_id"`
	Revision   int64     `json:"revision"`
	BundleHash string    `json:"bundle_hash"`
	ObservedAt time.Time `json:"observed_at"`
}

var ErrDistribution = errors.New("POLICY_DISTRIBUTION_UNAVAILABLE")
var ErrAssignmentConflict = errors.New("POLICY_ASSIGNMENT_CONFLICT")
