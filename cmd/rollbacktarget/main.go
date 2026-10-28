// rollbacktarget hashes public customer-reviewed ECS predecessor configuration.
// It performs no credential acquisition, provider call or database operation.
package main

import (
	"flag"
	"fmt"
	"go.yaml.in/yaml/v3"
	"io"
	"os"
	"strings"
	"themisy/adapters/ecs"
	"themisy/pkg/adapter"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	path := flag.String("config", "", "public predecessor YAML file")
	flag.Parse()
	if *path == "" {
		return fmt.Errorf("config is required")
	}
	f, err := os.Open(*path)
	if err != nil {
		return err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil || len(raw) > 65536 {
		return fmt.Errorf("invalid predecessor configuration size")
	}
	var input struct {
		Service        string            `yaml:"service"`
		Environment    string            `yaml:"environment"`
		Region         string            `yaml:"region"`
		ClusterARN     string            `yaml:"cluster_arn"`
		ServiceARN     string            `yaml:"service_arn"`
		TaskDefinition string            `yaml:"rollback_task_definition"`
		Containers     map[string]string `yaml:"rollback_container_digests"`
	}
	d := yaml.NewDecoder(strings.NewReader(string(raw)))
	d.KnownFields(true)
	if err := d.Decode(&input); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return fmt.Errorf("trailing YAML")
	}
	hash, err := ecs.HashCompensationTarget(adapter.Target{Service: input.Service, Environment: input.Environment}, ecs.Target{Region: input.Region, ClusterARN: input.ClusterARN, ServiceARN: input.ServiceARN, RollbackTaskDefinition: input.TaskDefinition, RollbackContainerDigests: input.Containers})
	if err != nil {
		return err
	}
	fmt.Println(hash)
	return nil
}
