package adapter

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	codexRolloutMaxBytes   = 8 << 20
	codexRolloutMaxLine    = 1 << 20
	codexRolloutMaxEntries = 50000
	codexNeverPolicyError  = "approval policy is Never; reject command — you cannot ask for escalated permissions if the approval policy is Never"
	codexRolloutFailure    = "codex_rollout_evidence_unavailable"
)

type codexRolloutCheckpoint struct {
	ThreadID    string
	Path        string
	Size        int64
	Captured    bool
	Unavailable bool
}

type codexRolloutFile struct {
	Path    string
	Size    int64
	ModTime int64
}

type codexRolloutCall struct {
	Name      string
	Escalated bool
}

// effectiveCodexHome resolves only the home selected for this child. In
// particular, rollout lookup never falls back to another user's or session's
// Codex directory.
func effectiveCodexHome(env []string, childDir string) (string, error) {
	home := envValue(env, "CODEX_HOME")
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		home = filepath.Join(userHome, ".codex")
	}
	if filepath.IsAbs(home) {
		return filepath.Clean(home), nil
	}
	if childDir == "" {
		var err error
		childDir, err = os.Getwd()
		if err != nil {
			return "", err
		}
	} else if !filepath.IsAbs(childDir) {
		var err error
		childDir, err = filepath.Abs(childDir)
		if err != nil {
			return "", err
		}
	}
	return filepath.Clean(filepath.Join(childDir, home)), nil
}

// codexRolloutCheckpointForLaunch is called before the child starts. A fresh
// invocation has no known thread ID yet, so its first matching rollout is
// read from byte zero. A resume invocation names its thread in argv; capture
// that exact file's size before any current-turn records can be appended.
func codexRolloutCheckpointForLaunch(codexHome, threadID string) codexRolloutCheckpoint {
	checkpoint := codexRolloutCheckpoint{ThreadID: threadID, Captured: true}
	if threadID == "" {
		return checkpoint
	}
	if !validCodexThreadID(threadID) || codexHome == "" {
		checkpoint.Unavailable = true
	} else if file, found, err := latestCodexRollout(codexHome, threadID); err != nil {
		checkpoint.Unavailable = true
	} else if found {
		checkpoint.Path = file.Path
		checkpoint.Size = file.Size
	}
	return checkpoint
}

// observeCodexThreadID fills only the identity left unknown by a fresh launch.
// The evidence boundary itself is always captured before the child starts;
// stdout timing can reveal the rollout identity but never moves that boundary.
func (p *processRecord) observeCodexThreadID(threadID string) {
	if threadID == "" {
		return
	}
	p.mu.Lock()
	if p.codexRolloutCheckpoint.Captured && p.codexRolloutCheckpoint.ThreadID == "" {
		p.codexRolloutCheckpoint.ThreadID = threadID
	}
	p.mu.Unlock()
}

// verifyCodexRollout reads only the current thread's latest rollout and only
// the bytes appended after launch when that resumed thread already had a file.
// An unreadable or incomplete native record fails closed before turn.completed
// can be accepted as success.
func (p *processRecord) verifyCodexRollout(obs parsedObservation) parsedObservation {
	p.mu.Lock()
	checkpoint := p.codexRolloutCheckpoint
	p.mu.Unlock()
	threadID := firstNonEmpty(obs.SessionID, checkpoint.ThreadID)
	if threadID == "" || !validCodexThreadID(threadID) || p.codexHome == "" || checkpoint.Unavailable ||
		(checkpoint.Captured && checkpoint.ThreadID != "" && checkpoint.ThreadID != threadID) {
		return codexRolloutUnavailableObservation(threadID)
	}
	file, found, err := latestCodexRollout(p.codexHome, threadID)
	if err != nil || !found {
		return codexRolloutUnavailableObservation(threadID)
	}
	offset := int64(0)
	needSessionMeta := true
	if checkpoint.Captured && checkpoint.Path != "" && checkpoint.Path == file.Path {
		offset = checkpoint.Size
		needSessionMeta = false
	}
	if file.Size < offset || file.Size-offset > codexRolloutMaxBytes {
		return codexRolloutUnavailableObservation(threadID)
	}
	items, denied, err := readCodexRolloutDelta(file, offset, threadID, needSessionMeta)
	if err != nil || items == 0 {
		return codexRolloutUnavailableObservation(threadID)
	}
	if denied {
		return codexCommandDeclinedObservation(threadID)
	}
	return obs
}

// codexResumeThreadID extracts only a canonical UUID positional following the
// verified `exec resume` command. Fresh launches deliberately return empty so
// their newly-created rollout is consumed from its beginning.
func codexResumeThreadID(argv []string) string {
	for i, arg := range argv {
		if arg != "resume" || i == 0 || argv[i-1] != "exec" {
			continue
		}
		for j := i + 1; j < len(argv); j++ {
			arg := argv[j]
			if arg == "--" {
				return ""
			}
			name, _, inline := strings.Cut(arg, "=")
			if strings.HasPrefix(arg, "-") {
				if takesValue, known := codexResumeFlags[name]; known && takesValue && !inline {
					j++
				}
				continue
			}
			if validCodexThreadID(arg) {
				return arg
			}
		}
	}
	return ""
}

func codexEphemeralDelegationError() error {
	return &AdapterError{
		Code:    ErrCapabilityUnavailable,
		Message: "Codex --ephemeral cannot be delegated because pre-execution policy denials require a persistent rollout",
		Details: map[string]any{"diagnostic_code": "codex_ephemeral_delegation_unsupported"},
	}
}

func codexRolloutUnavailableObservation(threadID string) parsedObservation {
	return parsedObservation{
		SessionID:   threadID,
		Kind:        "terminal",
		SourceState: codexRolloutFailure,
		State:       StateFailed,
		Liveness:    LivenessExited,
		Terminal:    true,
		Error:       "Codex completed a turn, but its current rollout could not be verified.",
		Data: map[string]any{
			"family":             "codex",
			"diagnostic_code":    codexRolloutFailure,
			"failure_category":   "native",
			"failure_structured": true,
		},
	}
}

func validCodexThreadID(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	for i, r := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			continue
		}
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

func latestCodexRollout(codexHome, threadID string) (codexRolloutFile, bool, error) {
	sessions := filepath.Join(codexHome, "sessions")
	resolved, err := filepath.EvalSymlinks(sessions)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return codexRolloutFile{}, false, nil
		}
		return codexRolloutFile{}, false, err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return codexRolloutFile{}, false, nil
		}
		return codexRolloutFile{}, false, err
	}
	if !info.IsDir() {
		return codexRolloutFile{}, false, errors.New("sessions is not a directory")
	}
	var candidates []codexRolloutFile
	entries := 0
	err = filepath.WalkDir(resolved, func(path string, entry fs.DirEntry, walkErr error) error {
		entries++
		if entries > codexRolloutMaxEntries {
			return errors.New("session entry bound exceeded")
		}
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !isCodexRolloutFilename(entry.Name(), threadID) {
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		fileInfo, err := entry.Info()
		if err != nil {
			return err
		}
		if !fileInfo.Mode().IsRegular() || fileInfo.Size() < 0 {
			return nil
		}
		candidates = append(candidates, codexRolloutFile{Path: path, Size: fileInfo.Size(), ModTime: fileInfo.ModTime().UnixNano()})
		return nil
	})
	if err != nil {
		return codexRolloutFile{}, false, err
	}
	if len(candidates) == 0 {
		return codexRolloutFile{}, false, nil
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].ModTime != candidates[j].ModTime {
			return candidates[i].ModTime > candidates[j].ModTime
		}
		return candidates[i].Path > candidates[j].Path
	})
	return candidates[0], true, nil
}

func isCodexRolloutFilename(name, threadID string) bool {
	if !strings.HasPrefix(name, "rollout-") || !strings.HasSuffix(name, ".jsonl") {
		return false
	}
	index := strings.Index(name, threadID)
	if index < len("rollout-") || index == 0 || name[index-1] != '-' {
		return false
	}
	end := index + len(threadID)
	return end < len(name) && (name[end] == '.' || name[end] == '_' || name[end] == '-')
}

func readCodexRolloutDelta(file codexRolloutFile, offset int64, threadID string, needSessionMeta bool) (int, bool, error) {
	opened, err := os.Open(file.Path)
	if err != nil {
		return 0, false, err
	}
	defer opened.Close()
	info, err := opened.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < offset || info.Size()-offset > codexRolloutMaxBytes {
		return 0, false, errors.New("rollout changed or exceeded its bound")
	}
	if _, err := opened.Seek(offset, io.SeekStart); err != nil {
		return 0, false, err
	}
	delta, err := io.ReadAll(io.LimitReader(opened, info.Size()-offset+1))
	if err != nil || int64(len(delta)) != info.Size()-offset || len(delta) == 0 || delta[len(delta)-1] != '\n' {
		return 0, false, errors.New("rollout delta is incomplete")
	}
	scanner := bufio.NewScanner(bytes.NewReader(delta))
	scanner.Buffer(make([]byte, 64<<10), codexRolloutMaxLine)
	calls := map[string]codexRolloutCall{}
	outputs := map[string]bool{}
	responseItems := 0
	matchedDenial := false
	sawSessionMeta := !needSessionMeta
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var wire struct {
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal(line, &wire); err != nil || wire.Type == "" {
			return 0, false, errors.New("malformed rollout record")
		}
		switch wire.Type {
		case "session_meta":
			var payload struct {
				ID string `json:"id"`
			}
			if len(wire.Payload) == 0 || json.Unmarshal(wire.Payload, &payload) != nil || payload.ID != threadID {
				return 0, false, errors.New("rollout thread metadata mismatch")
			}
			sawSessionMeta = true
		case "response_item":
			responseItems++
			var payload struct {
				Type      string          `json:"type"`
				CallID    string          `json:"call_id"`
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
				Output    json.RawMessage `json:"output"`
			}
			if len(wire.Payload) == 0 || json.Unmarshal(wire.Payload, &payload) != nil || payload.Type == "" {
				return 0, false, errors.New("malformed response item")
			}
			switch payload.Type {
			case "function_call":
				// Tool calls that the model later corrects are ordinary rollout
				// history, not corruption. Only a well-formed exec request can
				// establish that the exact policy response was a pre-execution
				// denial.
				if payload.CallID == "" || payload.Name == "" {
					return 0, false, errors.New("malformed function call")
				}
				if _, exists := calls[payload.CallID]; exists {
					return 0, false, errors.New("duplicate function call id")
				}
				call := codexRolloutCall{Name: payload.Name}
				if payload.Name == "exec_command" {
					call.Escalated = codexExecRequestsEscalation(payload.Arguments)
				}
				calls[payload.CallID] = call
			case "function_call_output":
				if payload.CallID == "" || len(payload.Output) == 0 {
					return 0, false, errors.New("malformed function output")
				}
				if outputs[payload.CallID] {
					return 0, false, errors.New("duplicate function output id")
				}
				outputs[payload.CallID] = true
				// Codex tool outputs are type-directed. Image and other
				// multimodal tools can return arrays; only a string exactly
				// matching the native policy error can be denial evidence.
				var output string
				if json.Unmarshal(payload.Output, &output) != nil {
					continue
				}
				if output == codexNeverPolicyError {
					call, correlated := calls[payload.CallID]
					matchedDenial = matchedDenial || correlated && call.Name == "exec_command" && call.Escalated
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, false, err
	}
	if !sawSessionMeta {
		return 0, false, errors.New("rollout is missing session metadata")
	}
	return responseItems, matchedDenial, nil
}

func codexExecRequestsEscalation(raw json.RawMessage) bool {
	var args any
	if len(raw) == 0 || json.Unmarshal(raw, &args) != nil {
		return false
	}
	if encoded, ok := args.(string); ok {
		if err := json.Unmarshal([]byte(encoded), &args); err != nil {
			return false
		}
	}
	object, ok := args.(map[string]any)
	if !ok {
		return false
	}
	command, ok := object["cmd"].(string)
	if !ok || strings.TrimSpace(command) == "" {
		return false
	}
	sandboxPermissions, hasSandboxPermissions := object["sandbox_permissions"].(string)
	if _, exists := object["sandbox_permissions"]; exists && !hasSandboxPermissions {
		return false
	}
	additional, hasAdditional := object["additional_permissions"]
	switch sandboxPermissions {
	case "require_escalated":
		if hasAdditional {
			return false
		}
		return true
	case "with_additional_permissions":
		if !hasAdditional || !validCodexAdditionalPermissions(additional) {
			return false
		}
		return true
	default:
		return false
	}
}

func validCodexAdditionalPermissions(raw any) bool {
	profile, ok := raw.(map[string]any)
	if !ok || len(profile) == 0 {
		return false
	}
	requested := false
	for key, value := range profile {
		switch key {
		case "network":
			network, ok := value.(map[string]any)
			if !ok {
				return false
			}
			enabled, ok := network["enabled"].(bool)
			if !ok || len(network) != 1 {
				return false
			}
			requested = requested || enabled
		case "file_system":
			fileSystem, ok := value.(map[string]any)
			if !ok {
				return false
			}
			if entries, ok := fileSystem["entries"].([]any); ok {
				requested = requested || len(entries) > 0
			} else {
				for _, field := range []string{"read", "write"} {
					paths, exists := fileSystem[field]
					if !exists {
						continue
					}
					list, ok := paths.([]any)
					if !ok {
						return false
					}
					requested = requested || len(list) > 0
				}
			}
		default:
			return false
		}
	}
	return requested
}
