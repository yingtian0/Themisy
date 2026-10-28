// buildprovenance runs in the trusted build workload, with its own signing key.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"io"
	"os"
	"themisy/internal/contextsnapshot"
	"themisy/internal/grant"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	input := flag.String("input", "", "trusted build output statement JSON")
	trust := flag.String("trust-bundle", "", "tenant-bound build issuer public trust")
	tenant := flag.String("tenant", "", "expected tenant")
	kmsID := flag.String("kms-key-id", "", "dedicated build KMS Ed25519 key")
	devFile := flag.String("development-key-file", "", "protected local fixture key")
	devID := flag.String("development-key-id", "", "fixture key ID")
	flag.Parse()
	if *input == "" || *trust == "" || *tenant == "" || (*kmsID == "") == (*devFile == "") {
		return fmt.Errorf("input, tenant, trust-bundle and exactly one KMS or development signer required")
	}
	file, err := os.Open(*input)
	if err != nil {
		return err
	}
	defer file.Close()
	var b contextsnapshot.BuildStatement
	decoder := json.NewDecoder(io.LimitReader(file, 64<<10))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&b) != nil || decoder.Decode(new(any)) != io.EOF {
		return fmt.Errorf("invalid bounded build statement")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var signer grant.Signer
	keyID := *devID
	if *kmsID != "" {
		cfg, err := awsconfig.LoadDefaultConfig(ctx)
		if err != nil {
			return err
		}
		signer, err = grant.NewAWSKMSSigner(kms.NewFromConfig(cfg), *kmsID)
		if err != nil {
			return err
		}
		keyID = *kmsID
	} else {
		signer, _, err = grant.LoadDevelopmentSigner(*devFile, *devID)
		if err != nil {
			return err
		}
	}
	signed, err := contextsnapshot.SignBuildStatement(ctx, signer, keyID, b)
	if err != nil {
		return err
	}
	if err := contextsnapshot.VerifyBuildStatement(ctx, signed, b.Issuer, *tenant, grant.FileKeys{Path: *trust, Issuer: b.Issuer, Tenant: *tenant}, time.Now().UTC()); err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(signed)
}
