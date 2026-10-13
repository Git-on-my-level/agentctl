package adapter

import (
	"strings"
	"testing"
)

func TestCodexStructuredFailureKeepsReasonAndDropsAnswer(t *testing.T) {
	record := &processRecord{parser: codexParser{}, maxOutput: 1 << 20, ref: SourceRef{Kind: "codex_thread"}}
	record.ingest([]byte(`{"type":"item","item":{"type":"agent_message","text":"do-not-store-this-answer"}}`), false)
	record.ingest([]byte(`{"type":"turn.failed","thread_id":"thread-1","error":{"message":"Selected model is at capacity. Please try a different model. token=secret","codex_error_info":"server_overloaded"}}`), false)
	if record.result == nil || record.result.Success || record.result.State != StateFailed {
		t.Fatalf("result=%#v", record.result)
	}
	if record.result.Content != "" || record.result.Summary != "" || strings.Contains(record.result.Error, "secret") || strings.Contains(record.result.Error, "do-not-store") {
		t.Fatalf("failure kept answer or secret: %#v", record.result)
	}
	if record.result.Data["native_code"] != "server_overloaded" || !strings.Contains(record.result.Error, "at capacity") || !strings.Contains(record.result.Error, "[REDACTED]") {
		t.Fatalf("result=%#v", record.result)
	}
	if record.result.SessionRef.OpaqueID != "thread-1" {
		t.Fatalf("session=%#v", record.result.SessionRef)
	}
}

func TestCodexTaskCompleteFailureAndGenericCode(t *testing.T) {
	overloaded := codexParser{}.Parse([]byte(`{"type":"event_msg","payload":{"type":"task_complete","last_agent_message":"do-not-store-this-answer","error":{"message":"Selected model is at capacity. Please try a different model.","codex_error_info":"server_overloaded"}}}`), false)
	if !overloaded.Terminal || overloaded.Content != "" || overloaded.Error == "" || overloaded.Data["native_code"] != "server_overloaded" || strings.Contains(overloaded.Error, "do-not-store") {
		t.Fatalf("task_complete=%#v", overloaded)
	}
	generic := codexParser{}.Parse([]byte(`{"type":"turn.failed","error":{"message":"the turn stopped","codex_error_info":"other"}}`), false)
	if !generic.Terminal || generic.Data["native_code"] != "other" || generic.Error != "the turn stopped" || generic.Data["failure_structured"] != true {
		t.Fatalf("generic=%#v", generic)
	}
	freeText := codexParser{}.Parse([]byte(`{"type":"turn.failed","error":{"message":"boom","codex_error_info":"Not A Code"}}`), false)
	if _, ok := freeText.Data["native_code"]; ok || freeText.Error != "boom" {
		t.Fatalf("free text became a code: %#v", freeText)
	}
}
