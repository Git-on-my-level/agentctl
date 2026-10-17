package adapter

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// devinACP owns a single prompt turn on the child's newline-delimited ACP v1
// connection. All content is transient until the explicit result path stores it.
// Unsupported client services are never advertised or inferred.
type devinACP struct {
	mu           sync.Mutex
	stdin        io.WriteCloser
	prompt       []byte
	cwd, session string
	grant        bool
}

type acpMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *struct {
		Code int `json:"code"`
	} `json:"error,omitempty"`
}

func newDevinACP(req LaunchRequest, argv []string) *devinACP {
	session, _ := argvFlagValue(argv, devinACPSessionFlag)
	cwd := req.Cwd
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	grant := false
	// Only the explicit plan's environment grants approval, never ambient env.
	for _, entry := range req.Env {
		if strings.HasPrefix(entry, "AGENTCTL_DEVIN_ACP_APPROVE=") {
			grant = entry == "AGENTCTL_DEVIN_ACP_APPROVE=1"
		}
	}
	return &devinACP{prompt: req.Stdin, cwd: cwd, session: session, grant: grant}
}

func (c *devinACP) send(value any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = c.stdin.Write(append(data, '\n'))
	return err
}
func (c *devinACP) request(id int, method string, params any) error {
	return c.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
}
func (c *devinACP) cancel(p *processRecord) {
	p.mu.Lock()
	session := p.ref.OpaqueID
	known := p.sessionKnown
	p.mu.Unlock()
	if known {
		_ = c.send(map[string]any{"jsonrpc": "2.0", "method": "session/cancel", "params": map[string]any{"sessionId": session}})
	}
}

func (c *devinACP) serve(p *processRecord, stdout io.Reader) {
	defer p.pipes.Done()
	defer func() {
		c.prompt = nil
		_ = c.stdin.Close()
		// ACP has no shutdown RPC. EOF is the graceful shutdown; bound a server
		// that keeps stderr/stdout open after EOF so the owner always reaps it.
		go func() {
			select {
			case <-p.done:
			case <-time.After(2 * time.Second):
				_ = killProcess(p.cmd)
			}
		}()
	}()
	fail := func(code string, nativeCode int) {
		p.mu.Lock()
		state := StateFailed
		if p.cancelled {
			state = StateCancelled
			code = "acp_cancelled"
		}
		p.ingestObservationLocked(parsedObservation{Kind: string(state), SourceState: "acp.error", State: state, Terminal: true, Error: code, Data: map[string]any{"diagnostic_code": code, "native_code": nativeCode, "failure_structured": true}})
		p.mu.Unlock()
	}
	if len(c.prompt) == 0 {
		fail("acp_prompt_required", 0)
		return
	}
	if err := c.request(1, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}, "clientInfo": map[string]any{"name": "agentctl", "version": "1"}}); err != nil {
		fail("acp_transport_write_failed", 0)
		return
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 4096), maxStructuredRecordBytes)
	stage := 1
	loading := c.session != ""
	denied := false
	var answer strings.Builder
	toolActivitySinceMessage := false
	total := 0
	for scanner.Scan() {
		total += len(scanner.Bytes())
		if total > p.maxOutput && p.maxOutput > 0 {
			fail("acp_stream_limit_exceeded", 0)
			return
		}
		var m acpMessage
		if json.Unmarshal(scanner.Bytes(), &m) != nil || m.JSONRPC != "2.0" {
			fail("acp_invalid_message", 0)
			return
		}
		if m.Method != "" {
			if len(m.ID) > 0 && string(m.ID) != "null" {
				if m.Method == "session/request_permission" {
					var params struct {
						SessionID string `json:"sessionId"`
						Options   []struct {
							ID   string `json:"optionId"`
							Kind string `json:"kind"`
						} `json:"options"`
					}
					if json.Unmarshal(m.Params, &params) != nil {
						fail("acp_invalid_permission_request", 0)
						return
					}
					p.mu.Lock()
					cancelled := p.cancelled
					p.mu.Unlock()
					option := ""
					if c.grant && !cancelled && stage == 3 && c.session != "" && params.SessionID == c.session {
						for _, o := range params.Options {
							if o.Kind == "allow_once" && o.ID != "" {
								option = o.ID
								break
							}
						}
					}
					outcome := map[string]any{"outcome": "cancelled"}
					if option != "" {
						outcome = map[string]any{"outcome": "selected", "optionId": option}
					} else {
						denied = true
						// An ordinary denial selects reject_once. Only actual
						// cancellation or an absent reject option uses cancelled.
						if !cancelled {
							for _, o := range params.Options {
								if o.Kind == "reject_once" && o.ID != "" {
									outcome = map[string]any{"outcome": "selected", "optionId": o.ID}
									break
								}
							}
						}
						p.ingestObservation(parsedObservation{Kind: "attention", SourceState: "acp.permission_denied", State: StateAttention, Data: map[string]any{"attention_kind": "permission", "diagnostic_code": "acp_permission_denied"}})
					}
					if err := c.send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{"outcome": outcome}}); err != nil {
						fail("acp_transport_write_failed", 0)
						return
					}
				} else {
					// Includes fs/* and terminal/*: not advertised, never touch local files.
					if err := c.send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": -32601, "message": "Client method not supported"}}); err != nil {
						fail("acp_transport_write_failed", 0)
						return
					}
				}
				continue
			}
			if m.Method == "session/update" {
				var params struct {
					SessionID string `json:"sessionId"`
					Update    struct {
						Kind    string          `json:"sessionUpdate"`
						Content json.RawMessage `json:"content"`
					} `json:"update"`
				}
				if json.Unmarshal(m.Params, &params) != nil {
					fail("acp_invalid_update", 0)
					return
				}
				// session/load replays history; only live prompt updates form this result.
				if stage != 3 || params.SessionID != c.session {
					continue
				}
				switch params.Update.Kind {
				case "agent_message_chunk":
					var content struct {
						Type string `json:"type"`
						Text string `json:"text"`
					}
					if json.Unmarshal(params.Update.Content, &content) != nil {
						fail("acp_invalid_message_chunk", 0)
						return
					}
					// Tool activity separates interim narration from the next
					// assistant message. Keep split chunks of that message,
					// resetting only when a later message actually starts.
					if toolActivitySinceMessage {
						answer.Reset()
						toolActivitySinceMessage = false
					}
					if content.Type == "text" {
						answer.WriteString(content.Text)
					}
					p.ingestObservation(parsedObservation{Kind: "progress", SourceState: "acp.agent_message", State: StateRunning, Data: map[string]any{"phase": "assistant"}})
				case "tool_call", "tool_call_update":
					toolActivitySinceMessage = true
					p.ingestObservation(parsedObservation{Kind: "progress", SourceState: "acp.tool_call", State: StateRunning, Data: map[string]any{"phase": "tool"}})
				}
			}
			continue
		}
		if string(m.ID) != fmt.Sprint(stage) {
			fail("acp_unexpected_response", 0)
			return
		}
		if m.Error != nil {
			fail("acp_rpc_error", m.Error.Code)
			return
		}
		switch stage {
		case 1:
			var init struct {
				Version      int `json:"protocolVersion"`
				Capabilities struct {
					Load bool `json:"loadSession"`
				} `json:"agentCapabilities"`
			}
			if json.Unmarshal(m.Result, &init) != nil || init.Version != 1 {
				fail("acp_protocol_version_unsupported", 0)
				return
			}
			params := map[string]any{"cwd": c.cwd, "mcpServers": []any{}}
			method := "session/new"
			if loading {
				if !init.Capabilities.Load {
					fail("acp_load_session_not_advertised", 0)
					return
				}
				method = "session/load"
				params["sessionId"] = c.session
			}
			stage = 2
			if err := c.request(stage, method, params); err != nil {
				fail("acp_transport_write_failed", 0)
				return
			}
		case 2:
			if len(m.Result) == 0 || m.Result[0] != '{' {
				fail("acp_invalid_session_result", 0)
				return
			}
			if !loading {
				var result struct {
					SessionID string `json:"sessionId"`
				}
				if json.Unmarshal(m.Result, &result) != nil || strings.TrimSpace(result.SessionID) == "" {
					fail("acp_session_id_missing", 0)
					return
				}
				c.session = result.SessionID
			}
			p.ingestObservation(parsedObservation{SessionID: c.session, Kind: "progress", SourceState: "acp.session", State: StateRunning})
			stage = 3
			prompt := string(c.prompt)
			c.prompt = nil
			if err := c.request(stage, "session/prompt", map[string]any{"sessionId": c.session, "prompt": []any{map[string]any{"type": "text", "text": prompt}}}); err != nil {
				fail("acp_transport_write_failed", 0)
				return
			}
		case 3:
			var result struct {
				Stop string `json:"stopReason"`
			}
			if json.Unmarshal(m.Result, &result) != nil {
				fail("acp_invalid_prompt_result", 0)
				return
			}
			state := StateFailed
			reason := "acp_unknown_stop_reason"
			switch result.Stop {
			case "end_turn":
				state = StateCompleted
				reason = ""
			case "cancelled":
				state = StateCancelled
				reason = "acp_cancelled"
			case "refusal":
				reason = "acp_refusal"
			case "max_tokens":
				reason = "acp_max_tokens"
			case "max_turn_requests":
				reason = "acp_max_turn_requests"
			}
			p.mu.Lock()
			cancelled := p.cancelled
			p.mu.Unlock()
			if cancelled {
				state = StateCancelled
				reason = "acp_cancelled"
			}
			if denied && !cancelled {
				state = StateFailed
				reason = "acp_permission_denied"
			}
			data := map[string]any{}
			if reason != "" {
				data["diagnostic_code"] = reason
				data["failure_structured"] = true
			}
			p.ingestObservation(parsedObservation{Kind: string(state), SourceState: "acp.prompt", State: state, Terminal: true, Success: state == StateCompleted, Content: answer.String(), ContentType: "text/plain", ContentSource: "assistant_terminal_result", Error: reason, Data: data})
			return
		}
	}
	fail("acp_transport_closed_before_result", 0)
}

// ValidateInvocationPrompt runs before native probing, journaling, or exec.
// ACP text must retain exactly the bytes admitted by the caller's digest.
func ValidateInvocationPrompt(adapterName string, argv []string, delivery string, prompt []byte) error {
	if adapterName != "devin" || !isDevinACP(argv) {
		return nil
	}
	if delivery != PromptDeliveryStdin {
		return &AdapterError{Code: ErrCapabilityUnavailable, Message: "Devin ACP requires agentctl-managed stdin prompt delivery", Details: map[string]any{"diagnostic_code": "acp_prompt_delivery_required"}}
	}
	if len(prompt) == 0 {
		return &AdapterError{Code: ErrUsage, Message: "Devin ACP requires a nonempty prompt", Details: map[string]any{"diagnostic_code": "acp_prompt_required"}}
	}
	if !utf8.Valid(prompt) {
		return &AdapterError{Code: ErrUsage, Message: "Devin ACP prompt must be valid UTF-8", Details: map[string]any{"diagnostic_code": "acp_prompt_invalid_utf8"}}
	}
	return nil
}
