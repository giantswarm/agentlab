package lab

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSessionDirectoryProbeReadsOnlySelectedIDs(t *testing.T) {
	foreign := "/usr/bin"
	_, err := exec.CommandContext(t.Context(), "base64", "-w0").Output()
	if err != nil {
		t.Skip("probe requires GNU-compatible base64")
	}
	root := t.TempDir()
	sessions := filepath.Join(root, workspacesTestSessions)
	const ownedID = "owned'quoted"
	owned := filepath.Join(sessions, ownedID)
	require.NoError(t, os.MkdirAll(filepath.Join(owned, filepath.Dir(workspaceStateFile)), 0o700))
	recorded := workspaceState{SetUp: time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)}
	data, err := json.Marshal(recorded)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(owned, workspaceStateFile), data, 0o600))
	require.NoError(t, os.Symlink(foreign, filepath.Join(sessions, "foreign")))
	output := runWorkspaceOwnerScript(t, root, sessionDirectoriesScript([]string{ownedID}))
	directories, err := parseSessionDirectories(output)
	require.NoError(t, err)
	require.Len(t, directories, 1)
	require.NotNil(t, directories[ownedID].state)
	require.Equal(t, recorded.SetUp, directories[ownedID].state.SetUp)
}

func TestSessionContentsClearPreservesParentOwnedEntry(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("permission check requires a non-root user")
	}
	root := t.TempDir()
	sessions := filepath.Join(root, workspacesTestSessions)
	owned := filepath.Join(sessions, "owned")
	require.NoError(t, os.MkdirAll(owned, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(owned, "private"), []byte("fixture"), 0o600))
	t.Cleanup(func() {
		_ = os.Chmod(sessions, 0o700) // #nosec G302 -- owner-only directories require the search bit.
	})
	require.NoError(t, os.Chmod(sessions, 0o500)) // #nosec G302 -- owner-only directories require the search bit.
	runWorkspaceOwnerScript(t, root, sessionClearContentsScript([]string{"owned"}))
	entries, err := os.ReadDir(owned)
	require.NoError(t, err)
	require.Empty(t, entries)
	information, err := os.Stat(sessions)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o500), information.Mode().Perm())
	require.NoError(t, os.Chmod(sessions, 0o700)) // #nosec G302 -- owner-only directories require the search bit.
	runWorkspaceOwnerScript(t, root, sessionRemoveEntriesScript([]string{"owned"}))
	_, err = os.Stat(owned)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestSessionIDsFollowOwnerIdentity(t *testing.T) {
	actor := volumeOwner{65532, 65532}
	owners := map[string]volumeOwner{
		"z": actor, "a": actor, "sync": workspaceVolumeOwner,
		"other-group": {65532, 1000}, "root": {},
	}
	require.Equal(t, []string{"a", "z"}, sessionIDsForOwner(owners, actor))
	require.Equal(t, []string{"root"}, sessionIDsForOwner(owners, volumeOwner{}))
	require.Empty(t, sessionIDsForOwner(owners, volumeOwner{1234, 1234}))
}

func TestEmptySessionSelectionPreservesFiles(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, "keep")
	require.NoError(t, os.WriteFile(marker, []byte("fixture"), 0o600))
	for _, script := range []string{sessionDirectoriesScript(nil), sessionClearContentsScript(nil), sessionRemoveEntriesScript(nil)} {
		runWorkspaceOwnerScript(t, root, script)
		require.FileExists(t, marker)
	}
}

func runWorkspaceOwnerScript(t *testing.T, root, script string) string {
	t.Helper()
	script = strings.ReplaceAll(script, workspacesTestMountPath+"/", root+"/")
	// #nosec G204 -- script is a repository constant with a generated temporary path.
	command := exec.CommandContext(t.Context(), "sh", "-ec", script)
	output, err := command.CombinedOutput()
	require.NoError(t, err, "%s", output)
	return string(output)
}
