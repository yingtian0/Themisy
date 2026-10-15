package credentials

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestCredentialValuesAreRedactedFromFormattingAndLogs(t *testing.T) {
	secrets := []string{"AKIA-super-secret", "secret-access-key-value", "session-token-value"}
	credential := New(map[string][]byte{
		AWSAccessKeyID:     []byte(secrets[0]),
		AWSSecretAccessKey: []byte(secrets[1]),
		AWSSessionToken:    []byte(secrets[2]),
	}, time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC))

	formatted := fmt.Sprintf("%v|%+v|%#v|%x|%q", credential, credential, credential, credential, credential)
	assertNoSecrets(t, formatted, secrets)

	var logs bytes.Buffer
	slog.New(slog.NewJSONHandler(&logs, nil)).Info("credential acquisition", slog.Any("credential", credential))
	assertNoSecrets(t, logs.String(), secrets)
	if !strings.Contains(logs.String(), "[REDACTED]") {
		t.Fatalf("structured log did not use the redaction marker: %s", logs.String())
	}

	encoded, err := json.Marshal(credential)
	if err != nil {
		t.Fatal(err)
	}
	assertNoSecrets(t, string(encoded), secrets)
}

func assertNoSecrets(t *testing.T, output string, secrets []string) {
	t.Helper()
	for _, secret := range secrets {
		if strings.Contains(output, secret) {
			t.Fatalf("credential value leaked in %q", output)
		}
	}
}
