package knowledge

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	privateDirMode  os.FileMode = 0o700
	privateFileMode os.FileMode = 0o600
)

// rejectSymlinkComponents checks every existing component, including the
// target itself. If allowMissing is true, the first missing component ends
// the walk; callers may then create the remaining path themselves.
func rejectSymlinkComponents(name string, allowMissing bool) error {
	abs, err := filepath.Abs(name)
	if err != nil {
		return err
	}
	volume := filepath.VolumeName(abs)
	part := volume + string(filepath.Separator)
	rest := abs[len(volume):]
	for _, component := range splitPathComponents(rest) {
		if component == "" || component == "." {
			continue
		}
		part = filepath.Join(part, component)
		info, statErr := os.Lstat(part)
		if os.IsNotExist(statErr) {
			if allowMissing {
				return nil
			}
			return fmt.Errorf("path component does not exist: %s", part)
		}
		if statErr != nil {
			return statErr
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink path component rejected: %s", part)
		}
	}
	return nil
}

func splitPathComponents(name string) []string {
	clean := filepath.Clean(name)
	if clean == string(filepath.Separator) {
		return nil
	}
	for len(clean) > 0 && clean[0] == filepath.Separator {
		clean = clean[1:]
	}
	if clean == "" {
		return nil
	}
	return strings.Split(filepath.ToSlash(clean), "/")
}

func secureMkdirAll(name string) error {
	if err := rejectSymlinkComponents(name, true); err != nil {
		return err
	}
	if err := os.MkdirAll(name, privateDirMode); err != nil {
		return err
	}
	return chmodPrivateDirTree(name)
}

func chmodPrivateDirTree(name string) error {
	info, err := os.Lstat(name)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("symlink path component rejected: %s", name)
	}
	if !info.IsDir() {
		return fmt.Errorf("expected directory: %s", name)
	}
	if err := os.Chmod(name, privateDirMode); err != nil {
		return err
	}
	return nil
}

func secureWriteFile(name string, data []byte) error {
	if err := rejectSymlinkComponents(filepath.Dir(name), false); err != nil {
		return err
	}
	if info, err := os.Lstat(name); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink path component rejected: %s", name)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.WriteFile(name, data, privateFileMode); err != nil {
		return err
	}
	return os.Chmod(name, privateFileMode)
}

func secureReadFile(name string) ([]byte, error) {
	if err := rejectSymlinkComponents(name, false); err != nil {
		return nil, err
	}
	return os.ReadFile(name)
}

func securePathAbsent(name string) error {
	if err := rejectSymlinkComponents(filepath.Dir(name), true); err != nil {
		return err
	}
	info, err := os.Lstat(name)
	if err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink path component rejected: %s", name)
		}
		return fmt.Errorf("path already exists: %s", name)
	}
	if !os.IsNotExist(err) {
		return err
	}
	return nil
}
