package spec

import (
	"maps"
	"slices"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/lonegunmanb/golden"
	"github.com/lonegunmanb/sofa/internal/provider"
	"github.com/zclconf/go-cty/cty"
)

const (
	DefaultMaxQCRounds          = 3
	DefaultMaxGatekeeperReworks = 3
)

var (
	qcAllowedAttrs = map[string]struct{}{
		"criteria":               {},
		"sandbox":                {},
		"harness":                {},
		"model_provider":         {},
		"model":                  {},
		"profile":                {},
		"reasoning_effort":       {},
		"permission":             {},
		"max_qc_rounds":          {},
		"max_gatekeeper_reworks": {},
		"tool_ids":               {},
		"tool_call_quota":        {},
		"allowed_tools":          {},
		"disallowed_tools":       {},
		"skill_directories":      {},
		"skills":                 {},
		"disabled_skills":        {},
	}
	qcAllowedNested = map[string]struct{}{
		"retry":      {},
		"gatekeeper": {},
	}
)

type QCConfig struct {
	Criteria                cty.Value
	Sandbox                 cty.Value
	Harness                 cty.Value
	ModelProvider           cty.Value
	ModelProviderSet        bool
	Model                   string
	Profile                 string
	ReasoningEffort         string
	Permission              string
	MaxQCRounds             int
	MaxGatekeeperReworks    int
	MaxGatekeeperReworksSet bool
	Gatekeepers             []GatekeeperConfig
	ToolIDs                 []string
	ToolCallQuota           map[string]int
	AllowedTools            []string
	DisallowedTools         []string
	SkillDirectories        []string
	Skills                  []string
	DisabledSkills          []string
	Retry                   provider.RetryOverride
}

func (q QCConfig) Clone() QCConfig {
	cloned := q
	if q.Gatekeepers != nil {
		cloned.Gatekeepers = make([]GatekeeperConfig, len(q.Gatekeepers))
		for i, g := range q.Gatekeepers {
			cloned.Gatekeepers[i] = g.Clone()
		}
	}
	cloned.ToolIDs = slices.Clone(q.ToolIDs)
	cloned.ToolCallQuota = maps.Clone(q.ToolCallQuota)
	cloned.AllowedTools = slices.Clone(q.AllowedTools)
	cloned.DisallowedTools = slices.Clone(q.DisallowedTools)
	cloned.SkillDirectories = slices.Clone(q.SkillDirectories)
	cloned.Skills = slices.Clone(q.Skills)
	cloned.DisabledSkills = slices.Clone(q.DisabledSkills)
	cloned.Retry = cloneRetryOverride(q.Retry)
	return cloned
}

func decodeQC(block *golden.HclBlock, ctx *hcl.EvalContext, deferred *bool) (QCConfig, error) {
	if len(block.Labels) != 0 {
		return QCConfig{}, fail("worker qc block does not accept labels")
	}
	if err := validateNestedBlockSchema(block, "qc", qcAllowedAttrs, qcAllowedNested); err != nil {
		return QCConfig{}, err
	}

	criteria := cty.MapValEmpty(cty.String)
	hasCriteria := false
	if criteriaAttr, ok := block.Attributes()["criteria"]; ok {
		val, diags := criteriaAttr.Expr.Value(ctx)
		if diags.HasErrors() {
			return QCConfig{}, diags
		}
		unmarkedCriteria, _ := val.UnmarkDeep()
		if !unmarkedCriteria.IsNull() {
			vt := unmarkedCriteria.Type()
			if !unmarkedCriteria.IsKnown() {
				if deferred == nil || (!vt.IsMapType() && !vt.IsObjectType() && vt != cty.DynamicPseudoType) {
					return QCConfig{}, fail("qc criteria must be a known non-empty map(string)")
				}
				*deferred = true
			} else {
				if (!vt.IsMapType() && !vt.IsObjectType()) || unmarkedCriteria.LengthInt() == 0 {
					return QCConfig{}, fail("qc criteria must be a known non-empty map(string)")
				}
				for iterator := unmarkedCriteria.ElementIterator(); iterator.Next(); {
					key, value := iterator.Element()
					if strings.TrimSpace(key.AsString()) == "" {
						return QCConfig{}, fail("qc criteria keys and values must be non-empty strings")
					}
					if !value.IsKnown() {
						if deferred == nil || (!value.Type().Equals(cty.String) && value.Type() != cty.DynamicPseudoType) {
							return QCConfig{}, fail("qc criteria must be a known non-empty map(string)")
						}
						*deferred = true
						continue
					}
					if value.IsNull() || !value.Type().Equals(cty.String) || strings.TrimSpace(value.AsString()) == "" {
						return QCConfig{}, fail("qc criteria keys and values must be non-empty strings")
					}
				}
			}
			criteria = val
			hasCriteria = true
		}
	}

	maxRounds, err := optionalWholeInt(block, ctx, "max_qc_rounds", DefaultMaxQCRounds, "qc")
	if err != nil {
		return QCConfig{}, err
	}
	if maxRounds <= 0 {
		return QCConfig{}, fail("qc max_qc_rounds must be positive")
	}

	maxGKReworks := DefaultMaxGatekeeperReworks
	maxGKReworksPtr, maxGKReworksSet, err := optionalWholeIntPtr(block, ctx, "max_gatekeeper_reworks", "qc")
	if err != nil {
		return QCConfig{}, err
	}
	if maxGKReworksSet {
		if *maxGKReworksPtr < 0 {
			return QCConfig{}, fail("qc max_gatekeeper_reworks must be non-negative")
		}
		maxGKReworks = *maxGKReworksPtr
	}

	result := QCConfig{
		Criteria:                criteria,
		Sandbox:                 cty.NullVal(cty.EmptyObject),
		Harness:                 cty.NullVal(cty.EmptyObject),
		ModelProvider:           cty.NullVal(cty.EmptyObject),
		MaxQCRounds:             maxRounds,
		MaxGatekeeperReworks:    maxGKReworks,
		MaxGatekeeperReworksSet: maxGKReworksSet,
	}

	if model, set, err := optionalKnownString(block, ctx, "model", "qc"); err != nil {
		return QCConfig{}, err
	} else if set {
		if strings.TrimSpace(model) == "" {
			return QCConfig{}, fail("qc model must not be empty")
		}
		result.Model = model
	}
	if profile, set, err := optionalKnownString(block, ctx, "profile", "qc"); err != nil {
		return QCConfig{}, err
	} else if set {
		if strings.TrimSpace(profile) == "" {
			return QCConfig{}, fail("qc profile must not be empty")
		}
		result.Profile = profile
	}
	if effort, set, err := optionalKnownString(block, ctx, "reasoning_effort", "qc"); err != nil {
		return QCConfig{}, err
	} else if set {
		if strings.TrimSpace(effort) == "" {
			return QCConfig{}, fail("qc reasoning_effort must not be empty")
		}
		result.ReasoningEffort = effort
	}
	if perm, set, err := optionalKnownString(block, ctx, "permission", "qc"); err != nil {
		return QCConfig{}, err
	} else if set {
		if perm != "approve_all" {
			return QCConfig{}, fail("qc permission must be approve_all")
		}
		result.Permission = perm
	}

	if result.ToolIDs, err = optionalKnownStringList(block, ctx, "tool_ids", "qc", true); err != nil {
		return QCConfig{}, err
	}
	if result.ToolCallQuota, err = optionalIntMap(block, ctx, "tool_call_quota", "qc"); err != nil {
		return QCConfig{}, err
	}
	if err := validateToolCallQuota(result.ToolCallQuota, result.ToolIDs, "qc"); err != nil {
		return QCConfig{}, err
	}
	if result.AllowedTools, err = optionalKnownStringList(block, ctx, "allowed_tools", "qc", true); err != nil {
		return QCConfig{}, err
	}
	if result.DisallowedTools, err = optionalKnownStringList(block, ctx, "disallowed_tools", "qc", true); err != nil {
		return QCConfig{}, err
	}
	if result.SkillDirectories, err = optionalKnownStringList(block, ctx, "skill_directories", "qc", true); err != nil {
		return QCConfig{}, err
	}
	if result.Skills, err = optionalKnownStringList(block, ctx, "skills", "qc", true); err != nil {
		return QCConfig{}, err
	}
	if result.DisabledSkills, err = optionalKnownStringList(block, ctx, "disabled_skills", "qc", true); err != nil {
		return QCConfig{}, err
	}

	if result.Sandbox, err = optionalBlockReference(block, ctx, "sandbox", "sandbox", "qc"); err != nil {
		return QCConfig{}, err
	}
	if result.Harness, err = optionalBlockReference(block, ctx, "harness", "harness", "qc"); err != nil {
		return QCConfig{}, err
	}
	if attr, ok := block.Attributes()["model_provider"]; ok {
		result.ModelProviderSet = true
		val, diags := attr.Expr.Value(ctx)
		if diags.HasErrors() {
			return QCConfig{}, diags
		}
		unmarked, _ := val.UnmarkDeep()
		if !unmarked.IsNull() {
			if err := validateBlockReference(val, "model_provider", "qc model_provider"); err != nil {
				return QCConfig{}, err
			}
			result.ModelProvider = val
		}
	}

	var retryBlocks []*golden.HclBlock
	gatekeeperNames := map[string]struct{}{}
	for _, nested := range block.NestedBlocks() {
		switch nested.Type {
		case "retry":
			retryBlocks = append(retryBlocks, nested)
		case "gatekeeper":
			gk, hasUnknown, err := decodeGatekeeper(nested, ctx, deferred != nil)
			if err != nil {
				return QCConfig{}, err
			}
			if hasUnknown && deferred != nil {
				*deferred = true
			}
			if _, exists := gatekeeperNames[gk.Name]; exists {
				return QCConfig{}, fail("qc gatekeeper %q is declared more than once", gk.Name)
			}
			gatekeeperNames[gk.Name] = struct{}{}
			result.Gatekeepers = append(result.Gatekeepers, gk)
		}
	}
	if len(retryBlocks) > 1 {
		return QCConfig{}, fail("qc supports at most one retry block")
	}
	if len(retryBlocks) == 1 {
		result.Retry, err = decodeRetry(retryBlocks[0], ctx, "qc retry")
		if err != nil {
			return QCConfig{}, err
		}
	}
	if !hasCriteria && len(result.Gatekeepers) == 0 {
		return QCConfig{}, fail("qc block must declare at least one gatekeeper or non-empty criteria")
	}
	return result, nil
}
