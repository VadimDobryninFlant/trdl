package pgp

import (
	"context"

	"github.com/hashicorp/vault/sdk/logical"
)

const (
	storageKeyPrefixTrustedPGPPublicKey = "trusted_pgp_public_key/"
)

func GetTrustedPGPPublicKeys(ctx context.Context, storage logical.Storage) ([]string, error) {
	list, err := storage.List(ctx, storageKeyPrefixTrustedPGPPublicKey)
	if err != nil {
		return nil, err
	}

	var trustedPGPPublicKeys []string
	for _, postfix := range list {
		storageEntryKey := storageKeyPrefixTrustedPGPPublicKey + postfix
		e, err := storage.Get(ctx, storageEntryKey)
		if err != nil {
			return nil, err
		}

		trustedPGPPublicKeys = append(trustedPGPPublicKeys, string(e.Value))
	}

	return trustedPGPPublicKeys, nil
}
