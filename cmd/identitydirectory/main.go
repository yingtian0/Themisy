// identitydirectory runs in the independent organization provisioning pipeline.
// Its input contains no IdP tokens; its signed output is provisioned locally.
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
	"themisy/internal/identity"
	"themisy/internal/store"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	input := flag.String("input", "", "unsigned Directory JSON")
	trust := flag.String("trust-bundle", "", "independent Directory issuer trust bundle")
	kmsID := flag.String("kms-key-id", "", "independent organization KMS Ed25519 key")
	devFile := flag.String("development-key-file", "", "protected local development key")
	devID := flag.String("development-key-id", "", "development key ID")
	flag.Parse()
	if *input == "" || *trust == "" || (*kmsID == "") == (*devFile == "") {
		return fmt.Errorf("input, trust-bundle and exactly one KMS or development signer are required")
	}
	file, err := os.Open(*input)
	if err != nil {
		return err
	}
	defer file.Close()
	var b identity.DirectoryBundle
	dec := json.NewDecoder(io.LimitReader(file, 1<<20))
	dec.DisallowUnknownFields()
	if dec.Decode(&b) != nil || dec.Decode(new(any)) != io.EOF {
		return fmt.Errorf("invalid bounded Directory input")
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
	signed, err := identity.SignDirectory(ctx, signer, keyID, b)
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp("", "themisy-directory-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	defer temp.Close()
	if err := json.NewEncoder(temp).Encode(signed); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	check := &identity.FileDirectory{Path: temp.Name(), Issuer: b.Issuer, TenantID: b.TenantID, MinimumRevision: b.Revision, Keys: grant.FileKeys{Path: *trust, Issuer: b.Issuer, Tenant: b.TenantID}, Ledger: &store.MemoryDirectoryLedger{}}
	if _, err := check.Read(ctx); err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(signed)
}
