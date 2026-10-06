// planmanifest runs in the independent approval/provisioning environment, not
// in the Grant dispatcher. Its output is provisioned to the Runner as a file.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"os"
	"themisy/internal/application"
	"themisy/internal/grant"
	postgresstore "themisy/internal/store/postgres"
	"themisy/pkg/protocol"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	runID := flag.String("run", "", "approved run ID")
	step := flag.String("step", "", "approved step ID")
	issuer := flag.String("issuer", "", "independent approval issuer")
	revision := flag.Int64("revision", 0, "monotonically increasing plan revision")
	capability := flag.String("capability", string(protocol.CapabilityDeploy), "typed operation")
	external := flag.String("external-execution-id", "", "original execution for rollback")
	kmsID := flag.String("kms-key-id", "", "independent approval KMS Ed25519 key")
	devFile := flag.String("development-key-file", "", "protected development key; never use for production")
	devID := flag.String("development-key-id", "", "development key ID")
	flag.Parse()
	if *runID == "" || *step == "" || *issuer == "" || *revision < 1 || (*kmsID == "") == (*devFile == "") {
		return fmt.Errorf("run, step, issuer, revision and exactly one KMS or development signer are required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var signer grant.Signer
	if *kmsID != "" {
		cfg, err := awsconfig.LoadDefaultConfig(ctx)
		if err != nil {
			return err
		}
		signer, err = grant.NewAWSKMSSigner(kms.NewFromConfig(cfg), *kmsID)
		if err != nil {
			return err
		}
	} else {
		var err error
		signer, _, err = grant.LoadDevelopmentSigner(*devFile, *devID)
		if err != nil {
			return err
		}
	}
	databaseURL := os.Getenv("THEMISY_DATABASE_URL")
	if databaseURL == "" {
		return fmt.Errorf("THEMISY_DATABASE_URL is required")
	}
	st, err := postgresstore.New(ctx, databaseURL)
	if err != nil {
		return err
	}
	defer st.Close()
	p, err := application.ApprovedManifest(ctx, st, signer, *issuer, application.GrantIssueRequest{RunID: *runID, StepID: *step, Capability: protocol.Capability(*capability), ExternalExecutionID: *external}, *revision, time.Now().UTC())
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode([]protocol.PlanManifest{p})
}
