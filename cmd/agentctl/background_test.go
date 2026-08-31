package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/output"
)

func TestBackgroundWorkerExitWaitsForStructuredOutput(t *testing.T) {
	executionID, err := ids.NewExecutionID(ids.CryptoGenerator{})
	if err != nil {
		t.Fatal(err)
	}
	want := output.NewError(output.CodeCapabilityUnavailable, "fixture rejection", false).WithDetail("diagnostic_code", "fixture_rejected")
	raw, err := json.Marshal(output.ErrorDocument{OK: false, SchemaVersion: 1, Error: want, Warnings: []output.Warning{}})
	if err != nil {
		t.Fatal(err)
	}
	line := make(chan []byte)
	release := make(chan struct{})
	go func() {
		<-release
		line <- raw
	}()
	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	problem := backgroundWorkerExited(ctx, time.After(time.Second), line, executionID, errors.New("exit status 5"))
	if problem.Code != output.CodeCapabilityUnavailable || problem.Details["diagnostic_code"] != "fixture_rejected" || problem.Details["execution_id"] != executionID.String() {
		t.Fatalf("structured worker error was lost: %#v", problem)
	}
}
