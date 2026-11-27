package spec

import (
	"slices"
	"strings"
	"time"

	"github.com/hashicorp/hcl/v2"
	"github.com/lonegunmanb/golden"
	"github.com/zclconf/go-cty/cty"
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

// ValidateGatekeeper validates a decoded GatekeeperConfig.
func ValidateGatekeeper(g GatekeeperConfig) error {
	return validateGatekeeper(g)
}

func validateGatekeeper(g GatekeeperConfig) error {
	return validateGatekeeperWithOptions(g, false)
}

func validateGatekeeperWithOptions(g GatekeeperConfig, allowUnknownCommand bool) error {
	if err := validateSafeLabel(g.Name, "gatekeeper", false); err != nil {
		return err
	}
	if !allowUnknownCommand {
		if len(g.Command) == 0 || strings.TrimSpace(g.Command[0]) == "" {
			return fail("gatekeeper %q command must be a non-empty list", g.Name)
		}
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
	cfg, _, err := decodeGatekeeper(block, ctx, false)
	return cfg, err
}

func decodeGatekeeper(block *golden.HclBlock, ctx *hcl.EvalContext, allowUnknownCommand bool) (GatekeeperConfig, bool, error) {
	if len(block.Labels) != 1 {
		return GatekeeperConfig{}, false, fail("gatekeeper block must have exactly one name")
	}
	name := block.Labels[0]
	if err := validateSafeLabel(name, "gatekeeper", false); err != nil {
		return GatekeeperConfig{}, false, err
	}
	scope := "gatekeeper " + name
	if err := validateNestedBlockSchema(block, scope, gatekeeperAllowedAttrs, nil); err != nil {
		return GatekeeperConfig{}, false, err
	}

	gatekeeper := GatekeeperBlock{Name: name}
	var commandHasUnknown bool
	var err error
	if allowUnknownCommand {
		gatekeeper.Command, commandHasUnknown, err = decodeGatekeeperCommandAllowUnknown(block, ctx, scope)
		if err != nil {
			return GatekeeperConfig{}, false, err
		}
	} else {
		if gatekeeper.Command, err = requiredKnownOrderedStringList(block, ctx, "command", scope, true); err != nil {
			return GatekeeperConfig{}, false, err
		}
	}
	if wd, set, err := optionalKnownString(block, ctx, "working_dir", scope); err != nil {
		return GatekeeperConfig{}, false, err
	} else if set {
		gatekeeper.WorkingDir = wd
	}
	if timeout, set, err := optionalKnownString(block, ctx, "timeout", scope); err != nil {
		return GatekeeperConfig{}, false, err
	} else if set {
		if strings.TrimSpace(timeout) == "" {
			return GatekeeperConfig{}, false, fail("gatekeeper %q timeout must not be empty", name)
		}
		gatekeeper.Timeout = timeout
	}
	if exitCode, err := optionalWholeInt(block, ctx, "expect_exit_code", 0, scope); err != nil {
		return GatekeeperConfig{}, false, err
	} else {
		gatekeeper.ExpectExitCode = exitCode
	}
	if clean, err := optionalBool(block, ctx, "require_clean_worktree", true, scope); err != nil {
		return GatekeeperConfig{}, false, err
	} else {
		gatekeeper.RequireCleanTree = &clean
	}

	config := gatekeeper.Config()
	if err := validateGatekeeperWithOptions(config, commandHasUnknown); err != nil {
		return GatekeeperConfig{}, false, err
	}
	return config, commandHasUnknown, nil
}

func decodeGatekeeperCommandAllowUnknown(block *golden.HclBlock, ctx *hcl.EvalContext, scope string) ([]string, bool, error) {
	attr, ok := block.Attributes()["command"]
	if !ok {
		return nil, false, fail("%s command is required", scope)
	}
	value, diags := attr.Expr.Value(ctx)
	if diags.HasErrors() {
		return nil, false, diags
	}
	unmarked, _ := value.UnmarkDeep()
	if unmarked.IsNull() {
		return nil, false, fail("%s command must not be null", scope)
	}
	vt := unmarked.Type()
	if !unmarked.IsKnown() {
		if !vt.IsListType() && !vt.IsTupleType() && vt != cty.DynamicPseudoType {
			return nil, false, fail("%s command must be a list of strings", scope)
		}
		return nil, true, nil
	}
	if !vt.IsListType() && !vt.IsTupleType() {
		return nil, false, fail("%s command must be a list of strings", scope)
	}
	if unmarked.LengthInt() == 0 {
		return nil, false, fail("%s command must be a non-empty list", scope)
	}
	result := make([]string, 0, unmarked.LengthInt())
	hasUnknown := false
	for it := unmarked.ElementIterator(); it.Next(); {
		_, item := it.Element()
		if !item.IsKnown() {
			if !item.Type().Equals(cty.String) && item.Type() != cty.DynamicPseudoType {
				return nil, false, fail("%s command elements must be non-null strings", scope)
			}
			hasUnknown = true
			continue
		}
		if item.IsNull() || !item.Type().Equals(cty.String) {
			return nil, false, fail("%s command elements must be non-null strings", scope)
		}
		str := item.AsString()
		if strings.TrimSpace(str) == "" {
			return nil, false, fail("%s command must not contain empty strings", scope)
		}
		result = append(result, str)
	}
	if hasUnknown {
		return nil, true, nil
	}
	return result, false, nil
}
