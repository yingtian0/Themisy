// policybundle runs in the independently approved Policy publishing pipeline.
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
	"themisy/internal/grant"
	"themisy/internal/policy"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	input := flag.String("input", "", "unsigned reviewed Policy JSON")
	trust := flag.String("trust-bundle", "", "customer Policy trust bundle")
	tenant := flag.String("tenant", "", "tenant bound to the Policy trust bundle")
	kmsID := flag.String("kms-key-id", "", "independent Policy KMS Ed25519 key")
	devFile := flag.String("development-key-file", "", "protected local fixture key")
	devID := flag.String("development-key-id", "", "fixture key ID")
	flag.Parse()
	if *input == "" || *trust == "" || *tenant == "" || (*kmsID == "") == (*devFile == "") {
		return fmt.Errorf("input, tenant, trust-bundle and exactly one KMS or development signer are required")
	}
	file, err := os.Open(*input)
	if err != nil {
		return err
	}
	defer file.Close()
	var bundle policy.Bundle
	decoder := json.NewDecoder(io.LimitReader(file, 2<<20))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&bundle) != nil || decoder.Decode(new(any)) != io.EOF {
		return fmt.Errorf("invalid bounded Policy JSON")
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
		signer, _, err = grant.LoadDevelopmentSigner(*devFile, *devID)
		if err != nil {
			return err
		}
	}
	signed, err := policy.SignBundle(ctx, signer, bundle)
	if err != nil {
		return err
	}
	keys := policy.FileBundleKeys{Trust: grant.FileKeys{Path: *trust, Issuer: bundle.Issuer, Tenant: *tenant}}
	if _, err := policy.NewVerifiedOPA(ctx, signed, bundle.Issuer, keys, time.Now().UTC()); err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(signed)
}
