package adapter

import (
	"strings"
	"testing"
)

const zcodeQuotaStderrFixture = `ProviderBusinessError: [1308][Usage limit reached for 5 hour. Your limit will reset at 2026-10-15 19:38:47]
  code: 'PROVIDER_BUSINESS_ERROR', providerCode: '1308', responseStatus: 429, 'retry-after': '1266'`

const zcodeUnknownProviderStderrFixture = `ProviderBusinessError: provider request failed
  code: 'PROVIDER_BUSINESS_ERROR', providerCode: '7314', responseStatus: 500`

func TestZCodeStderrQuotaFailureIsBoundedAndRetryableCandidate(t *testing.T) {
	record := &processRecord{parser: zcodeParser{}, maxOutput: 1 << 20, done: make(chan struct{}), ref: SourceRef{Kind: "zcode_session"}}
	for _, line := range strings.Split(zcodeQuotaStderrFixture, "\n") {
		record.ingest([]byte(line), true)
	}
	record.ingest([]byte("Error: Turn execution failed"), true)
	exitCode := 1
	record.exitCode = &exitCode
	record.finish(nil)

	if record.result == nil || record.result.Success || record.result.State != StateFailed {
		t.Fatalf("result=%#v", record.result)
	}
	if record.result.Error != "ZCode provider usage limit reached; resets 2026-10-15 19:38:47" || record.result.Data["native_code"] != "1308" || record.result.Data["failure_structured"] != true {
		t.Fatalf("result=%#v", record.result)
	}
	if len(record.result.Error) > 200 {
		t.Fatalf("failure message exceeds 200 bytes: %d", len(record.result.Error))
	}
	for _, leaked := range []string{"ProviderBusinessError", "providerCode", "responseStatus", "retry-after", "request id", "requestId", "trace id", "traceId"} {
		if strings.Contains(record.result.Error, leaked) {
			t.Fatalf("failure message leaked %q: %s", leaked, record.result.Error)
		}
	}
}

func TestZCodeUnknownProviderFailureGetsGenericBoundedCandidate(t *testing.T) {
	record := &processRecord{parser: zcodeParser{}, maxOutput: 1 << 20, done: make(chan struct{}), ref: SourceRef{Kind: "zcode_session"}}
	for _, line := range strings.Split(zcodeUnknownProviderStderrFixture, "\n") {
		record.ingest([]byte(line), true)
	}
	exitCode := 1
	record.exitCode = &exitCode
	record.finish(nil)

	if record.result == nil || record.result.State != StateFailed || !strings.Contains(record.result.Error, "provider request failed") || record.result.Data["native_code"] != "7314" {
		t.Fatalf("result=%#v", record.result)
	}
	if len(record.result.Error) > 200 {
		t.Fatalf("failure message exceeds 200 bytes: %d", len(record.result.Error))
	}
	for _, leaked := range []string{"providerCode", "responseStatus", "request id", "requestId", "trace id", "traceId"} {
		if strings.Contains(record.result.Error, leaked) {
			t.Fatalf("failure message leaked %q: %s", leaked, record.result.Error)
		}
	}
}

func TestZCodeJSONFailureRetainsBoundedClassificationCause(t *testing.T) {
	obs := (zcodeParser{}).Parse([]byte(`{"error":{"message":"Authentication required; please login to continue (requestId: fixture-only traceId: fixture-only)"}}`), false)
	if !obs.Terminal || obs.State != StateFailed || obs.Error != "authentication required" {
		t.Fatalf("observation=%#v", obs)
	}
	if len(obs.Error) > 200 {
		t.Fatalf("failure message exceeds 200 bytes: %d", len(obs.Error))
	}
}

func TestZCodeGenericRateLimitRemainsForSharedClassification(t *testing.T) {
	diagnostic := parseZCodeFailure("provider returned a rate limit response")
	if diagnostic.Category != "" || !strings.Contains(diagnostic.Message, "rate limit") {
		t.Fatalf("diagnostic=%#v", diagnostic)
	}
}

func TestZCodeFailureClassifierRecognizesHTTP429(t *testing.T) {
	diagnostic := zcodeStderrFailure("provider returned responseStatus: 429")
	if diagnostic == nil || diagnostic.Category != "capacity" || diagnostic.Message != "ZCode provider usage limit reached" {
		t.Fatalf("diagnostic=%#v", diagnostic)
	}
}
