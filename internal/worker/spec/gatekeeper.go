package spec

import (
	"slices"
	"strings"
	"time"

	"github.com/hashicorp/hcl/v2"
	"github.com/lonegunmanb/golden"
)

var gatekeeperAllowedAttrs = map[string]struct{}{
	"command":                {},
	"working_dir":            {},
	"timeout":                {},
	"expect_exit_code":       {},
	"require_clean_worktree": {},
}

type GatekeeperConfig struct {
	Name             string
	Command          []string
	WorkingDir       string
	Timeout          string
	ExpectExitCode   int
	RequireCleanTree bool
}

func (g GatekeeperConfig) Clone() GatekeeperConfig {
	cloned := g
	cloned.Command = slices.Clone(g.Command)
	return cloned
}

type GatekeeperBlock struct {
	Name             string
	Command          []string
	WorkingDir       string
	Timeout          string
	ExpectExitCode   int
	RequireCleanTree *bool
}

func (g GatekeeperBlock) Config() GatekeeperConfig {
	timeout := g.Timeout
	if timeout == "" {
		timeout = "10m"
	}
	workingDir := g.WorkingDir
	if workingDir == "" {
		workingDir = "."
	}
	requireCleanTree := true
	if g.RequireCleanTree != nil {
		requireCleanTree = *g.RequireCleanTree
	}
	return GatekeeperConfig{
		Name:             g.Name,
		Command:          slices.Clone(g.Command),
		WorkingDir:       workingDir,
		Timeout:          timeout,
		ExpectExitCode:   g.ExpectExitCode,
		RequireCleanTree: requireCleanTree,
	}
}

func validateGatekeeper(g GatekeeperConfig) error {
	if err := validateSafeLabel(g.Name, "gatekeeper", false); err != nil {
		return err
	}
	if len(g.Command) == 0 || strings.TrimSpace(g.Command[0]) == "" {
		return fail("gatekeeper %q command must be a non-empty list", g.Name)
	}
	if err := validateWorkspaceRelativePath(g.WorkingDir, "gatekeeper working_dir", true); err != nil {
		return err
	}
	duration, err := time.ParseDuration(g.Timeout)
	if err != nil || duration <= 0 {
		return fail("gatekeeper %q timeout must be a positive duration", g.Name)
	}
	return nil
}

func DecodeGatekeeper(block *golden.HclBlock, ctx *hcl.EvalContext) (GatekeeperConfig, error) {
	if len(block.Labels) != 1 {
		return GatekeeperConfig{}, fail("worker gatekeeper block must have exactly one name")
	}
	name := block.Labels[0]
	if err := validateSafeLabel(name, "gatekeeper", false); err != nil {
		return GatekeeperConfig{}, err
	}
	scope := "gatekeeper " + name
	if err := validateNestedBlockSchema(block, scope, gatekeeperAllowedAttrs, nil); err != nil {
		return GatekeeperConfig{}, err
	}

	gatekeeper := GatekeeperBlock{Name: name}
	var err error
	if gatekeeper.Command, err = requiredKnownOrderedStringList(block, ctx, "command", scope, true); err != nil {
		return GatekeeperConfig{}, err
	}
	if wd, set, err := optionalKnownString(block, ctx, "working_dir", scope); err != nil {
		return GatekeeperConfig{}, err
	} else if set {
		gatekeeper.WorkingDir = wd
	}
	if timeout, set, err := optionalKnownString(block, ctx, "timeout", scope); err != nil {
		return GatekeeperConfig{}, err
	} else if set {
		if strings.TrimSpace(timeout) == "" {
			return GatekeeperConfig{}, fail("gatekeeper %q timeout must not be empty", name)
		}
		gatekeeper.Timeout = timeout
	}
	if exitCode, err := optionalWholeInt(block, ctx, "expect_exit_code", 0, scope); err != nil {
		return GatekeeperConfig{}, err
	} else {
		gatekeeper.ExpectExitCode = exitCode
	}
	if clean, err := optionalBool(block, ctx, "require_clean_worktree", true, scope); err != nil {
		return GatekeeperConfig{}, err
	} else {
		gatekeeper.RequireCleanTree = &clean
	}

	config := gatekeeper.Config()
	if err := validateGatekeeper(config); err != nil {
		return GatekeeperConfig{}, err
	}
	return config, nil
}
