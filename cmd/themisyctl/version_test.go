package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"testing"
)

func TestVersionReportsEmbeddedSourceWithoutServer(t *testing.T) {
	var out bytes.Buffer
	if err := run(context.Background(), []string{"-server", "invalid", "version"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	var result map[string]string
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result["version"] != version || result["source_commit"] != sourceCommit {
		t.Fatalf("version=%v", result)
	}
	if err := run(context.Background(), []string{"version", "extra"}, io.Discard, io.Discard); err == nil {
		t.Fatal("unexpected version argument accepted")
	}
}
