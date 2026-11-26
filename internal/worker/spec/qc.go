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

const DefaultMaxQCRounds = 3

var (
	qcAllowedAttrs = map[string]struct{}{
		"criteria":          {},
		"sandbox":           {},
		"harness":           {},
		"model_provider":    {},
		"model":             {},
		"profile":           {},
		"reasoning_effort":  {},
		"permission":        {},
		"max_qc_rounds":     {},
		"tool_ids":          {},
		"tool_call_quota":   {},
		"allowed_tools":     {},
		"disallowed_tools":  {},
		"skill_directories": {},
		"skills":            {},
		"disabled_skills":   {},
	}
	qcAllowedNested = map[string]struct{}{
		"retry": {},
	}
)

type QCConfig struct {
	Criteria         cty.Value
	Sandbox          cty.Value
	Harness          cty.Value
	ModelProvider    cty.Value
	ModelProviderSet bool
	Model            string
	Profile          string
	ReasoningEffort  string
	Permission       string
	MaxQCRounds      int
	ToolIDs          []string
	ToolCallQuota    map[string]int
	AllowedTools     []string
	DisallowedTools  []string
	SkillDirectories []string
	Skills           []string
	DisabledSkills   []string
	Retry            provider.RetryOverride
}

func (q QCConfig) Clone() QCConfig {
	cloned := q
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

func decodeQC(block *golden.HclBlock, ctx *hcl.EvalContext) (QCConfig, error) {
	if len(block.Labels) != 0 {
		return QCConfig{}, fail("worker qc block does not accept labels")
	}
	if err := validateNestedBlockSchema(block, "qc", qcAllowedAttrs, qcAllowedNested); err != nil {
		return QCConfig{}, err
	}

	criteriaAttr, ok := block.Attributes()["criteria"]
	if !ok {
		return QCConfig{}, fail("qc criteria is required")
	}
	criteria, diags := criteriaAttr.Expr.Value(ctx)
	if diags.HasErrors() {
		return QCConfig{}, diags
	}
	unmarkedCriteria, _ := criteria.UnmarkDeep()
	if !unmarkedCriteria.IsWhollyKnown() || unmarkedCriteria.IsNull() ||
		(!unmarkedCriteria.Type().IsMapType() && !unmarkedCriteria.Type().IsObjectType()) ||
		unmarkedCriteria.LengthInt() == 0 {
		return QCConfig{}, fail("qc criteria must be a known non-empty map(string)")
	}
	for iterator := unmarkedCriteria.ElementIterator(); iterator.Next(); {
		key, value := iterator.Element()
		if value.IsNull() || !value.Type().Equals(cty.String) ||
			strings.TrimSpace(value.AsString()) == "" || strings.TrimSpace(key.AsString()) == "" {
			return QCConfig{}, fail("qc criteria keys and values must be non-empty strings")
		}
	}

	maxRounds, err := optionalWholeInt(block, ctx, "max_qc_rounds", DefaultMaxQCRounds, "qc")
	if err != nil {
		return QCConfig{}, err
	}
	if maxRounds <= 0 {
		return QCConfig{}, fail("qc max_qc_rounds must be positive")
	}

	result := QCConfig{
		Criteria:      criteria,
		Sandbox:       cty.NullVal(cty.EmptyObject),
		Harness:       cty.NullVal(cty.EmptyObject),
		ModelProvider: cty.NullVal(cty.EmptyObject),
		MaxQCRounds:   maxRounds,
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
	for _, nested := range block.NestedBlocks() {
		if nested.Type == "retry" {
			retryBlocks = append(retryBlocks, nested)
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
	return result, nil
}
