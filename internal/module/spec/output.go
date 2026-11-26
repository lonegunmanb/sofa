package spec

import "github.com/zclconf/go-cty/cty"

// Output is the stable, inspectable representation of a native golden output.
// It contains no block or decoding behavior.
type Output struct {
	Name        string
	Value       cty.Value
	Description *string
	Sensitive   bool
	Expression  string
}
