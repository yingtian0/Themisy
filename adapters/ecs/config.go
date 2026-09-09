package ecs

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

type TargetKey struct {
	Service     string
	Environment string
}

type TaskDefinition struct {
	ARN                     string
	ExpectedTaskDefinitions []string
}

// Target is trusted Runner-local configuration. None of these provider
// resource identifiers are accepted from an Action Grant.
type Target struct {
	Region                 string
	ClusterARN             string
	ServiceARN             string
	TaskDefinitions        map[string]TaskDefinition // artifact digest -> trusted task definition
	RollbackTaskDefinition string
}

type Config struct{ Targets map[TargetKey]Target }

func (c Config) Validate() error {
	if len(c.Targets) == 0 {
		return errors.New("at least one ECS target is required")
	}
	for key, target := range c.Targets {
		if strings.TrimSpace(key.Service) == "" || strings.TrimSpace(key.Environment) == "" || target.Region == "" || target.ClusterARN == "" || target.ServiceARN == "" || len(target.TaskDefinitions) == 0 {
			return fmt.Errorf("ECS target %s/%s is incomplete", key.Service, key.Environment)
		}
		for digest, definition := range target.TaskDefinitions {
			if !validDigest(digest) || definition.ARN == "" || len(definition.ExpectedTaskDefinitions) == 0 {
				return fmt.Errorf("ECS task definition mapping for %s/%s is incomplete", key.Service, key.Environment)
			}
		}
	}
	return nil
}

func validDigest(value string) bool {
	if !strings.HasPrefix(value, "sha256:") || len(value) != len("sha256:")+64 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil
}
