package source

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode"

	"github.com/hashicorp/hcl/v2"
	"github.com/lonegunmanb/golden"
	specpkg "github.com/lonegunmanb/sofa/internal/spec"
	"github.com/zclconf/go-cty/cty"
)

var environmentVariablePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var scpRepositoryPattern = regexp.MustCompile(`^[^@\s]+@[^:\s]+:[^\s]+$`)

type SourceConfig struct {
	URL       string
	Branch    string
	TokenRef  *string
	SSHKeyRef *string
}

func (c SourceConfig) Validate() error {
	if err := ValidateRepositoryURL(c.URL); err != nil {
		return err
	}
	if err := specpkg.ValidateBranchName(c.Branch); err != nil {
		return err
	}
	if c.TokenRef != nil {
		if err := ValidateEnvironmentVariableName(*c.TokenRef); err != nil {
			return fmt.Errorf("invalid token_ref: %w", err)
		}
	}
	if c.SSHKeyRef != nil {
		if err := ValidateEnvironmentVariableName(*c.SSHKeyRef); err != nil {
			return fmt.Errorf("invalid ssh_key_ref: %w", err)
		}
	}
	if c.TokenRef != nil && c.SSHKeyRef != nil {
		return fmt.Errorf("token_ref and ssh_key_ref are mutually exclusive")
	}
	return nil
}

func ValidateRepositoryURL(repositoryURL string) error {
	if repositoryURL == "" {
		return fmt.Errorf("repository url is required")
	}
	if strings.IndexFunc(repositoryURL, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}) >= 0 {
		return fmt.Errorf("repository url must not contain whitespace or control characters")
	}

	if strings.Contains(repositoryURL, "://") {
		parsed, err := url.Parse(repositoryURL)
		if err != nil {
			return fmt.Errorf("invalid repository url: %w", err)
		}
		switch parsed.Scheme {
		case "http", "https":
			if parsed.Host == "" {
				return fmt.Errorf("repository url %q must include a host", repositoryURL)
			}
			if parsed.User != nil {
				if _, hasPassword := parsed.User.Password(); hasPassword {
					return fmt.Errorf("repository url must not embed a password; use token_ref instead")
				}
			}
		case "ssh":
			if parsed.Host == "" {
				return fmt.Errorf("repository ssh url %q must include a host", repositoryURL)
			}
		case "file":
			if parsed.Path == "" {
				return fmt.Errorf("repository file url %q must include a path", repositoryURL)
			}
		default:
			return fmt.Errorf("unsupported repository url scheme %q", parsed.Scheme)
		}
		return nil
	}

	if scpRepositoryPattern.MatchString(repositoryURL) {
		return nil
	}
	if strings.HasPrefix(repositoryURL, "-") {
		return fmt.Errorf("local repository path must not start with '-'")
	}
	return nil
}

func ValidateEnvironmentVariableName(name string) error {
	if !environmentVariablePattern.MatchString(name) {
		return fmt.Errorf("environment variable name %q is invalid", name)
	}
	return nil
}

type SourceBlock struct {
	*golden.BaseBlock
	URL       string  `hcl:"url"`
	Branch    string  `hcl:"branch"`
	TokenRef  *string `hcl:"token_ref,optional"`
	SSHKeyRef *string `hcl:"ssh_key_ref,optional"`
	planned   cty.Value
}

func (s *SourceBlock) Type() string            { return "" }
func (s *SourceBlock) BlockType() string       { return "source" }
func (s *SourceBlock) AddressLength() int      { return 2 }
func (s *SourceBlock) CanExecutePrePlan() bool { return false }

func (s *SourceBlock) BaseDecode(block *golden.HclBlock, ctx *hcl.EvalContext) error {
	for _, attributeName := range []string{"url", "branch"} {
		attribute, exists := block.Body.Attributes[attributeName]
		if !exists {
			continue
		}
		value, diags := attribute.Expr.Value(ctx)
		if diags.HasErrors() {
			return diags
		}
		if !value.IsKnown() || value.IsNull() || !value.Type().Equals(cty.String) {
			return fmt.Errorf("source %q %s must be a known non-null string", s.Name(), attributeName)
		}
	}
	for _, attributeName := range []string{"token_ref", "ssh_key_ref"} {
		attribute, exists := block.Body.Attributes[attributeName]
		if !exists {
			continue
		}
		value, diags := attribute.Expr.Value(ctx)
		if diags.HasErrors() {
			return diags
		}
		if value.IsNull() {
			continue
		}
		if !value.IsKnown() || !value.Type().Equals(cty.String) {
			return fmt.Errorf("source %q %s must be a known string or null", s.Name(), attributeName)
		}
	}
	return nil
}

func (s *SourceBlock) ExecuteDuringPlan() error {
	config := s.SourceConfig()
	if err := config.Validate(); err != nil {
		return fmt.Errorf("source %q: %w", s.Name(), err)
	}
	s.planned = cty.ObjectVal(map[string]cty.Value{
		"address":    cty.StringVal(s.Address()),
		"kind":       cty.StringVal("source"),
		"name":       cty.StringVal(s.Name()),
		"url":        cty.StringVal(s.URL),
		"branch":     cty.StringVal(s.Branch),
		"commit_sha": cty.UnknownVal(cty.String),
	})
	return nil
}

func (s *SourceBlock) SourceConfig() SourceConfig {
	return SourceConfig{
		URL:       s.URL,
		Branch:    s.Branch,
		TokenRef:  s.TokenRef,
		SSHKeyRef: s.SSHKeyRef,
	}
}

func (s *SourceBlock) Value() cty.Value {
	if s.planned == cty.NilVal {
		return cty.DynamicVal
	}
	return s.planned
}

var _ golden.PlanBlock = (*SourceBlock)(nil)
var _ golden.SingleValueBlock = (*SourceBlock)(nil)
var _ golden.BaseDecode = (*SourceBlock)(nil)

type PreserveConfig struct {
	URL        string
	Branch     specpkg.BranchRef
	BranchExpr string
}

type PreserveBlock struct {
	*golden.BaseBlock
	URL     string
	Branch  specpkg.BranchRef
	planned cty.Value
}

func (p *PreserveBlock) Type() string            { return "" }
func (p *PreserveBlock) BlockType() string       { return "preserve" }
func (p *PreserveBlock) AddressLength() int      { return 2 }
func (p *PreserveBlock) CanExecutePrePlan() bool { return false }

func (p *PreserveBlock) Decode(block *golden.HclBlock, ctx *hcl.EvalContext) error {
	p.URL = ""
	p.Branch = specpkg.BranchRef{}
	p.planned = cty.NilVal

	for name := range block.Body.Attributes {
		if name != "url" && name != "branch" && !golden.MetaAttributeNames.Contains(name) {
			return fmt.Errorf("preserve %q contains unknown attribute %q", p.Name(), name)
		}
	}
	for _, nested := range block.NestedBlocks() {
		if !golden.MetaNestedBlockNames.Contains(nested.Type) {
			return fmt.Errorf("preserve %q contains unsupported nested block %q", p.Name(), nested.Type)
		}
	}

	urlAttribute, hasURL := block.Body.Attributes["url"]
	if !hasURL {
		return fmt.Errorf("preserve %q MUST declare url or be bound to a single root source", p.Name())
	}
	urlValue, diags := urlAttribute.Expr.Value(ctx)
	if diags.HasErrors() {
		return diags
	}
	urlValue, _ = urlValue.UnmarkDeep()
	if !urlValue.IsKnown() || urlValue.IsNull() || !urlValue.Type().Equals(cty.String) {
		return fmt.Errorf("preserve %q url must be a known non-null string", p.Name())
	}
	p.URL = urlValue.AsString()
	if err := ValidateRepositoryURL(p.URL); err != nil {
		return fmt.Errorf("preserve %q url: %w", p.Name(), err)
	}

	branchAttribute, hasBranch := block.Attributes()["branch"]
	if !hasBranch {
		return fmt.Errorf("preserve %q MUST declare branch", p.Name())
	}
	branchValue, diags := branchAttribute.Expr.Value(ctx)
	if diags.HasErrors() {
		return diags
	}
	branch, err := specpkg.NewBranchRef(branchValue, branchAttribute.ExprString())
	if err != nil {
		return fmt.Errorf("preserve %q branch: %w", p.Name(), err)
	}
	p.Branch = branch
	return nil
}

func (p *PreserveBlock) ExecuteDuringPlan() error {
	if p.URL == "" {
		return fmt.Errorf("preserve %q MUST declare url", p.Name())
	}
	if p.Branch.Value == cty.NilVal {
		return fmt.Errorf("preserve %q MUST declare branch", p.Name())
	}
	p.planned = cty.ObjectVal(map[string]cty.Value{
		"address":     cty.StringVal(p.Address()),
		"kind":        cty.StringVal("preserve"),
		"name":        cty.StringVal(p.Name()),
		"url":         cty.StringVal(p.URL),
		"branch":      p.Branch.Value,
		"branch_expr": cty.StringVal(p.Branch.Expression),
	})
	return nil
}

func (p *PreserveBlock) PreserveConfig() PreserveConfig {
	return PreserveConfig{
		URL:        p.URL,
		Branch:     p.Branch,
		BranchExpr: p.Branch.Expression,
	}
}

func (p *PreserveBlock) Value() cty.Value {
	if p.planned == cty.NilVal {
		return cty.DynamicVal
	}
	return p.planned
}

var _ golden.CustomDecode = (*PreserveBlock)(nil)
var _ golden.PlanBlock = (*PreserveBlock)(nil)
var _ golden.SingleValueBlock = (*PreserveBlock)(nil)
