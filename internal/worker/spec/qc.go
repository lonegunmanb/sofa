package spec

import "github.com/zclconf/go-cty/cty"

type QCConfig struct {
	Criteria         cty.Value
	Harness          cty.Value
	ModelProvider    cty.Value
	Model            string
	ReasoningEffort  string
	MaxQCRounds      int
	ToolIDs          []string
	SkillDirectories []string
	Skills           []string
	DisabledSkills   []string
}

type QCBlock struct {
	Criteria         cty.Value
	Harness          cty.Value
	ModelProvider    cty.Value
	Model            string
	ReasoningEffort  string
	MaxQCRounds      int
	ToolIDs          []string
	SkillDirectories []string
	Skills           []string
	DisabledSkills   []string
}
