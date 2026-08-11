package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/output"
)

const maxPromotionInputBytes = 1 << 20

func (a *app) promotionProvenance(contextPath string, handoff []byte) (model.ExecutionProvenance, []output.Warning, *output.Error) {
	value := model.ExecutionProvenance{}
	warnings := []output.Warning{}
	if len(handoff) > 0 {
		value.HandoffDigest = sha256Digest(handoff)
	}
	if strings.TrimSpace(contextPath) != "" {
		data, problem := readBoundedRegularFile(contextPath, "context file")
		if problem != nil {
			return value, nil, problem
		}
		value.ContextDigest = sha256Digest(data)
	}
	digest, state, problem := a.portableSkillDigest()
	if problem != nil {
		return value, nil, problem
	}
	value.PortableSkillDigest = digest
	if state == "missing" {
		warnings = append(warnings, output.Warning{Code: "portable_skill_unavailable", Message: "no installed agentctl-portable skill was found; promotion provenance omits its digest"})
	}
	return value, warnings, nil
}

func (a *app) portableSkillDigest() (string, string, *output.Error) {
	candidates := []string{}
	if a.getenv != nil {
		if explicit := strings.TrimSpace(a.getenv("AGENTCTL_SKILL_PATH")); explicit != "" {
			candidates = append(candidates, explicit)
		}
	}
	home := ""
	if a.getenv != nil {
		home = strings.TrimSpace(a.getenv("HOME"))
	}
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	if home != "" {
		for _, root := range []string{".agents/skills", ".codex/skills", ".hermes/skills", ".claude/skills", ".cursor/skills", ".omp/agent/skills"} {
			candidates = append(candidates, filepath.Join(home, filepath.FromSlash(root), "agentctl-portable", "SKILL.md"))
		}
	}
	digests := map[string]bool{}
	seen := map[string]bool{}
	for _, path := range candidates {
		absolute, err := filepath.Abs(path)
		if err != nil || seen[absolute] {
			continue
		}
		seen[absolute] = true
		info, err := os.Lstat(absolute)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return "", "", output.Wrap(output.CodeAuthorizationDenied, "inspect portable skill", false, err)
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return "", "", output.NewError(output.CodeAuthorizationDenied, "portable skill must be a regular non-symlink file", false)
		}
		if info.Size() > maxPromotionInputBytes {
			return "", "", output.NewError(output.CodeUsage, "portable skill exceeds 1 MiB", false)
		}
		data, err := os.ReadFile(absolute)
		if err != nil {
			return "", "", output.Wrap(output.CodeAuthorizationDenied, "read portable skill", false, err)
		}
		digests[sha256Digest(data)] = true
	}
	if len(digests) == 0 {
		return "", "missing", nil
	}
	if len(digests) > 1 {
		values := make([]string, 0, len(digests))
		for digest := range digests {
			values = append(values, digest)
		}
		sort.Strings(values)
		return "", "drifted", output.NewError(output.CodeConflict, "installed agentctl-portable skills have different digests", false).WithDetail("digests", values)
	}
	for digest := range digests {
		return digest, "installed", nil
	}
	return "", "missing", nil
}

func readBoundedRegularFile(path, label string) ([]byte, *output.Error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, output.Wrap(output.CodeUsage, "resolve "+label, false, err)
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, output.NewError(output.CodeNotFound, label+" not found", false)
		}
		return nil, output.Wrap(output.CodeAuthorizationDenied, "inspect "+label, false, err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, output.NewError(output.CodeAuthorizationDenied, label+" must be a regular non-symlink file", false)
	}
	if info.Size() > maxPromotionInputBytes {
		return nil, output.NewError(output.CodeUsage, label+" exceeds 1 MiB", false)
	}
	data, err := os.ReadFile(absolute)
	if err != nil {
		return nil, output.Wrap(output.CodeAuthorizationDenied, "read "+label, false, err)
	}
	return data, nil
}

func sha256Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func buildPromotionDescription(handoff []byte, sourceID fmt.Stringer, provenance model.ExecutionProvenance) []byte {
	var value bytes.Buffer
	if len(handoff) > 0 {
		value.Write(bytes.TrimSpace(handoff))
		value.WriteString("\n\n")
	}
	value.WriteString("Agentctl provenance:\n")
	value.WriteString("source_execution: ")
	value.WriteString(sourceID.String())
	value.WriteByte('\n')
	for _, item := range []struct{ name, digest string }{
		{"portable_skill_digest", provenance.PortableSkillDigest},
		{"context_digest", provenance.ContextDigest},
		{"handoff_digest", provenance.HandoffDigest},
	} {
		if item.digest != "" {
			value.WriteString(item.name)
			value.WriteString(": ")
			value.WriteString(item.digest)
			value.WriteByte('\n')
		}
	}
	return value.Bytes()
}
