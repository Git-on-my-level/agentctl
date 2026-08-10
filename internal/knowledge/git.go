package knowledge

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// GitRunner is intentionally argv-based. Implementations must not invoke a
// shell, and credentials remain wholly owned by native Git/SSH.
type GitRunner interface {
	Run(ctx context.Context, dir string, args ...string) ([]byte, error)
}

type NativeGit struct{ Binary string }

func (g NativeGit) Run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	binary := g.Binary
	if binary == "" {
		binary = "git"
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = dir
	cmd.Env = gitEnvironment()
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(redactGitArgs(args), " "), err, redactGitText(strings.TrimSpace(out.String())))
	}
	return out.Bytes(), nil
}

func gitEnvironment() []string {
	env := make([]string, 0, len(os.Environ())+1)
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "GIT_TERMINAL_PROMPT=") {
			continue
		}
		env = append(env, entry)
	}
	return append(env, "GIT_TERMINAL_PROMPT=0")
}

func redactGitArgs(args []string) []string {
	redacted := make([]string, len(args))
	for i, arg := range args {
		redacted[i] = redactGitToken(arg)
	}
	return redacted
}

func redactGitText(text string) string {
	parts := strings.Fields(text)
	for i, part := range parts {
		parts[i] = redactGitToken(part)
	}
	return strings.Join(parts, " ")
}

func redactGitToken(token string) string {
	if strings.Contains(token, "://") || (strings.Contains(token, "@") && strings.Contains(token, ":")) {
		return "<redacted-remote>"
	}
	lower := strings.ToLower(token)
	for _, marker := range []string{"password=", "token=", "secret=", "authorization:"} {
		if idx := strings.Index(lower, marker); idx >= 0 {
			return token[:idx] + "<redacted>"
		}
	}
	return token
}

// SyncSource performs the only operation in this package which changes a
// checkout. Callers must opt into it explicitly. Read/compile functions only
// inspect an existing checkout and never call SyncSource.
func SyncSource(ctx context.Context, reg SourceRegistration, checkoutDir string) (SyncResult, error) {
	return SyncSourceWithGit(ctx, NativeGit{}, reg, checkoutDir)
}

// Sync is the explicit-write alias; no read path calls it implicitly.
func Sync(ctx context.Context, reg SourceRegistration, checkoutDir string) (SyncResult, error) {
	return SyncSource(ctx, reg, checkoutDir)
}

func SyncSourceWithGit(ctx context.Context, git GitRunner, reg SourceRegistration, checkoutDir string) (SyncResult, error) {
	if git == nil {
		return SyncResult{}, fmt.Errorf("nil git runner")
	}
	if err := ValidateSourceRegistration(reg); err != nil {
		return SyncResult{}, err
	}
	if checkoutDir == "" {
		return SyncResult{}, fmt.Errorf("checkout directory is required")
	}
	if abs, err := filepath.Abs(checkoutDir); err != nil || filepath.Base(abs) == ".git" {
		return SyncResult{}, fmt.Errorf("invalid checkout directory")
	}
	if err := secureMkdirAll(checkoutDir); err != nil {
		return SyncResult{}, err
	}
	// init is harmless on a new checkout and deterministic on subsequent syncs.
	if _, err := git.Run(ctx, checkoutDir, "init", "--quiet"); err != nil {
		return SyncResult{}, err
	}
	if _, err := git.Run(ctx, checkoutDir, "remote", "get-url", "origin"); err != nil {
		if _, addErr := git.Run(ctx, checkoutDir, "remote", "add", "origin", reg.Remote.URL); addErr != nil {
			return SyncResult{}, addErr
		}
	} else if _, setErr := git.Run(ctx, checkoutDir, "remote", "set-url", "origin", reg.Remote.URL); setErr != nil {
		return SyncResult{}, setErr
	}
	if _, err := git.Run(ctx, checkoutDir, "fetch", "--no-tags", "--depth=1", "origin", reg.Ref); err != nil {
		return SyncResult{}, err
	}
	if _, err := git.Run(ctx, checkoutDir, "checkout", "--detach", "FETCH_HEAD"); err != nil {
		return SyncResult{}, err
	}
	commitBytes, err := git.Run(ctx, checkoutDir, "rev-parse", "HEAD^{commit}")
	if err != nil {
		return SyncResult{}, err
	}
	treeBytes, err := git.Run(ctx, checkoutDir, "rev-parse", "HEAD^{tree}")
	if err != nil {
		return SyncResult{}, err
	}
	return SyncResult{CheckoutDir: checkoutDir, Commit: strings.TrimSpace(string(commitBytes)), TreeDigest: "sha256:" + strings.TrimSpace(string(treeBytes))}, nil
}

// ResolveRevision reads Git metadata only. It performs no fetch, checkout,
// remote update, or other write.
func ResolveRevision(ctx context.Context, git GitRunner, checkoutDir string) (commit, tree string, err error) {
	if git == nil {
		git = NativeGit{}
	}
	if err := rejectSymlinkComponents(checkoutDir, false); err != nil {
		return "", "", err
	}
	b, err := git.Run(ctx, checkoutDir, "rev-parse", "HEAD^{commit}")
	if err != nil {
		return "", "", err
	}
	t, err := git.Run(ctx, checkoutDir, "rev-parse", "HEAD^{tree}")
	if err != nil {
		return "", "", err
	}
	return strings.TrimSpace(string(b)), "sha256:" + strings.TrimSpace(string(t)), nil
}
