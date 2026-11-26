//go:build integration

package checkpoint_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lonegunmanb/sofa/internal/artifact"
	"github.com/lonegunmanb/sofa/internal/blob"
	"github.com/lonegunmanb/sofa/internal/checkpoint"
	"github.com/lonegunmanb/sofa/internal/git"
	"github.com/lonegunmanb/sofa/internal/session"
	"github.com/stretchr/testify/require"
)

func runIntegrationGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v failed: %s", args, string(out))
	return strings.TrimSpace(string(out))
}

func initIntegrationBareRepo(t *testing.T) (bareDir, baseSHA string) {
	t.Helper()
	root := t.TempDir()
	bareDir = filepath.Join(root, "remote.git")
	seedDir := filepath.Join(root, "seed")

	runIntegrationGit(t, root, "init", "--bare", bareDir)
	require.NoError(t, os.MkdirAll(seedDir, 0o755))
	runIntegrationGit(t, seedDir, "init")
	runIntegrationGit(t, seedDir, "config", "user.name", "seed")
	runIntegrationGit(t, seedDir, "config", "user.email", "seed@sofa.local")
	runIntegrationGit(t, seedDir, "config", "commit.gpgsign", "false")

	require.NoError(t, os.MkdirAll(filepath.Join(seedDir, "cmd"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(seedDir, "cmd", "app.go"), []byte("package main\nfunc main() {}\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(seedDir, "README.md"), []byte("base readme\n"), 0o644))

	runIntegrationGit(t, seedDir, "add", "-A")
	runIntegrationGit(t, seedDir, "commit", "-m", "initial")
	runIntegrationGit(t, seedDir, "branch", "-M", "main")
	runIntegrationGit(t, seedDir, "push", bareDir, "HEAD:refs/heads/main")
	baseSHA = runIntegrationGit(t, seedDir, "rev-parse", "HEAD")
	return bareDir, baseSHA
}

func TestCrossMachineCheckpointAndRecoveryIntegration(t *testing.T) {
	ctx := context.Background()
	bareDir, baseSHA := initIntegrationBareRepo(t)

	sharedRoot := t.TempDir()
	blobStore, err := blob.NewLocalStore(filepath.Join(sharedRoot, "blobs"))
	require.NoError(t, err)
	artSvc := artifact.NewService(blobStore, artifact.ServiceOptions{})
	statePath := filepath.Join(sharedRoot, "control-plane", "node-state.json")

	runID := "run-int-1"
	nodeAddr := "worker.implement"
	writable := []string{"cmd/**"}

	// --- Machine A (epoch 1) ---
	machineADir := filepath.Join(t.TempDir(), "machine-a")
	wsA := filepath.Join(machineADir, "workspace")
	artA := filepath.Join(machineADir, "artifacts")
	require.NoError(t, os.MkdirAll(artA, 0o755))

	ledgerA := git.NewLedger(git.SourceRef{URL: bareDir, Branch: "main"})
	gitA := git.NewCLIAdapter(git.CLIAdapterOptions{Ledger: ledgerA})
	storeA, err := checkpoint.NewLocalNodeStateStore(checkpoint.LocalNodeStateStoreOptions{
		InitialEpoch: 1,
		Ledger:       ledgerA,
		OwnerNode:    nodeAddr,
		StatePath:    statePath,
	})
	require.NoError(t, err)

	require.NoError(t, gitA.PrepareWorkspace(ctx, bareDir, "main", baseSHA, wsA, git.AuthConfig{}))

	mgrA, err := checkpoint.NewManager(checkpoint.ManagerConfig{
		RunID:       runID,
		NodeAddr:    nodeAddr,
		RepoURL:     bareDir,
		StateStore:  storeA,
		GitAdapter:  gitA,
		BlobStore:   blobStore,
		ArtifactSvc: artSvc,
	})
	require.NoError(t, err)

	// Checkpoint 1 completes cleanly with code + /.sofa/artifacts/draft.md.
	v1Code := "package main\nfunc main() { println(\"ckpt1\") }\n"
	require.NoError(t, os.WriteFile(filepath.Join(wsA, "cmd", "app.go"), []byte(v1Code), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(artA, "draft.md"), []byte("# Draft v1\n"), 0o644))

	diary1 := session.DiaryInput{
		Completed:    []string{"implemented ckpt1 handler"},
		TestEvidence: []string{"go test ok"},
		NextSteps:    []string{"add ckpt2 feature"},
	}
	ckpt1, err := mgrA.SaveCheckpoint(ctx, 1, wsA, artA, writable, diary1)
	require.NoError(t, err)
	require.Equal(t, uint64(1), ckpt1.Seq)
	require.Equal(t, checkpoint.StatusComplete, ckpt1.Status)
	require.NotEqual(t, baseSHA, ckpt1.FinalSHA)
	require.NotNil(t, ckpt1.ArtifactsSnapshot)

	// Machine A begins Checkpoint 2 edits and crashes after writing StatusPending + pushing uncommitted/partial state.
	require.NoError(t, os.WriteFile(filepath.Join(wsA, "cmd", "app.go"), []byte("package main\nfunc main() { println(\"crashed-ckpt2\") }\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(artA, "draft.md"), []byte("# Draft v2 crashed\n"), 0o644))
	diary2Crash := session.DiaryInput{
		Completed: []string{"partial ckpt2 work"},
		NextSteps: []string{"finish ckpt2"},
	}
	seq2, err := storeA.BeginCheckpoint(ctx, 1, ckpt1.Branch, ckpt1.FinalSHA, diary2Crash)
	require.NoError(t, err)
	require.Equal(t, uint64(2), seq2)

	// Simulate total Machine A loss.
	require.NoError(t, os.RemoveAll(machineADir))

	// --- Machine B (epoch 2) ---
	machineBDir := filepath.Join(t.TempDir(), "machine-b")
	wsB := filepath.Join(machineBDir, "workspace")
	artB := filepath.Join(machineBDir, "artifacts")

	ledgerB := git.NewLedger(git.SourceRef{URL: bareDir, Branch: "main"})
	gitB := git.NewCLIAdapter(git.CLIAdapterOptions{Ledger: ledgerB})
	storeB, err := checkpoint.NewLocalNodeStateStore(checkpoint.LocalNodeStateStoreOptions{
		Ledger:    ledgerB,
		OwnerNode: nodeAddr,
		StatePath: statePath,
	})
	require.NoError(t, err)
	require.NoError(t, storeB.AdvanceEpoch(2))

	mgrB, err := checkpoint.NewManager(checkpoint.ManagerConfig{
		RunID:       runID,
		NodeAddr:    nodeAddr,
		RepoURL:     bareDir,
		StateStore:  storeB,
		GitAdapter:  gitB,
		BlobStore:   blobStore,
		ArtifactSvc: artSvc,
	})
	require.NoError(t, err)

	recRes, err := mgrB.RecoverLatestAligned(ctx, 2, wsB, artB)
	require.NoError(t, err)
	require.Equal(t, uint64(1), recRes.Record.Seq)
	require.Equal(t, ckpt1.FinalSHA, recRes.Record.FinalSHA)
	require.Equal(t, git.CheckpointBranchName(runID, nodeAddr, 2), recRes.NewCheckpointBranch)

	// Verify 100% restored workspace code, artifacts, and chronological diary (without crashed seq2 diary).
	gotCode, err := os.ReadFile(filepath.Join(wsB, "cmd", "app.go"))
	require.NoError(t, err)
	require.Equal(t, v1Code, strings.ReplaceAll(string(gotCode), "\r\n", "\n"))

	gotDraft, err := os.ReadFile(filepath.Join(artB, "draft.md"))
	require.NoError(t, err)
	require.Equal(t, "# Draft v1\n", string(gotDraft))

	require.Len(t, recRes.ChronologicalDiaries, 1)
	require.Contains(t, recRes.DiaryPromptContext, "implemented ckpt1 handler")
	require.NotContains(t, recRes.DiaryPromptContext, "partial ckpt2 work")

	// Machine B modifies code and artifacts and pushes a new Checkpoint on e2 branch.
	v2CodeB := "package main\nfunc main() { println(\"recovered-e2\") }\n"
	require.NoError(t, os.WriteFile(filepath.Join(wsB, "cmd", "app.go"), []byte(v2CodeB), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(artB, "draft.md"), []byte("# Draft v2 on B\n"), 0o644))

	diary3 := session.DiaryInput{
		Completed: []string{"finished feature on machine B"},
		NextSteps: []string{"submit to QC"},
	}
	ckptB, err := mgrB.SaveCheckpoint(ctx, 2, wsB, artB, writable, diary3)
	require.NoError(t, err)
	require.Equal(t, uint64(3), ckptB.Seq)
	require.Equal(t, uint64(2), ckptB.Epoch)
	require.Equal(t, recRes.NewCheckpointBranch, ckptB.Branch)
	require.Equal(t, ckpt1.FinalSHA, ckptB.InitialSHA)
	require.NotEqual(t, ckpt1.FinalSHA, ckptB.FinalSHA)
	require.Equal(t, checkpoint.StatusComplete, ckptB.Status)

	headSHA, contains, err := gitB.BranchContainsSHA(ctx, bareDir, recRes.NewCheckpointBranch, ckptB.FinalSHA, git.AuthConfig{})
	require.NoError(t, err)
	require.True(t, contains)
	require.Equal(t, ckptB.FinalSHA, headSHA)
}
