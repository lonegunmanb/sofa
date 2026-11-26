package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	goruntime "runtime"
	"strings"

	"github.com/lonegunmanb/sofa/internal/git"
	sandboxruntime "github.com/lonegunmanb/sofa/internal/sandbox/runtime"
)

// ExecuteMergeCandidate performs deterministic candidate rebase onto targetBranch, optional
// container gatekeeper verification, and lease-protected target branch advancement.
func ExecuteMergeCandidate(ctx context.Context, req MergeCandidateRequest) (MergeCandidateResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return MergeCandidateResult{}, err
	}
	if strings.TrimSpace(req.RunID) == "" {
		return MergeCandidateResult{}, errors.New("merge candidate: run_id is required")
	}
	if strings.TrimSpace(req.MergeNodeAddr) == "" {
		return MergeCandidateResult{}, errors.New("merge candidate: merge_node_addr is required")
	}
	if req.Epoch == 0 {
		return MergeCandidateResult{}, errors.New("merge candidate: epoch must be > 0")
	}
	if strings.TrimSpace(req.RepoURL) == "" {
		return MergeCandidateResult{}, errors.New("merge candidate: repo_url is required")
	}
	if strings.TrimSpace(req.CandidateBranch) == "" {
		return MergeCandidateResult{}, errors.New("merge candidate: candidate_branch is required")
	}
	if req.GitAdapter == nil {
		return MergeCandidateResult{}, errors.New("merge candidate: git_adapter is required")
	}

	targetBranch := git.WorkBranchName(req.RunID, req.MergeNodeAddr)
	if req.TargetBranch != nil && strings.TrimSpace(*req.TargetBranch) != "" {
		targetBranch = strings.TrimSpace(*req.TargetBranch)
	}

	engine := req.SandboxEngine
	if engine == nil {
		engine = sandboxruntime.NewPodmanEngine(sandboxruntime.PodmanEngineOptions{
			GitAdapter: req.GitAdapter,
		})
	}

	hostOS := strings.TrimSpace(req.HostOS)
	if hostOS == "" {
		hostOS = goruntime.GOOS
	}

	// Probe whether targetBranch already exists on remote without relying on error string parsing.
	existingTip, _, err := req.GitAdapter.BranchContainsSHA(ctx, req.RepoURL, targetBranch, strings.Repeat("0", 40), req.Auth)
	if err != nil {
		return MergeCandidateResult{}, fmt.Errorf("inspect target branch %q: %w", targetBranch, err)
	}

	baseSHA := strings.TrimSpace(req.BaseSHA)
	resolveBaseSHA := func() (string, error) {
		if baseSHA != "" {
			return baseSHA, nil
		}
		if strings.TrimSpace(req.BasedSource) != "" {
			resolved, rerr := req.GitAdapter.ResolveBranchSHA(ctx, req.RepoURL, req.BasedSource, req.Auth)
			if rerr != nil {
				return "", rerr
			}
			baseSHA = resolved
			return baseSHA, nil
		}
		if existingTip != "" {
			baseSHA = existingTip
			return baseSHA, nil
		}
		return "", errors.New("merge candidate: base_sha or based_source is required to initialize target branch")
	}

	if req.Ledger != nil {
		if ensureErr := req.Ledger.EnsureRegistered(req.RepoURL, targetBranch); ensureErr != nil {
			if !errors.Is(ensureErr, git.ErrUnregisteredBranch) {
				return MergeCandidateResult{}, ensureErr
			}
			pinBaseSHA, rerr := resolveBaseSHA()
			if rerr != nil {
				return MergeCandidateResult{}, rerr
			}
			if regErr := req.Ledger.Register(req.RepoURL, targetBranch, pinBaseSHA, git.KindMergeBranch, req.MergeNodeAddr, req.Epoch); regErr != nil {
				return MergeCandidateResult{}, regErr
			}
		}
	}

	if existingTip == "" {
		pinBaseSHA, rerr := resolveBaseSHA()
		if rerr != nil {
			return MergeCandidateResult{}, rerr
		}
		initDir, mkErr := os.MkdirTemp(req.TempRoot, "sofa-merge-init-*")
		if mkErr != nil {
			return MergeCandidateResult{}, fmt.Errorf("create merge init dir: %w", mkErr)
		}
		defer func() {
			_ = os.RemoveAll(initDir)
		}()
		sourceBranch := strings.TrimSpace(req.BasedSource)
		if sourceBranch == "" {
			return MergeCandidateResult{}, errors.New("merge candidate: based_source is required to initialize missing target branch")
		}
		if err := req.GitAdapter.PrepareWorkspace(ctx, req.RepoURL, sourceBranch, pinBaseSHA, initDir, req.Auth); err != nil {
			return MergeCandidateResult{}, fmt.Errorf("prepare initial target workspace: %w", err)
		}
		if err := req.GitAdapter.PushCommit(ctx, initDir, req.RepoURL, targetBranch, pinBaseSHA, "", req.Auth); err != nil {
			return MergeCandidateResult{}, fmt.Errorf("initialize target branch %q: %w", targetBranch, err)
		}
	}

	tempWorkDir, err := os.MkdirTemp(req.TempRoot, "sofa-merge-work-*")
	if err != nil {
		return MergeCandidateResult{}, fmt.Errorf("create merge work dir: %w", err)
	}
	defer func() {
		_ = os.RemoveAll(tempWorkDir)
	}()

	rebaseOut, err := req.GitAdapter.RebaseCandidate(ctx, req.RepoURL, targetBranch, req.CandidateBranch, tempWorkDir, req.Auth)
	if err != nil {
		return MergeCandidateResult{}, fmt.Errorf("rebase candidate %q onto %q: %w", req.CandidateBranch, targetBranch, err)
	}
	if rebaseOut.Conflicted {
		return MergeCandidateResult{
			Status:          MergeStatusReworkSource,
			TargetBranch:    targetBranch,
			LatestTargetSHA: rebaseOut.TargetSHA,
			ConflictFiles:   append([]string(nil), rebaseOut.ConflictFiles...),
			FailureReport:   rebaseOut.Details,
		}, nil
	}

	if len(req.Gatekeepers) > 0 {
		gkFailure, gkErr := engine.RunGatekeepers(ctx, sandboxruntime.GatekeeperRunSpec{
			Sandbox:          req.Sandbox,
			HostWorkspaceDir: tempWorkDir,
			BundleDir:        req.BundleDir,
			ImportsDir:       req.ImportsDir,
			HostOS:           hostOS,
		}, req.Gatekeepers)
		if gkErr != nil {
			return MergeCandidateResult{}, fmt.Errorf("run merge gatekeepers: %w", gkErr)
		}
		if gkFailure != nil {
			return MergeCandidateResult{
				Status:            MergeStatusReworkSource,
				TargetBranch:      targetBranch,
				LatestTargetSHA:   rebaseOut.TargetSHA,
				GatekeeperFailure: gkFailure,
				FailureReport:     formatGatekeeperFailurePrompt(gkFailure),
			}, nil
		}
	}

	if err := req.GitAdapter.PushCommit(ctx, tempWorkDir, req.RepoURL, targetBranch, rebaseOut.RebasedSHA, rebaseOut.TargetSHA, req.Auth); err != nil {
		return MergeCandidateResult{}, fmt.Errorf("advance target branch %q: %w", targetBranch, err)
	}

	return MergeCandidateResult{
		Status:          MergeStatusSucceeded,
		TargetBranch:    targetBranch,
		LatestTargetSHA: rebaseOut.RebasedSHA,
		MergedCommitSHA: rebaseOut.RebasedSHA,
	}, nil
}
