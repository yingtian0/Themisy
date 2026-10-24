package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"

	"themisy/internal/grant"
	"themisy/internal/policy"
)

func runPolicyCandidate(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	usage := "Usage: themisyctl policy-candidate register|get|status -directory DIR -issuer ISSUER -key-id KEY_ID -public-key KEY [-bundle FILE | -hash sha256:...]"
	if len(args) == 0 {
		return fmt.Errorf("%s", usage)
	}
	if args[0] == "-h" || args[0] == "--help" {
		fmt.Fprintln(stderr, usage)
		fmt.Fprintln(stderr, "Local candidate storage only; does not activate or distribute Policy. Current-time verification is mandatory.")
		return nil
	}
	action := args[0]
	if action != "register" && action != "get" && action != "status" {
		return fmt.Errorf("%s", usage)
	}
	flags := flag.NewFlagSet("policy-candidate "+action, flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() {
		fmt.Fprintln(stderr, usage)
		fmt.Fprintln(stderr, "Operator-owned existing directory; no server connection, activation, distribution, approval or Write authorization. No historical clock override.")
		flags.PrintDefaults()
	}
	directory := flags.String("directory", "", "existing operator-owned local catalog directory")
	issuer := flags.String("issuer", "", "independently trusted Bundle issuer")
	keyID := flags.String("key-id", "", "independently trusted signing key ID")
	keyPath := flags.String("public-key", "", "trusted Ed25519 public key file (base64)")
	bundlePath := flags.String("bundle", "", "signed candidate Bundle JSON (register only)")
	hash := flags.String("hash", "", "exact sha256 content hash (get only)")
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 || *directory == "" || *issuer == "" || *keyID == "" || *keyPath == "" ||
		(action == "register" && (*bundlePath == "" || *hash != "")) ||
		(action == "get" && (*hash == "" || *bundlePath != "")) ||
		(action == "status" && (*hash != "" || *bundlePath != "")) {
		return fmt.Errorf("%s", usage)
	}
	key, err := grant.LoadEd25519PublicKey(*keyPath)
	if err != nil {
		return err
	}
	var bundle policy.Bundle
	if action == "register" {
		if err := readShadowJSON(*bundlePath, &bundle); err != nil {
			return err
		}
	}
	catalog, err := policy.OpenCandidateCatalog(*directory, *issuer, policy.StaticBundleKeys{*issuer + "\x00" + *keyID: key})
	if err != nil {
		return err
	}
	if action == "status" {
		inventory, err := catalog.Status(ctx)
		if err := errors.Join(err, catalog.Close()); err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(inventory)
	}
	var object *policy.Bundle
	resultHash := *hash
	if action == "register" {
		resultHash, err = catalog.Register(ctx, bundle)
	} else {
		bundle, err = catalog.Get(ctx, *hash)
		object = &bundle
	}
	if err := errors.Join(err, catalog.Close()); err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(struct {
		Mode   string         `json:"mode"`
		Hash   string         `json:"hash"`
		Bundle *policy.Bundle `json:"bundle,omitempty"`
	}{Mode: "LOCAL_CANDIDATE_ONLY", Hash: resultHash, Bundle: object})
}
