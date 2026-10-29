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

// ACP (Agent Client Protocol) is newline-delimited JSON-RPC 2.0 between a
// client and an agent server on the child's stdio. One generic client drives
// every adapter whose exact invocation selects ACP; a small reviewed profile
// says how an argv selects it and which plan environment grants approval.
// Unsupported client services are never advertised or inferred.

const (
	// acpSessionFlag selects session/load for a continuation. It is stripped
	// before exec and never reaches the native CLI.
	acpSessionFlag = "--agentctl-acp-session"
	// acpConfigFlag carries one id=value session config option that must be
	// set and read back before the prompt. It is stripped before exec.
	acpConfigFlag = "--agentctl-acp-config"
)

// acpProfile is one adapter's reviewed ACP invocation.
type acpProfile struct {
	label string
	// invocation reports whether the exact argv selects this ACP server.
	invocation func(argv []string) bool
	// approveEnv names the plan environment entry that grants allow_once.
	approveEnv string
	// settle bounds how long a successful turn keeps reading for an assistant
	// message the server sends after its end_turn response. Zero ends at once.
	settle time.Duration
	// sessionURL extracts a reviewed provider session URL from a prompt
	// result's _meta. Nil records none.
	sessionURL func(meta json.RawMessage) string
}

var acpProfiles = map[string]acpProfile{
	"devin": {label: "Devin ACP", invocation: isDevinACP, approveEnv: "AGENTCTL_DEVIN_ACP_APPROVE"},
	// The cloud relay can deliver a turn's final devin_message after the
	// end_turn response (observed on 3000.11.228), so a turn settles briefly.
	"devin-cloud": {label: "Devin cloud ACP", invocation: isDevinCloudACP, approveEnv: "AGENTCTL_DEVIN_ACP_APPROVE", settle: 20 * time.Second, sessionURL: devinCloudSessionURL},
}

// devinCloudSessionURL reads the app.devin.ai session link Devin returns in
// a cloud prompt result. Any other value is discarded.
func devinCloudSessionURL(meta json.RawMessage) string {
	var value struct {
		Session struct {
			URL string `json:"cognition.ai/url"`
		} `json:"cognition.ai/session"`
	}
	if json.Unmarshal(meta, &value) != nil {
		return ""
	}
	id, ok := strings.CutPrefix(value.Session.URL, "https://app.devin.ai/sessions/")
	if !ok || id == "" || strings.IndexFunc(id, func(r rune) bool { return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') }) >= 0 {
		return ""
	}
	return value.Session.URL
}

// acpInvocation reports the ACP profile an exact invocation selects.
func acpInvocation(adapterName string, argv []string) (acpProfile, bool) {
	profile, ok := acpProfiles[adapterName]
	if !ok || !profile.invocation(argv) {
		return acpProfile{}, false
	}
	return profile, true
}

func isDevinACP(argv []string) bool {
	return len(argv) > 1 && argv[1] == "acp" && !argvHasFlag(argv, "--cloud")
}

func isDevinCloudACP(argv []string) bool {
	return len(argv) > 1 && argv[1] == "acp" && argvHasFlag(argv, "--cloud")
}

// acpResumeArgv continues sessionID through session/load. Config flags stay
// on the base argv, so a continuation verifies the same settings again.
func acpResumeArgv(base []string, sessionID, delivery string) ([]string, error) {
	if !isDevinACP(base) && !isDevinCloudACP(base) {
		return nil, fmt.Errorf("devin print mode does not report its session id; continue requires ACP")
	}
	if delivery != PromptDeliveryStdin {
		return nil, fmt.Errorf("devin ACP requires stdin prompt delivery")
	}
	return append(withoutSessionSelection(base, acpSessionFlag), acpSessionFlag, sessionID), nil
}

// acpExecArgv removes agentctl-private ACP flags before exec.
func acpExecArgv(argv []string) []string {
	return withoutSessionSelection(argv, acpSessionFlag, acpConfigFlag)
}

type acpConfigPair struct{ id, value string }

// acpConfigPairs returns the requested config options in argv order.
func acpConfigPairs(argv []string) ([]acpConfigPair, error) {
	var pairs []acpConfigPair
	seen := map[string]bool{}
	for i := 1; i < len(argv); i++ {
		if argv[i] == "--" {
			break
		}
		raw, found := "", false
		if argv[i] == acpConfigFlag && i+1 < len(argv) {
			raw, found = argv[i+1], true
			i++
		} else if value, ok := strings.CutPrefix(argv[i], acpConfigFlag+"="); ok {
			raw, found = value, true
		}
		if !found {
			continue
		}
		id, value, ok := strings.Cut(raw, "=")
		if !ok || strings.TrimSpace(id) == "" || strings.TrimSpace(value) == "" || seen[id] {
			return nil, fmt.Errorf("%s requires one unique nonempty id=value per option", acpConfigFlag)
		}
		seen[id] = true
		pairs = append(pairs, acpConfigPair{id: id, value: value})
	}
	return pairs, nil
}

// acpSession owns a single prompt turn on the child's ACP v1 connection. All
// content is transient until the explicit result path stores it.
type acpSession struct {
	profile      acpProfile
	mu           sync.Mutex
	stdin        io.WriteCloser
	prompt       []byte
	cwd, session string
	grant        bool
	config       []acpConfigPair
	// options holds the latest currentValue/values of each config option.
	options  map[string]acpOption
	verified map[string]any
}

type acpOption struct {
	current string
	values  map[string]bool
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

// acpConfigOptions is the select-option shape of ACP session config options.
// Grouped option lists are flattened; non-select options carry no values.
type acpConfigOptions []struct {
	ID      string          `json:"id"`
	Current json.RawMessage `json:"currentValue"`
	Options []struct {
		Value   string `json:"value"`
		Options []struct {
			Value string `json:"value"`
		} `json:"options"`
	} `json:"options"`
}

func newACPSession(profile acpProfile, req LaunchRequest, argv []string) (*acpSession, error) {
	config, err := acpConfigPairs(argv)
	if err != nil {
		return nil, err
	}
	session, _ := argvFlagValue(argv, acpSessionFlag)
	cwd := req.Cwd
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	grant := false
	// Only the explicit plan's environment grants approval, never ambient env.
	for _, entry := range req.Env {
		if strings.HasPrefix(entry, profile.approveEnv+"=") {
			grant = entry == profile.approveEnv+"=1"
		}
	}
	return &acpSession{profile: profile, prompt: req.Stdin, cwd: cwd, session: session, grant: grant, config: config, options: map[string]acpOption{}, verified: map[string]any{}}, nil
}

func (c *acpSession) send(value any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = c.stdin.Write(append(data, '\n'))
	return err
}
func (c *acpSession) request(id int, method string, params any) error {
	return c.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
}

// promptUnlessCancelled sends session/prompt unless the record is already
// cancelled. Holding the transport mutex (not p.mu) across the check and the
// write orders it against cancelRecord, which sets cancelled before its
// session/cancel send queues on that mutex: either the cancel is seen here, or
// its notification follows the prompt. A blocked write never holds p.mu, so
// the cancel grace timer can still kill the server.
func (c *acpSession) promptUnlessCancelled(p *processRecord, id int, params any) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p.mu.Lock()
	cancelled := p.cancelled
	p.mu.Unlock()
	if cancelled {
		return false, nil
	}
	data, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": "session/prompt", "params": params})
	if err != nil {
		return true, err
	}
	_, err = c.stdin.Write(append(data, '\n'))
	return true, err
}
func (c *acpSession) cancel(p *processRecord) {
	p.mu.Lock()
	session := p.ref.OpaqueID
	known := p.sessionKnown
	p.mu.Unlock()
	if known {
		_ = c.send(map[string]any{"jsonrpc": "2.0", "method": "session/cancel", "params": map[string]any{"sessionId": session}})
	}
}

// absorbOptions records config option state from a response or update. It
// reports false only for a malformed list.
func (c *acpSession) absorbOptions(raw json.RawMessage) bool {
	if len(raw) == 0 || string(raw) == "null" {
		return true
	}
	var options acpConfigOptions
	if json.Unmarshal(raw, &options) != nil {
		return false
	}
	for _, option := range options {
		if option.ID == "" {
			continue
		}
		current := ""
		_ = json.Unmarshal(option.Current, &current)
		values := map[string]bool{}
		for _, value := range option.Options {
			if value.Value != "" {
				values[value.Value] = true
			}
			for _, nested := range value.Options {
				if nested.Value != "" {
					values[nested.Value] = true
				}
			}
		}
		c.options[option.ID] = acpOption{current: current, values: values}
	}
	return true
}

const (
	// acpTrailingQuiet ends a settling turn once a trailing message stops.
	acpTrailingQuiet = 2 * time.Second
	// acpDrainBound bounds waiting for the reader after the turn ends.
	acpDrainBound = 5 * time.Second
)

type acpPhase int

const (
	acpInitialize acpPhase = iota + 1
	acpSessionStart
	acpConfigure
	acpPrompt
)

func (c *acpSession) serve(p *processRecord, stdout io.Reader) {
	defer p.pipes.Done()
	// A reader goroutine feeds lines so a settling turn can stop on a timer.
	// serve waits a bounded time for it to drain stdout to EOF, which the stdin
	// close below (or the bounded kill) normally guarantees. A helper that
	// inherited stdout can hold the pipe open past the kill (Windows kills only
	// the direct process), so the wait never blocks the owner indefinitely.
	lines := make(chan []byte)
	stop := make(chan struct{})
	drained := make(chan struct{})
	readerFailed := false
	go func() {
		defer close(drained)
		defer close(lines)
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 4096), maxStructuredRecordBytes)
		for scanner.Scan() {
			line := append([]byte(nil), scanner.Bytes()...)
			select {
			case lines <- line:
			case <-stop:
				for scanner.Scan() {
				}
				return
			}
		}
		readerFailed = scanner.Err() != nil
	}()
	defer func() {
		select {
		case <-drained:
		case <-time.After(acpDrainBound):
		}
	}()
	defer close(stop)
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
	fail := func(code string, nativeCode int, details ...map[string]any) {
		p.mu.Lock()
		state := StateFailed
		if p.cancelled {
			state = StateCancelled
			code = "acp_cancelled"
			details = nil
		}
		data := map[string]any{"diagnostic_code": code, "native_code": nativeCode, "failure_structured": true}
		for _, extra := range details {
			for k, v := range extra {
				data[k] = v
			}
		}
		p.ingestObservationLocked(parsedObservation{Kind: string(state), SourceState: "acp.error", State: state, Terminal: true, Error: code, Data: data})
		p.mu.Unlock()
	}
	if len(c.prompt) == 0 {
		fail("acp_prompt_required", 0)
		return
	}
	pending := 1
	phase := acpInitialize
	if err := c.request(pending, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}, "clientInfo": map[string]any{"name": "agentctl", "version": "1"}}); err != nil {
		fail("acp_transport_write_failed", 0)
		return
	}
	next := func(method string, params any) bool {
		pending++
		if err := c.request(pending, method, params); err != nil {
			fail("acp_transport_write_failed", 0)
			return false
		}
		return true
	}
	configIndex := 0
	// configure walks the requested options in order. A current value equal to
	// the request is verified as is (a loaded session may no longer list it);
	// otherwise the request must be a listed value, which is set and read back.
	configure := func() bool {
		for configIndex < len(c.config) {
			want := c.config[configIndex]
			option, ok := c.options[want.id]
			if !ok {
				fail("acp_config_option_unavailable", 0, map[string]any{"acp_config_mismatch": map[string]any{"config_id": want.id, "requested": want.value}})
				return false
			}
			if option.current != want.value {
				if !option.values[want.value] {
					fail("acp_config_value_unavailable", 0, map[string]any{"acp_config_mismatch": map[string]any{"config_id": want.id, "requested": want.value, "current": option.current}})
					return false
				}
				phase = acpConfigure
				return next("session/set_config_option", map[string]any{"sessionId": c.session, "configId": want.id, "value": want.value})
			}
			c.verified[want.id] = want.value
			configIndex++
		}
		if len(c.verified) != 0 {
			p.ingestObservation(parsedObservation{Kind: "progress", SourceState: "acp.config", State: StateRunning, Data: map[string]any{"acp_config": cloneMap(c.verified)}})
		}
		phase = acpPrompt
		prompt := string(c.prompt)
		c.prompt = nil
		pending++
		sent, err := c.promptUnlessCancelled(p, pending, map[string]any{"sessionId": c.session, "prompt": []any{map[string]any{"type": "text", "text": prompt}}})
		if !sent {
			fail("acp_cancelled", 0)
			return false
		}
		if err != nil {
			fail("acp_transport_write_failed", 0)
			return false
		}
		return true
	}
	loading := c.session != ""
	denied := false
	var answer strings.Builder
	toolActivitySinceMessage := false
	total := 0
	// A completed turn waiting for a trailing message keeps its terminal here.
	var settled *parsedObservation
	var settleTimer <-chan time.Time
	finish := func(obs parsedObservation) {
		obs.Content = answer.String()
		p.ingestObservation(obs)
	}
	// finishSettled commits a turn that ended before its trailing message. A
	// cancellation after end_turn wins, and a turn that never produced an
	// answer fails: silence after end_turn is not a completed answer.
	finishSettled := func() {
		p.mu.Lock()
		cancelled := p.cancelled
		p.mu.Unlock()
		if cancelled {
			fail("acp_cancelled", 0)
			return
		}
		if answer.Len() == 0 {
			details := map[string]any{}
			for _, key := range []string{"session_url", "acp_config"} {
				if value, ok := settled.Data[key]; ok {
					details[key] = value
				}
			}
			fail("acp_answer_missing", 0, details)
			return
		}
		finish(*settled)
	}
	for {
		var line []byte
		select {
		case next, open := <-lines:
			if !open {
				if readerFailed {
					fail("acp_invalid_message", 0)
					return
				}
				if settled != nil {
					finishSettled()
					return
				}
				fail("acp_transport_closed_before_result", 0)
				return
			}
			line = next
		case <-settleTimer:
			finishSettled()
			return
		}
		total += len(line)
		if total > p.maxOutput && p.maxOutput > 0 {
			fail("acp_stream_limit_exceeded", 0)
			return
		}
		var m acpMessage
		if json.Unmarshal(line, &m) != nil || m.JSONRPC != "2.0" {
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
					if c.grant && !cancelled && phase == acpPrompt && c.session != "" && params.SessionID == c.session {
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
						Kind          string          `json:"sessionUpdate"`
						Content       json.RawMessage `json:"content"`
						ConfigOptions json.RawMessage `json:"configOptions"`
					} `json:"update"`
				}
				if json.Unmarshal(m.Params, &params) != nil {
					fail("acp_invalid_update", 0)
					return
				}
				// A loaded session's options may arrive only as an update.
				if params.Update.Kind == "config_option_update" && c.session != "" && params.SessionID == c.session && phase != acpPrompt {
					if !c.absorbOptions(params.Update.ConfigOptions) {
						fail("acp_invalid_config_options", 0)
						return
					}
					continue
				}
				// session/load replays history; only live prompt updates form this result.
				if phase != acpPrompt || params.SessionID != c.session {
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
					if settled != nil {
						// A trailing message may arrive in several chunks; the turn
						// ends once the stream stays quiet for a short window.
						settleTimer = time.After(acpTrailingQuiet)
						continue
					}
					p.ingestObservation(parsedObservation{Kind: "progress", SourceState: "acp.agent_message", State: StateRunning, Data: map[string]any{"phase": "assistant"}})
				case "tool_call", "tool_call_update":
					if settled != nil {
						continue
					}
					toolActivitySinceMessage = true
					p.ingestObservation(parsedObservation{Kind: "progress", SourceState: "acp.tool_call", State: StateRunning, Data: map[string]any{"phase": "tool"}})
				}
			}
			continue
		}
		if string(m.ID) != fmt.Sprint(pending) {
			fail("acp_unexpected_response", 0)
			return
		}
		if m.Error != nil {
			code := "acp_rpc_error"
			if phase == acpConfigure {
				code = "acp_config_rejected"
			}
			fail(code, m.Error.Code)
			return
		}
		switch phase {
		case acpInitialize:
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
			phase = acpSessionStart
			if !next(method, params) {
				return
			}
		case acpSessionStart:
			var result struct {
				SessionID     string          `json:"sessionId"`
				ConfigOptions json.RawMessage `json:"configOptions"`
			}
			if len(m.Result) == 0 || m.Result[0] != '{' || json.Unmarshal(m.Result, &result) != nil {
				fail("acp_invalid_session_result", 0)
				return
			}
			if !loading {
				if strings.TrimSpace(result.SessionID) == "" {
					fail("acp_session_id_missing", 0)
					return
				}
				c.session = result.SessionID
			}
			if !c.absorbOptions(result.ConfigOptions) {
				fail("acp_invalid_config_options", 0)
				return
			}
			p.ingestObservation(parsedObservation{SessionID: c.session, Kind: "progress", SourceState: "acp.session", State: StateRunning})
			if !configure() {
				return
			}
		case acpConfigure:
			var result struct {
				ConfigOptions json.RawMessage `json:"configOptions"`
			}
			want := c.config[configIndex]
			if json.Unmarshal(m.Result, &result) != nil || !c.absorbOptions(result.ConfigOptions) {
				fail("acp_invalid_config_options", 0)
				return
			}
			if c.options[want.id].current != want.value {
				fail("acp_config_unverified", 0, map[string]any{"acp_config_mismatch": map[string]any{"config_id": want.id, "requested": want.value, "current": c.options[want.id].current}})
				return
			}
			c.verified[want.id] = want.value
			configIndex++
			if !configure() {
				return
			}
		case acpPrompt:
			if settled != nil {
				fail("acp_unexpected_response", 0)
				return
			}
			var result struct {
				Stop string          `json:"stopReason"`
				Meta json.RawMessage `json:"_meta"`
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
			if len(c.verified) != 0 {
				data["acp_config"] = cloneMap(c.verified)
			}
			if c.profile.sessionURL != nil {
				if url := c.profile.sessionURL(result.Meta); url != "" {
					data["session_url"] = url
				}
			}
			obs := parsedObservation{Kind: string(state), SourceState: "acp.prompt", State: state, Terminal: true, Success: state == StateCompleted, ContentType: "text/plain", ContentSource: "assistant_terminal_result", Error: reason, Data: data}
			if state == StateCompleted && c.profile.settle > 0 {
				settled = &obs
				settleTimer = time.After(c.profile.settle)
				continue
			}
			finish(obs)
			return
		}
	}
}

// ValidateInvocationPrompt runs before native probing, journaling, or exec.
// ACP text must retain exactly the bytes admitted by the caller's digest, and
// a Devin invocation must match its adapter's placement.
func ValidateInvocationPrompt(adapterName string, argv []string, delivery string, prompt []byte) error {
	if adapterName == "hermes" {
		return validateHermesPrompt(delivery, prompt)
	}
	if err := validateDevinPlacement(adapterName, argv); err != nil {
		return err
	}
	profile, ok := acpInvocation(adapterName, argv)
	if !ok {
		return nil
	}
	if _, err := acpConfigPairs(argv); err != nil {
		return &AdapterError{Code: ErrUsage, Message: err.Error(), Details: map[string]any{"diagnostic_code": "acp_config_invalid"}}
	}
	if delivery != PromptDeliveryStdin {
		return &AdapterError{Code: ErrCapabilityUnavailable, Message: profile.label + " requires agentctl-managed stdin prompt delivery", Details: map[string]any{"diagnostic_code": "acp_prompt_delivery_required"}}
	}
	if len(prompt) == 0 {
		return &AdapterError{Code: ErrUsage, Message: profile.label + " requires a nonempty prompt", Details: map[string]any{"diagnostic_code": "acp_prompt_required"}}
	}
	if !utf8.Valid(prompt) {
		return &AdapterError{Code: ErrUsage, Message: profile.label + " prompt must be valid UTF-8", Details: map[string]any{"diagnostic_code": "acp_prompt_invalid_utf8"}}
	}
	return nil
}

// validateDevinPlacement keeps local and cloud Devin apart: the local adapter
// never runs --cloud, and devin-cloud runs only the cloud ACP relay.
func validateDevinPlacement(adapterName string, argv []string) error {
	switch adapterName {
	case "devin":
		if argvHasFlag(argv, "--cloud") {
			return &AdapterError{Code: ErrCapabilityUnavailable, Message: "the devin adapter runs local sessions only; use the devin-cloud adapter for --cloud", Details: map[string]any{"diagnostic_code": "devin_cloud_requires_cloud_adapter"}}
		}
	case "devin-cloud":
		if !isDevinCloudACP(argv) {
			return &AdapterError{Code: ErrCapabilityUnavailable, Message: "the devin-cloud adapter requires devin acp --cloud", Details: map[string]any{"diagnostic_code": "devin_cloud_requires_acp"}}
		}
	}
	return nil
}
