package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"themisy/internal/grant"
	"themisy/internal/policy"
)

const maximumShadowFileBytes = 2 << 20

func runPolicyShadow(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("policy-shadow", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() {
		fmt.Fprintln(stderr, "Usage: themisyctl policy-shadow -current current.json -candidate candidate.json -cases cases.json -issuer ISSUER -key-id KEY_ID -public-key public.key [-at RFC3339]")
		fmt.Fprintln(stderr, "Offline analysis only. Verifies both signed Bundles; does not apply policies, contact the server, issue Grants or authorize Writes. Changed cases still exit 0; analysis errors exit 1 with no report.")
		flags.PrintDefaults()
	}
	currentPath := flags.String("current", "", "current signed Bundle JSON")
	candidatePath := flags.String("candidate", "", "candidate signed Bundle JSON")
	casesPath := flags.String("cases", "", "JSON array of named canonical policy input cases")
	issuer := flags.String("issuer", "", "independently trusted Bundle issuer")
	keyID := flags.String("key-id", "", "independently trusted signing key ID")
	publicPath := flags.String("public-key", "", "trusted Ed25519 public key file (base64)")
	at := flags.String("at", "", "analysis observation time; default is current UTC time")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 || *currentPath == "" || *candidatePath == "" || *casesPath == "" || *issuer == "" || *keyID == "" || *publicPath == "" {
		return fmt.Errorf("policy-shadow requires -current, -candidate, -cases, -issuer, -key-id and -public-key, with no positional arguments")
	}
	now := time.Now().UTC()
	if *at != "" {
		parsed, err := time.Parse(time.RFC3339Nano, *at)
		if err != nil {
			return fmt.Errorf("invalid -at: %w", err)
		}
		now = parsed.UTC()
	}
	var current, candidate policy.Bundle
	var cases []policy.ShadowCase
	for _, item := range []struct {
		path  string
		value any
	}{{*currentPath, &current}, {*candidatePath, &candidate}, {*casesPath, &cases}} {
		if err := readShadowJSON(item.path, item.value); err != nil {
			return err
		}
	}
	key, err := grant.LoadEd25519PublicKey(*publicPath)
	if err != nil {
		return err
	}
	report, err := policy.CompareSignedBundles(ctx, current, candidate, *issuer, policy.StaticBundleKeys{*issuer + "\x00" + *keyID: key}, cases, now)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(report)
}

func readShadowJSON(path string, value any) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("read shadow input: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > maximumShadowFileBytes {
		return fmt.Errorf("shadow input must be a regular JSON file no larger than %d bytes", maximumShadowFileBytes)
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maximumShadowFileBytes+1))
	if err != nil {
		return err
	}
	if len(data) > maximumShadowFileBytes {
		return fmt.Errorf("shadow input exceeds size limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return fmt.Errorf("decode shadow input: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("shadow input must contain exactly one JSON value")
	}
	return nil
}
