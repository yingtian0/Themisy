package policy

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"themisy/internal/domain"
	"themisy/internal/grant"
	"themisy/pkg/protocol"
)

func TestLowerLayerCannotOverrideUpperDenyOrApproval(t *testing.T) {
	ctx := context.Background()
	base, signer, keys, now := shadowFixture(t)
	for _, kind := range []string{"upper deny", "upper approval", "embedded mandatory"} {
		t.Run(kind, func(t *testing.T) {
			b := base
			b.Version = kind
			input := Input{UserID: "user", AgentID: "agent", Environment: domain.EnvironmentStaging, Risk: "low"}
			if kind == "upper deny" {
				b.Modules = append(b.Modules, Module{Name: "environment.rego", Layer: LayerEnvironment, Source: `package themisy.authorization
import rego.v1
deny contains "ENVIRONMENT_FREEZE" if { true }`})
			}
			if kind == "upper approval" {
				input.Risk = "high"
			}
			b.Modules = append(b.Modules, Module{Name: "service.rego", Layer: LayerService, Source: `package themisy.authorization
import rego.v1
allow if { true }`})
			if kind == "embedded mandatory" {
				b.Modules = []Module{{Name: "replacement.rego", Layer: LayerPlatform, Source: `package themisy.authorization
import rego.v1
decision := {"allow":true,"reasons":[]}`}}
				input.UserID = ""
			}
			signed, err := SignBundle(ctx, signer, b)
			if err != nil {
				t.Fatal(err)
			}
			opa, err := NewVerifiedOPA(ctx, signed, signed.Issuer, keys, now)
			if err != nil {
				t.Fatal(err)
			}
			evaluation, err := opa.Evaluate(ctx, input)
			if err != nil {
				t.Fatal(err)
			}
			expected := domain.DecisionDeny
			if kind == "upper approval" {
				expected = domain.DecisionRequireApproval
			}
			if evaluation.Decision != expected {
				t.Fatalf("lower layer widened authority: %+v", evaluation)
			}
			if kind == "upper deny" && !containsString(evaluation.Reasons, "ENVIRONMENT_FREEZE") {
				t.Fatal("upper reason removed")
			}
			if kind == "upper approval" && !containsString(evaluation.RequiredRoles, "service-owner") {
				t.Fatal("upper approval role removed")
			}
		})
	}
}
func TestEnforcedBundleCannotUseNetworkBuiltin(t *testing.T) {
	b, signer, keys, now := shadowFixture(t)
	b.Modules = append(b.Modules, Module{Name: "network.rego", Layer: LayerService, Source: `package themisy.authorization
import rego.v1
deny contains "NETWORK_LOOKUP" if { http.send({"method":"GET","url":"https://example.invalid"}).status_code == 200 }`})
	signed, err := SignBundle(context.Background(), signer, b)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewVerifiedOPA(context.Background(), signed, b.Issuer, keys, now); err == nil {
		t.Fatal("network-capable enforcement module accepted")
	}
}

func TestLargeBundleUsesBoundedDomainSeparatedSigning(t *testing.T) {
	bundle, signer, keys, now := shadowFixture(t)
	bundle.Modules[0].Source += "\n# " + strings.Repeat("large reviewed policy comment ", 1000)
	bounded := policyBoundedSigner{Signer: signer}
	signed, err := SignBundle(context.Background(), bounded, bundle)
	if err != nil {
		t.Fatal(err)
	}
	if signed.SigningVersion != BundleSigningVersion {
		t.Fatal("missing signing domain")
	}
	if err := VerifyBundle(context.Background(), signed, signed.Issuer, keys, now); err != nil {
		t.Fatal(err)
	}
	signed.SigningVersion = ""
	if err := VerifyBundle(context.Background(), signed, signed.Issuer, keys, now); err == nil {
		t.Fatal("signature version stripped")
	}
}

type policyBoundedSigner struct{ grant.Signer }

func (s policyBoundedSigner) Sign(ctx context.Context, message []byte) (protocol.Signature, error) {
	if len(message) > 4096 {
		return protocol.Signature{}, fmt.Errorf("KMS raw-message bound exceeded")
	}
	return s.Signer.Sign(ctx, message)
}

func TestLegacyBundleSignatureStillVerifies(t *testing.T) {
	bundle, signer, keys, now := shadowFixture(t)
	bundle.SigningVersion = ""
	var err error
	bundle.Hash, err = CalculateBundleHash(bundle)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := CanonicalBundlePayload(bundle)
	if err != nil {
		t.Fatal(err)
	}
	bundle.Signature, err = signer.Sign(context.Background(), payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyBundle(context.Background(), bundle, bundle.Issuer, keys, now); err != nil {
		t.Fatal(err)
	}
}
