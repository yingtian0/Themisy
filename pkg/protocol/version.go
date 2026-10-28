package protocol

import "fmt"

const VersionV1Alpha1 = "themisy.protocol/v1alpha1"

const VersionV1Alpha2 = "themisy.protocol/v1alpha2"

func ValidateVersion(version string) error {
	if version != VersionV1Alpha1 && version != VersionV1Alpha2 {
		return fmt.Errorf("UNSUPPORTED_PROTOCOL_VERSION: %q", version)
	}
	return nil
}
