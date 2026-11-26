package spec

import (
	"maps"
	"slices"
	"time"

	corespec "github.com/lonegunmanb/sofa/internal/spec"
	"github.com/zclconf/go-cty/cty"
)

// Plan captures the offline planning snapshot for a root or child module.
type Plan struct {
	Directory           string
	BundlePath          string
	Outputs             map[string]Output
	Modules             map[string]ModulePlan
	DeferredValidations map[string][]corespec.Condition
	ChildConfig         any
}

// Clone returns a deep copy of Plan.
func (p Plan) Clone() Plan {
	cloned := Plan{
		Directory:   p.Directory,
		BundlePath:  p.BundlePath,
		ChildConfig: p.ChildConfig,
	}
	if p.Outputs != nil {
		cloned.Outputs = make(map[string]Output, len(p.Outputs))
		for k, v := range p.Outputs {
			cloned.Outputs[k] = v.Clone()
		}
	}
	if p.Modules != nil {
		cloned.Modules = make(map[string]ModulePlan, len(p.Modules))
		for k, v := range p.Modules {
			cloned.Modules[k] = v.Clone()
		}
	}
	if p.DeferredValidations != nil {
		cloned.DeferredValidations = make(map[string][]corespec.Condition, len(p.DeferredValidations))
		for k, conds := range p.DeferredValidations {
			cloned.DeferredValidations[k] = slices.Clone(conds)
		}
	}
	return cloned
}

// ModulePlan captures a planned module call and its nested child Plan.
type ModulePlan struct {
	Plan
	Address          string
	Name             string
	CanonicalAddress string
	Source           string
	Parallelism      *int
	Timeout          *time.Duration
	TimeoutRaw       string
	Inputs           map[string]cty.Value
	InputExprs       map[string]string
}

// Clone returns a deep copy of ModulePlan.
func (m ModulePlan) Clone() ModulePlan {
	cloned := ModulePlan{
		Plan:             m.Plan.Clone(),
		Address:          m.Address,
		Name:             m.Name,
		CanonicalAddress: m.CanonicalAddress,
		Source:           m.Source,
		TimeoutRaw:       m.TimeoutRaw,
		Inputs:           maps.Clone(m.Inputs),
		InputExprs:       maps.Clone(m.InputExprs),
	}
	if m.Parallelism != nil {
		p := *m.Parallelism
		cloned.Parallelism = &p
	}
	if m.Timeout != nil {
		d := *m.Timeout
		cloned.Timeout = &d
	}
	return cloned
}
