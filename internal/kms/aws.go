package kms

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/kms/types"
)

type AWSKMS struct{ client *kms.Client }

func NewAWS(ctx context.Context, region string) (*AWSKMS, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("kms: load AWS config: %w", err)
	}
	return &AWSKMS{client: kms.NewFromConfig(cfg)}, nil
}

func (k *AWSKMS) GenerateDataKey(ctx context.Context, keyID string) ([]byte, []byte, error) {
	out, err := k.client.GenerateDataKey(ctx, &kms.GenerateDataKeyInput{KeyId: aws.String(keyID), KeySpec: types.DataKeySpecAes256})
	if err != nil {
		return nil, nil, fmt.Errorf("kms: generate data key: %w", err)
	}
	return out.Plaintext, out.CiphertextBlob, nil
}

func (k *AWSKMS) Decrypt(ctx context.Context, ciphertext []byte) ([]byte, error) {
	out, err := k.client.Decrypt(ctx, &kms.DecryptInput{CiphertextBlob: ciphertext})
	if err != nil {
		return nil, fmt.Errorf("kms: decrypt data key: %w", err)
	}
	return out.Plaintext, nil
}
