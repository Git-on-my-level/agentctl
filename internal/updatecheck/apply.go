package updatecheck

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	defaultReleaseBase = "https://github.com/Git-on-my-level/agentctl/releases/download"
	maxChecksumBytes   = 1 << 20
	maxArchiveBytes    = 128 << 20
	maxExtractedBytes  = 256 << 20
)

type ApplyOptions struct {
	Check          Options
	Executable     string
	ReleaseBaseURL string
	Client         *http.Client
}

type ApplyResult struct {
	CurrentVersion   string `json:"current_version"`
	InstalledVersion string `json:"installed_version,omitempty"`
	Updated          bool   `json:"updated"`
}

type ApplyError struct {
	Code      string
	Retryable bool
	Cause     error
}

func (e *ApplyError) Error() string { return e.Cause.Error() }
func (e *ApplyError) Unwrap() error { return e.Cause }

// Apply checks once and installs a verified release only over an installation
// owned by the packaged agentctl installer.
func Apply(ctx context.Context, options ApplyOptions) (ApplyResult, error) {
	result := ApplyResult{CurrentVersion: options.Check.CurrentVersion}
	options.Check.DiscoveryOnly = true
	notice, err := Check(ctx, options.Check)
	if err != nil || notice == nil {
		return result, err
	}
	executable := options.Executable
	if executable == "" {
		executable, err = os.Executable()
		if err != nil {
			return result, recordApplyError(options.Check.StatePath, "executable_unavailable", err)
		}
	}
	prefix, err := managedPrefix(executable)
	if err != nil {
		return result, recordApplyError(options.Check.StatePath, "unmanaged_install", err)
	}

	client := options.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	base := strings.TrimRight(options.ReleaseBaseURL, "/")
	if base == "" {
		base = defaultReleaseBase
	}
	archiveName := fmt.Sprintf("agentctl_%s_%s_%s.tar.gz", notice.LatestVersion, runtime.GOOS, runtime.GOARCH)
	checksums, err := download(ctx, client, base+"/"+notice.LatestVersion+"/SHA256SUMS", maxChecksumBytes)
	if err != nil {
		return result, recordApplyError(options.Check.StatePath, "checksum_download_failed", err)
	}
	wantHash, err := checksumFor(checksums, archiveName)
	if err != nil {
		return result, recordApplyError(options.Check.StatePath, "checksum_missing", err)
	}
	archive, err := download(ctx, client, base+"/"+notice.LatestVersion+"/"+archiveName, maxArchiveBytes)
	if err != nil {
		return result, recordApplyError(options.Check.StatePath, "archive_download_failed", err)
	}
	digest := sha256.Sum256(archive)
	if !strings.EqualFold(hex.EncodeToString(digest[:]), wantHash) {
		return result, recordApplyError(options.Check.StatePath, "checksum_mismatch", errors.New("release archive checksum mismatch"))
	}

	root, err := os.MkdirTemp(filepath.Dir(options.Check.StatePath), ".agentctl-update.*")
	if err != nil {
		return result, recordApplyError(options.Check.StatePath, "staging_failed", err)
	}
	defer os.RemoveAll(root)
	if err := os.Chmod(root, 0o700); err != nil {
		return result, recordApplyError(options.Check.StatePath, "staging_failed", err)
	}
	if err := extractArchive(archive, root); err != nil {
		return result, recordApplyError(options.Check.StatePath, "archive_invalid", err)
	}
	packageRoot := filepath.Join(root, strings.TrimSuffix(archiveName, ".tar.gz"))
	binary := filepath.Join(packageRoot, "agentctl")
	installer := filepath.Join(packageRoot, "scripts", "install.sh")
	if !regularExecutable(binary) || !regularExecutable(installer) {
		return result, recordApplyError(options.Check.StatePath, "archive_invalid", errors.New("release archive omitted executable binary or installer"))
	}
	command := exec.CommandContext(ctx, installer, "--binary", binary, "--prefix", prefix, "--name", filepath.Base(executable))
	command.Env = replaceEnvironment(os.Environ(), "AGENTCTL_UPDATE_MODE", "off")
	var output boundedBuffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Run(); err != nil {
		return result, recordApplyError(options.Check.StatePath, "install_failed", fmt.Errorf("packaged installer failed: %w: %s", err, strings.TrimSpace(output.String())))
	}
	if err := recordInstalled(options.Check.StatePath, notice.LatestVersion); err != nil {
		return result, err
	}
	result.InstalledVersion, result.Updated = notice.LatestVersion, true
	return result, nil
}

func download(ctx context.Context, client *http.Client, url string, limit int64) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/octet-stream")
	request.Header.Set("User-Agent", "agentctl-updater")
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download returned HTTP %d", response.StatusCode)
	}
	if response.ContentLength > limit {
		return nil, errors.New("download exceeds size limit")
	}
	content, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(content)) > limit {
		return nil, errors.New("download exceeds size limit")
	}
	return content, nil
}

func checksumFor(manifest []byte, name string) (string, error) {
	found := ""
	for _, line := range strings.Split(string(manifest), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || strings.TrimPrefix(fields[1], "*") != name {
			continue
		}
		if len(fields[0]) != sha256.Size*2 {
			return "", errors.New("invalid SHA256SUMS entry")
		}
		if _, err := hex.DecodeString(fields[0]); err != nil || found != "" {
			return "", errors.New("invalid or duplicate SHA256SUMS entry")
		}
		found = strings.ToLower(fields[0])
	}
	if found == "" {
		return "", fmt.Errorf("SHA256SUMS does not contain %s", name)
	}
	return found, nil
}

func extractArchive(content []byte, destination string) error {
	gz, err := gzip.NewReader(bytes.NewReader(content))
	if err != nil {
		return err
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	var total int64
	for count := 0; ; count++ {
		if count >= 512 {
			return errors.New("release archive contains too many entries")
		}
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		// Archive names are POSIX paths. Reject every occurrence before using a
		// tainted entry in filesystem operations; release packages never require
		// a literal double-dot filename.
		if strings.Contains(header.Name, "..") {
			return errors.New("release archive contains an unsafe path")
		}
		clean := filepath.Clean(header.Name)
		if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return errors.New("release archive contains an unsafe path")
		}
		target := filepath.Join(destination, clean)
		relative, err := filepath.Rel(destination, target)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return errors.New("release archive escapes the staging directory")
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			total += header.Size
			if header.Size < 0 || total > maxExtractedBytes {
				return errors.New("release archive expands beyond size limit")
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}
			file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				return err
			}
			_, copyErr := io.CopyN(file, reader, header.Size)
			closeErr := file.Close()
			if copyErr != nil || closeErr != nil {
				return errors.Join(copyErr, closeErr)
			}
			if header.FileInfo().Mode()&0o111 != 0 {
				if err := os.Chmod(target, 0o700); err != nil {
					return err
				}
			}
		default:
			return errors.New("release archive contains a link or unsupported entry")
		}
	}
}

func managedPrefix(executable string) (string, error) {
	absolute, err := filepath.Abs(filepath.Clean(executable))
	if err != nil || absolute != executable {
		return "", errors.New("executable path is not absolute and clean")
	}
	if err := rejectSymlinkComponents(absolute); err != nil {
		return "", err
	}
	info, err := os.Lstat(absolute)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("executable is not a regular managed file")
	}
	prefix := filepath.Dir(filepath.Dir(absolute))
	manifest := filepath.Join(prefix, "share", "agentctl", "install-manifest")
	data, err := os.ReadFile(manifest)
	if err != nil {
		return "", err
	}
	manifestInfo, err := os.Lstat(manifest)
	if err != nil || !manifestInfo.Mode().IsRegular() || manifestInfo.Mode().Perm()&0o077 != 0 {
		return "", errors.New("install manifest is not an owner-only regular file")
	}
	wantTarget, wantHash := "target="+absolute, ""
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "sha256=") {
			wantHash = strings.TrimPrefix(line, "sha256=")
		}
	}
	if !linePresent(data, "manifest_version=1") || !linePresent(data, wantTarget) || len(wantHash) != sha256.Size*2 {
		return "", errors.New("install manifest does not own this executable")
	}
	content, err := os.ReadFile(absolute)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(content)
	if !strings.EqualFold(hex.EncodeToString(digest[:]), wantHash) {
		return "", errors.New("installed executable differs from its managed manifest")
	}
	return prefix, nil
}

func rejectSymlinkComponents(path string) error {
	volume := filepath.VolumeName(path)
	anchor := volume + string(os.PathSeparator)
	relative, err := filepath.Rel(anchor, path)
	if err != nil {
		return err
	}
	current := anchor
	for _, part := range strings.Split(relative, string(os.PathSeparator)) {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("managed install path contains a symlink")
		}
	}
	return nil
}

func linePresent(content []byte, want string) bool {
	for _, line := range strings.Split(string(content), "\n") {
		if line == want {
			return true
		}
	}
	return false
}

func regularExecutable(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 && info.Mode()&0o111 != 0
}

func replaceEnvironment(environment []string, name, value string) []string {
	prefix := name + "="
	result := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		if !strings.HasPrefix(entry, prefix) {
			result = append(result, entry)
		}
	}
	return append(result, prefix+value)
}

func recordInstalled(path, installed string) error {
	state, err := readState(path)
	if err != nil {
		return err
	}
	state.InstalledVersion = installed
	state.InstalledAt = time.Now().UTC()
	state.LastErrorCode, state.LastErrorAt = "", time.Time{}
	return writeState(path, state)
}

func recordApplyError(path, code string, cause error) error {
	state, err := readState(path)
	retryable := retryableApplyError(code)
	if err == nil {
		state.LastErrorCode = code
		state.LastErrorAt = time.Now().UTC()
		if retryable {
			state.CheckedOn = ""
			state.LastAttemptAt = state.LastErrorAt
		}
		err = writeState(path, state)
	}
	return &ApplyError{Code: code, Retryable: retryable, Cause: errors.Join(cause, err)}
}

func retryableApplyError(code string) bool {
	switch code {
	case "checksum_download_failed", "archive_download_failed", "staging_failed", "install_failed":
		return true
	default:
		return false
	}
}

type boundedBuffer struct{ bytes.Buffer }

func (b *boundedBuffer) Write(content []byte) (int, error) {
	const limit = 16 << 10
	original := len(content)
	if b.Len() < limit {
		remaining := limit - b.Len()
		if len(content) > remaining {
			content = content[:remaining]
		}
		_, _ = b.Buffer.Write(content)
	}
	return original, nil
}
