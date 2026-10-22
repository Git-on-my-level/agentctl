package updatecheck

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Git-on-my-level/agentctl/internal/testutil"
)

func TestInstallerStageSurvivesStatusWithoutRawOutput(t *testing.T) {
	root := testutil.TempDir(t)
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "update.json")
	if err := writeState(path, cacheState{SchemaVersion: 1}); err != nil {
		t.Fatal(err)
	}
	err := recordApplyError(path, "install_failed", installerDiagnostic(errors.New("failed"), "token=secret\nAGENTCTL_INSTALL_STAGE=bootstrap_preflight\nAGENTCTL_INSTALL_ROLLBACK=binary_restored_bootstrap_retained\n"))
	var d *ApplyError
	if !errors.As(err, &d) || d.Stage != "bootstrap_preflight" {
		t.Fatalf("diagnostic %v", err)
	}
	state, err := readState(path)
	if err != nil || state.LastErrorStage != "bootstrap_preflight" || state.LastErrorRollback != "binary_restored_bootstrap_retained" || strings.Contains(d.Error(), "secret") {
		t.Fatalf("state %#v err %v", state, err)
	}
	if err := recordInstalled(path, "v0.7.0"); err != nil {
		t.Fatal(err)
	}
	state, _ = readState(path)
	if state.LastErrorStage != "" || state.LastErrorRollback != "" {
		t.Fatal("stale failure stage")
	}
}
