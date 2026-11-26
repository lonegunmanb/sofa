package spec

import (
	"slices"

	"github.com/zclconf/go-cty/cty"
)

// Output is the stable, inspectable representation of a native golden output.
// It contains no block or decoding behavior.
type Output struct {
	Name        string
	Value       cty.Value
	Description *string
	Sensitive   bool
	Expression  string
	SourceURLs  []string
}

// Clone returns a deep copy of Output.
func (o Output) Clone() Output {
	cloned := o
	if o.Description != nil {
		desc := *o.Description
		cloned.Description = &desc
	}
	cloned.SourceURLs = slices.Clone(o.SourceURLs)
	return cloned
}
