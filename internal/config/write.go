package config

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"
)

// ConfigFileMode is the mode used for an agentctl configuration. Configuration
// files can contain authority and endpoint information, so group and world
// access is never enabled.
const ConfigFileMode fs.FileMode = 0o600

var (
	// ErrConflict indicates that a path contains a managed configuration which
	// cannot be replaced by the requested operation.
	ErrConflict = errors.New("agentctl config conflict")
	// ErrUnmanaged indicates that an existing path is not an owner-only,
	// syntactically valid agentctl configuration.
	ErrUnmanaged = errors.New("agentctl config is unmanaged")
	// ErrUnsafePath indicates that a path component is a symbolic link. Save
	// refuses links even when overwrite is requested, preventing writes from
	// being redirected outside the explicitly selected path.
	ErrUnsafePath = errors.New("agentctl config path contains a symbolic link")
)

// Save atomically writes cfg to path using an owner-only file. Missing parent
// directories are created owner-only. Existing valid, owner-only configs are
// treated as managed and may be updated; invalid, malformed, or broadly
// permissioned files require overwrite=true. Symbolic-link components are
// always rejected.
//
// Save performs no network, credential, or cache operation. It validates the
// complete document before creating a temporary file and fsyncs both the file
// and its containing directory before returning.
func Save(path string, cfg Config, overwrite bool) error {
	path, err := cleanSavePath(path)
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("validate config: %w", err)
	}
	document, err := marshalConfig(cfg)
	if err != nil {
		return err
	}

	parent := filepath.Dir(path)
	if err := ensureOwnerPath(parent); err != nil {
		return err
	}

	managed, exists, existing, err := existingConfigState(path)
	if err != nil {
		return err
	}
	if exists && !managed && !overwrite {
		return fmt.Errorf("%w: %s (pass overwrite=true to replace)", ErrUnmanaged, path)
	}
	if exists && managed && !overwrite {
		if reflect.DeepEqual(existing, cfg) {
			return nil
		}
		return fmt.Errorf("%w: managed config differs at %s (pass overwrite=true to replace)", ErrConflict, path)
	}

	// Keep temporary files beside the destination so rename is atomic on the
	// same filesystem. O_EXCL prevents us from ever opening an attacker-chosen
	// pre-existing temporary path.
	tmp, err := os.OpenFile(path+".tmp-"+randomSuffix(), os.O_WRONLY|os.O_CREATE|os.O_EXCL, ConfigFileMode)
	if err != nil {
		return fmt.Errorf("create config temporary file: %w", err)
	}
	tmpPath := tmp.Name()
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(ConfigFileMode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("set config temporary mode: %w", err)
	}
	if _, err := tmp.Write(document); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write config temporary file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync config temporary file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close config temporary file: %w", err)
	}

	// Check the destination immediately before rename. Rename replaces a
	// symlink itself, but refusing a newly introduced link keeps the operation
	// consistent with the path safety contract.
	if info, statErr := os.Lstat(path); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return ErrUnsafePath
		}
	} else if !errors.Is(statErr, fs.ErrNotExist) {
		return fmt.Errorf("inspect config destination: %w", statErr)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace config: %w", err)
	}
	removeTemp = false
	if err := syncDirectory(parent); err != nil {
		return err
	}
	return nil
}

func cleanSavePath(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("config path is required")
	}
	clean := filepath.Clean(path)
	if clean == "." || clean == string(filepath.Separator) {
		return "", errors.New("config path must name a file")
	}
	absolute, err := filepath.Abs(clean)
	if err != nil {
		return "", fmt.Errorf("resolve config path: %w", err)
	}
	return filepath.Clean(absolute), nil
}

// ensureOwnerPath verifies every existing component and creates absent
// directories. It intentionally uses Lstat, never Stat, so symlinks are not
// followed while walking the requested path.
func ensureOwnerPath(path string) error {
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return fmt.Errorf("resolve config directory: %w", err)
	}
	volume := filepath.VolumeName(abs)
	remainder := strings.TrimPrefix(abs, volume)
	root := volume + string(filepath.Separator)
	if volume != "" && !strings.HasPrefix(remainder, string(filepath.Separator)) {
		root = volume
	}
	current := root
	for _, component := range strings.Split(strings.TrimPrefix(remainder, string(filepath.Separator)), string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}
		candidate := filepath.Join(current, component)
		info, statErr := os.Lstat(candidate)
		if statErr == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("%w: %s", ErrUnsafePath, candidate)
			}
			if !info.IsDir() {
				return fmt.Errorf("config parent %s is not a directory", candidate)
			}
			current = candidate
			continue
		}
		if !errors.Is(statErr, fs.ErrNotExist) {
			return fmt.Errorf("inspect config directory %s: %w", candidate, statErr)
		}
		if mkdirErr := os.Mkdir(candidate, 0o700); mkdirErr != nil && !errors.Is(mkdirErr, fs.ErrExist) {
			return fmt.Errorf("create config directory %s: %w", candidate, mkdirErr)
		}
		info, statErr = os.Lstat(candidate)
		if statErr != nil {
			return fmt.Errorf("inspect created config directory %s: %w", candidate, statErr)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: %s", ErrUnsafePath, candidate)
		}
		if !info.IsDir() {
			return fmt.Errorf("config parent %s is not a directory", candidate)
		}
		current = candidate
	}
	return nil
}

func existingConfigState(path string) (managed, exists bool, existing Config, err error) {
	info, statErr := os.Lstat(path)
	if errors.Is(statErr, fs.ErrNotExist) {
		return false, false, Config{}, nil
	}
	if statErr != nil {
		return false, false, Config{}, fmt.Errorf("inspect config destination: %w", statErr)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return false, true, Config{}, ErrUnsafePath
	}
	if !info.Mode().IsRegular() {
		return false, true, Config{}, fmt.Errorf("%w: destination is not a regular file", ErrUnmanaged)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return false, true, Config{}, nil
	}
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		return false, true, Config{}, nil
	}
	existing, parseErr := decodeConfig(data)
	if parseErr != nil {
		return false, true, Config{}, nil
	}
	return true, true, existing, nil
}

func marshalConfig(cfg Config) ([]byte, error) {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal config: %w", err)
	}
	return append(data, '\n'), nil
}

func decodeConfig(data []byte) (Config, error) {
	var cfg Config
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return Config{}, errors.New("parse config: trailing JSON document")
		}
		return Config{}, fmt.Errorf("parse config: trailing JSON document: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open config directory for sync: %w", err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		// Some filesystems/platforms do not support syncing directories. Return
		// the error on platforms that do, because Save promises durability.
		return fmt.Errorf("sync config directory: %w", err)
	}
	return nil
}

// randomSuffix avoids collisions without using a process-global temporary
// name. It does not provide security; O_EXCL provides the safety property.
func randomSuffix() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err == nil {
		return fmt.Sprintf("%x", raw[:])
	}
	// Randomness is only needed to avoid a temporary-name collision. The
	// process id/time fallback remains protected by O_EXCL.
	return fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
}
