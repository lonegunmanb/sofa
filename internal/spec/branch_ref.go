package spec

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/zclconf/go-cty/cty"
)

// BranchRef retains both the evaluated branch value and its source expression.
type BranchRef struct {
	Value      cty.Value
	Expression string
}

func NewBranchRef(value cty.Value, expression string) (BranchRef, error) {
	if value == cty.NilVal {
		return BranchRef{}, fmt.Errorf("branch must be a string")
	}
	unmarked, _ := value.UnmarkDeep()
	if unmarked.IsNull() {
		return BranchRef{}, fmt.Errorf("branch must not be null")
	}
	if !unmarked.Type().Equals(cty.String) {
		return BranchRef{}, fmt.Errorf("branch must be a string, got %s", unmarked.Type().FriendlyName())
	}
	if unmarked.IsWhollyKnown() {
		if err := ValidateBranchName(unmarked.AsString()); err != nil {
			return BranchRef{}, err
		}
	}
	return BranchRef{
		Value:      value,
		Expression: expression,
	}, nil
}

// ValidateBranchName applies the frozen Stage 1 branch-name restrictions.
func ValidateBranchName(branch string) error {
	switch {
	case branch == "":
		return fmt.Errorf("branch name must not be empty")
	case strings.HasPrefix(branch, "-"):
		return fmt.Errorf("branch name %q must not start with '-'", branch)
	case strings.HasPrefix(branch, "/"), strings.HasSuffix(branch, "/"):
		return fmt.Errorf("branch name %q must not start or end with '/'", branch)
	case strings.HasPrefix(branch, "."), strings.HasSuffix(branch, "."):
		return fmt.Errorf("branch name %q must not start or end with '.'", branch)
	case strings.Contains(branch, ".."):
		return fmt.Errorf("branch name %q must not contain '..'", branch)
	case strings.Contains(branch, "@{"):
		return fmt.Errorf("branch name %q must not contain '@{'", branch)
	case strings.ContainsAny(branch, "~^:?*[\\"):
		return fmt.Errorf("branch name %q contains a prohibited character", branch)
	case strings.HasSuffix(branch, ".lock"):
		return fmt.Errorf("branch name %q must not end with '.lock'", branch)
	case strings.IndexFunc(branch, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}) >= 0:
		return fmt.Errorf("branch name %q must not contain whitespace or control characters", branch)
	}
	return nil
}

// IsKnown reports whether the branch can be validated as a literal value.
func (b BranchRef) IsKnown() bool {
	return b.Value != cty.NilVal && b.Value.IsWhollyKnown()
}
