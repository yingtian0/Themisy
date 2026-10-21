package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"themisy/internal/contract"
	"themisy/internal/domain"
	"themisy/internal/planner"
	"themisy/internal/profile"
)

const offlineNotice = "OFFLINE ONLY: synthetic local context; no identity/evidence verification, approval, grant issuance, credentials, or adapter writes. This plan does not authorize execution."

// This path depends only on file loaders and the deterministic planner. It must
// never construct an application service, credential broker, or executor.
func runOfflinePlan(args []string, stdout, stderr io.Writer) error {
	fmt.Fprintln(stderr, offlineNotice)
	flags := flag.NewFlagSet("offline-plan", flag.ContinueOnError)
	flags.SetOutput(stderr)
	contractsDir := flags.String("contracts", "examples/contracts", "local Service Contract directory")
	profilesDir := flags.String("profiles", "examples/profiles", "local Release Profile directory")
	at := flags.String("at", "", "fixed planning time (RFC3339); defaults to current UTC time")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("offline-plan requires exactly one ReleaseIntent JSON file; options precede the file")
	}
	now := time.Now().UTC()
	if *at != "" {
		var err error
		now, err = time.Parse(time.RFC3339, *at)
		if err != nil {
			return fmt.Errorf("-at: %w", err)
		}
	}
	contracts, err := contract.LoadDir(*contractsDir)
	if err != nil {
		return err
	}
	profiles, err := profile.LoadDir(*profilesDir)
	if err != nil {
		return err
	}
	p, err := planner.NewFromContracts(contracts, profiles, planner.Options{Now: func() time.Time { return now }})
	if err != nil {
		return err
	}
	data, err := os.ReadFile(flags.Arg(0))
	if err != nil {
		return fmt.Errorf("read intent: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var intent domain.ReleaseIntent
	if err := decoder.Decode(&intent); err != nil {
		return fmt.Errorf("decode ReleaseIntent: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("ReleaseIntent must contain exactly one JSON document")
	}
	plan, err := p.PlanIntent(intent)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(struct {
		Mode   string             `json:"mode"`
		Notice string             `json:"notice"`
		Plan   domain.ReleasePlan `json:"plan"`
	}{"offline", offlineNotice, plan})
}
