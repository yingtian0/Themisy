package main

import (
	"bytes"
	"context"
	"net/http"
	"testing"
)

func TestHelpRoutingWithoutNetwork(t *testing.T) {
	transport := &rejectOfflineTransport{}
	old := http.DefaultTransport
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = old })
	for _, tc := range []struct {
		name   string
		args   []string
		output string
		stderr bool
		fail   bool
	}{
		{name: "general", args: []string{"help"}, output: cliHelp},
		{name: "offline topic", args: []string{"help", "offline-plan"}, output: offlinePlanHelp},
		{name: "global short", args: []string{"-h"}, output: cliHelp, stderr: true},
		{name: "global long", args: []string{"--help"}, output: cliHelp, stderr: true},
		{name: "offline flag", args: []string{"offline-plan", "-help"}, output: offlinePlanHelp, stderr: true},
		{name: "unknown topic", args: []string{"help", "unknown"}, fail: true},
		{name: "extra topic", args: []string{"help", "offline-plan", "extra"}, fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := run(context.Background(), tc.args, &stdout, &stderr)
			if (err != nil) != tc.fail {
				t.Fatalf("error=%v", err)
			}
			wantOut, wantErr := tc.output, ""
			if tc.stderr {
				wantOut, wantErr = "", tc.output
			}
			if stdout.String() != wantOut || stderr.String() != wantErr {
				t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
		})
	}
	if transport.calls != 0 {
		t.Fatalf("HTTP calls=%d", transport.calls)
	}
}
