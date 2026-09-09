package grant

import (
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/kms/types"
)

// AWSKMSAPI is the narrow AWS SDK v2 boundary used by the Control Plane.
type AWSKMSAPI interface {
	Sign(context.Context, *kms.SignInput, ...func(*kms.Options)) (*kms.SignOutput, error)
}

type AWSKMSClient struct{ Client AWSKMSAPI }

func (c AWSKMSClient) Sign(ctx context.Context, keyID string, payload []byte) ([]byte, error) {
	if c.Client == nil || keyID == "" || len(payload) == 0 || len(payload) > 4096 {
		return nil, errors.New("AWS KMS Ed25519 signer is not configured")
	}
	output, err := c.Client.Sign(ctx, &kms.SignInput{
		KeyId:            aws.String(keyID),
		Message:          append([]byte(nil), payload...),
		MessageType:      types.MessageTypeRaw,
		SigningAlgorithm: types.SigningAlgorithmSpecEd25519Sha512,
	})
	if err != nil {
		return nil, err
	}
	if output == nil || len(output.Signature) != 64 || output.SigningAlgorithm != types.SigningAlgorithmSpecEd25519Sha512 {
		return nil, fmt.Errorf("AWS KMS returned an invalid Ed25519 signature")
	}
	return append([]byte(nil), output.Signature...), nil
}

func NewAWSKMSSigner(client AWSKMSAPI, keyID string) (*KMSSigner, error) {
	if client == nil || keyID == "" {
		return nil, errors.New("AWS KMS client and key ID are required")
	}
	return &KMSSigner{Client: AWSKMSClient{Client: client}, KeyID: keyID, Algorithm: AlgorithmEd25519}, nil
}
