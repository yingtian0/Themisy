package grant

import (
	"context"
	"themisy/pkg/protocol"
)

func SignPlanManifest(ctx context.Context, signer Signer, m protocol.PlanManifest) (protocol.PlanManifest, error) {
	payload, err := protocol.ManifestSigningMessage(m)
	if err != nil {
		return protocol.PlanManifest{}, err
	}
	m.Signature, err = signer.Sign(ctx, payload)
	return m, err
}
