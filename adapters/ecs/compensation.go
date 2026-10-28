package ecs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"themisy/pkg/adapter"
)

// CompensationTargetHash identifies the immutable customer-reviewed predecessor
// configuration without accepting provider identifiers from a Grant.
func (a *Adapter) CompensationTargetHash(key adapter.Target) (string, error) {
	target, ok := a.config.Targets[TargetKey{Service: key.Service, Environment: key.Environment}]
	if !ok {
		return "", errors.New("trusted predecessor unavailable")
	}
	return HashCompensationTarget(key, target)
}

func HashCompensationTarget(key adapter.Target, target Target) (string, error) {
	if key.Service == "" || key.Environment == "" || target.Region == "" || target.ClusterARN == "" || target.ServiceARN == "" || target.RollbackTaskDefinition == "" || !validContainers(target.RollbackContainerDigests) {
		return "", errors.New("trusted predecessor unavailable")
	}
	raw, err := json.Marshal(struct {
		Version        string            `json:"version"`
		Target         adapter.Target    `json:"target"`
		Region         string            `json:"region"`
		Cluster        string            `json:"cluster"`
		Service        string            `json:"service"`
		TaskDefinition string            `json:"task_definition"`
		Containers     map[string]string `json:"containers"`
	}{"themisy.ecs-compensation-target/v1", key, target.Region, target.ClusterARN, target.ServiceARN, target.RollbackTaskDefinition, target.RollbackContainerDigests})
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(hash[:]), nil
}
