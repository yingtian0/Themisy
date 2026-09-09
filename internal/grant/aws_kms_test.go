package grant

import (
	"bytes"
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/kms/types"
)

func TestAWSKMSSignerUsesRawEd25519AndDoesNotExposeKeyMaterial(t *testing.T) {
	client := &fakeAWSKMS{signature: bytes.Repeat([]byte{1}, 64)}
	signer, err := NewAWSKMSSigner(client, "arn:aws:kms:ap-northeast-1:111122223333:key/test")
	if err != nil {
		t.Fatal(err)
	}
	signature, err := signer.Sign(context.Background(), []byte("canonical-grant"))
	if err != nil {
		t.Fatal(err)
	}
	if signature.Algorithm != AlgorithmEd25519 || signature.KeyID != signer.KeyID || len(signature.Value) == 0 {
		t.Fatalf("signature=%#v", signature)
	}
	if client.input == nil || client.input.MessageType != types.MessageTypeRaw || client.input.SigningAlgorithm != types.SigningAlgorithmSpecEd25519Sha512 || aws.ToString(client.input.KeyId) != signer.KeyID {
		t.Fatalf("input=%#v", client.input)
	}
}

type fakeAWSKMS struct {
	input     *kms.SignInput
	signature []byte
}

func (f *fakeAWSKMS) Sign(_ context.Context, input *kms.SignInput, _ ...func(*kms.Options)) (*kms.SignOutput, error) {
	f.input = input
	return &kms.SignOutput{Signature: append([]byte(nil), f.signature...), SigningAlgorithm: types.SigningAlgorithmSpecEd25519Sha512}, nil
}
