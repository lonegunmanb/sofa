package gotool_test

import (
	"testing"

	"github.com/lonegunmanb/sofa/internal/tool/gotool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zclconf/go-cty/cty"
)

func TestAnalyzeValidAndNestedStruct(t *testing.T) {
	t.Parallel()

	source := `
import "context"

type Filter struct {
	Tag   string ` + "`json:\"tag\"`" + `
	Limit *int   ` + "`json:\"limit,omitempty\"`" + `
}

type Input struct {
	Query   string            ` + "`json:\"query\"`" + `
	Filter  *Filter           ` + "`json:\"filter\"`" + `
	Checks  map[string]string ` + "`json:\"checks\"`" + `
	Pair    [2]string         ` + "`json:\"pair\"`" + `
	Counts  []int64           ` + "`json:\"counts\"`" + `
	Ratio   float64           ` + "`json:\"ratio\"`" + `
	Enabled bool              ` + "`json:\"enabled\"`" + `
	Ignored string            ` + "`json:\"-\"`" + `
}

type Output struct {
	Summary string ` + "`json:\"summary\"`" + `
}

func Invoke(ctx context.Context, in Input) (ToolResponse[Output], error) {
	return ToolResponse[Output]{Accepted: true}, nil
}
`
	analysis, err := gotool.Analyze(source)
	require.NoError(t, err)

	assert.True(t, analysis.InputType.IsObjectType())
	assert.False(t, analysis.InputType.AttributeOptional("query"))
	assert.True(t, analysis.InputType.AttributeOptional("filter"))
	assert.False(t, analysis.InputType.HasAttribute("Ignored"))

	filterType := analysis.InputType.AttributeType("filter")
	assert.True(t, filterType.IsObjectType())
	assert.False(t, filterType.AttributeOptional("tag"))
	assert.True(t, filterType.AttributeOptional("limit"))
	assert.True(t, filterType.AttributeType("limit").Equals(cty.Number))

	assert.True(t, analysis.InputType.AttributeType("checks").Equals(cty.Map(cty.String)))
	assert.True(t, analysis.InputType.AttributeType("pair").Equals(cty.Tuple([]cty.Type{cty.String, cty.String})))
	assert.True(t, analysis.InputType.AttributeType("counts").Equals(cty.List(cty.Number)))
	assert.True(t, analysis.InputType.AttributeType("ratio").Equals(cty.Number))
	assert.True(t, analysis.InputType.AttributeType("enabled").Equals(cty.Bool))

	assert.True(t, analysis.OutputType.Equals(cty.Object(map[string]cty.Type{
		"summary": cty.String,
	})))
}

func TestAnalyzeWithDeclaredDependenciesOffline(t *testing.T) {
	t.Parallel()

	source := `
import (
	"context"
	"github.com/google/uuid"
)

type Input struct {
	Seed string ` + "`json:\"seed\"`" + `
}

type Output string

func Invoke(ctx context.Context, in Input) (ToolResponse[Output], error) {
	out := Output(uuid.NewString())
	return ToolResponse[Output]{Accepted: true, Output: &out}, nil
}
`
	analysis, err := gotool.AnalyzeWithDependencies(source, []string{
		"github.com/google/uuid@v1.6.0",
		"golang.org/x/text@v0.3.0",
	})
	require.NoError(t, err)
	assert.True(t, analysis.InputType.Equals(cty.Object(map[string]cty.Type{"seed": cty.String})))
	assert.True(t, analysis.OutputType.Equals(cty.String))
}

func TestAnalyzeSupportsAliasedContextAndQuotePaths(t *testing.T) {
	t.Parallel()

	source := `
import stdctx "context"

type Quote string

type Input struct {
	Primary Quote   ` + "`json:\"primary\"`" + `
	Many    []Quote ` + "`json:\"many\"`" + `
	Nested  struct {
		Sources []Quote ` + "`json:\"sources\"`" + `
	} ` + "`json:\"nested\"`" + `
}

type Output string

func Invoke(_ stdctx.Context, _ Input) (ToolResponse[Output], error) {
	return ToolResponse[Output]{Accepted: true}, nil
}
`
	analysis, err := gotool.Analyze(source)
	require.NoError(t, err)
	assert.ElementsMatch(t, [][]string{
		{"primary"},
		{"many", "[]"},
		{"nested", "sources", "[]"},
	}, analysis.QuotePaths)
}

func TestAnalyzeJSONTagEdgeCases(t *testing.T) {
	t.Parallel()

	source := `
import "context"

type Input struct {
	Dash    *string ` + "`json:\"-,omitempty\"`" + `
	Invalid string  ` + "`json:\"bad\\\\name\"`" + `
	Plain   int
}
type Output string
func Invoke(context.Context, Input) (ToolResponse[Output], error) { return ToolResponse[Output]{}, nil }
`
	analysis, err := gotool.Analyze(source)
	require.NoError(t, err)
	require.True(t, analysis.InputType.HasAttribute("-"))
	assert.True(t, analysis.InputType.AttributeOptional("-"))
	assert.True(t, analysis.InputType.HasAttribute("Invalid"))
	assert.False(t, analysis.InputType.HasAttribute(`bad\name`))
	assert.True(t, analysis.InputType.HasAttribute("Plain"))
}

func TestAnalyzeRejectsInvalidSources(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		source       string
		dependencies []string
		wantErr      string
	}{
		{
			name: "syntax error includes line number",
			source: `import "context"
type Input struct {
  Broken string
`,
			wantErr: "tool.go:3:",
		},
		{
			name:    "package clause rejected",
			source:  "package main\nfunc Invoke() {}",
			wantErr: "package clause is not allowed",
		},
		{
			name: "main function rejected",
			source: `import "context"
type Input struct{}
type Output string
func main() {}
func Invoke(context.Context, Input) (ToolResponse[Output], error) { return ToolResponse[Output]{}, nil }`,
			wantErr: "main function is not allowed",
		},
		{
			name: "undeclared non-stdlib import rejected",
			source: `import (
  "context"
  "github.com/google/uuid"
)
type Input struct{}
type Output string
func Invoke(context.Context, Input) (ToolResponse[Output], error) { return ToolResponse[Output]{}, nil }`,
			wantErr: "import must be from the Go standard library or declared dependencies",
		},
		{
			name: "missing Input rejected",
			source: `import "context"
type Output string
func Invoke(context.Context, Input) (ToolResponse[Output], error) { return ToolResponse[Output]{}, nil }`,
			wantErr: "type Input is required",
		},
		{
			name: "missing Output rejected",
			source: `import "context"
type Input struct{}
func Invoke(context.Context, Input) (ToolResponse[Output], error) { return ToolResponse[Output]{}, nil }`,
			wantErr: "type Output is required",
		},
		{
			name: "alias Input rejected",
			source: `import "context"
type Input = struct{}
type Output string
func Invoke(context.Context, Input) (ToolResponse[Output], error) { return ToolResponse[Output]{}, nil }`,
			wantErr: "Input must be a defined type",
		},
		{
			name: "non-struct Input rejected",
			source: `import "context"
type Input string
type Output string
func Invoke(context.Context, Input) (ToolResponse[Output], error) { return ToolResponse[Output]{}, nil }`,
			wantErr: "Input must be a struct",
		},
		{
			name: "non-struct local-named Input rejected",
			source: `import "context"
type Alias string
type Input Alias
type Output string
func Invoke(context.Context, Input) (ToolResponse[Output], error) { return ToolResponse[Output]{}, nil }`,
			wantErr: "Input must be a struct",
		},
		{
			name: "wrong Invoke signature",
			source: `import "context"
type Input struct{}
type Output string
func Invoke(Input) error { return nil }`,
			wantErr: "Invoke must have signature",
		},
		{
			name: "wrong Invoke response type argument",
			source: `import "context"
type Input struct{}
type Output struct{}
func Invoke(context.Context, Input) (ToolResponse[string], error) { return ToolResponse[string]{}, nil }`,
			wantErr: "Invoke must return ToolResponse[Output]",
		},
		{
			name: "chan field rejected",
			source: `import "context"
type Input struct { Jobs chan int }
type Output string
func Invoke(context.Context, Input) (ToolResponse[Output], error) { return ToolResponse[Output]{}, nil }`,
			wantErr: "unsupported Go type",
		},
		{
			name: "func field rejected",
			source: `import "context"
type Input struct { Fn func() }
type Output string
func Invoke(context.Context, Input) (ToolResponse[Output], error) { return ToolResponse[Output]{}, nil }`,
			wantErr: "unsupported Go type",
		},
		{
			name: "byte slice rejected",
			source: `import "context"
type Input struct { Raw []byte }
type Output string
func Invoke(context.Context, Input) (ToolResponse[Output], error) { return ToolResponse[Output]{}, nil }`,
			wantErr: "[]byte has JSON string semantics",
		},
		{
			name: "non-string map key rejected",
			source: `import "context"
type Input struct { Values map[int]string }
type Output string
func Invoke(context.Context, Input) (ToolResponse[Output], error) { return ToolResponse[Output]{}, nil }`,
			wantErr: "map keys must be strings",
		},
		{
			name: "embedded field rejected",
			source: `import "context"
type Payload struct { A string }
type Input struct { Payload }
type Output string
func Invoke(context.Context, Input) (ToolResponse[Output], error) { return ToolResponse[Output]{}, nil }`,
			wantErr: "fields must be exported and non-embedded",
		},
		{
			name: "unexported field rejected",
			source: `import "context"
type Input struct { value string }
type Output string
func Invoke(context.Context, Input) (ToolResponse[Output], error) { return ToolResponse[Output]{}, nil }`,
			wantErr: "fields must be exported and non-embedded",
		},
		{
			name: "duplicate JSON field rejected",
			source: `import "context"
type Input struct {
  First string ` + "`json:\"name\"`" + `
  Second string ` + "`json:\"name\"`" + `
}
type Output string
func Invoke(context.Context, Input) (ToolResponse[Output], error) { return ToolResponse[Output]{}, nil }`,
			wantErr: "duplicate JSON field",
		},
		{
			name: "unsupported JSON tag option rejected",
			source: `import "context"
type Input struct {
  Count int ` + "`json:\"count,omitzero\"`" + `
}
type Output string
func Invoke(context.Context, Input) (ToolResponse[Output], error) { return ToolResponse[Output]{}, nil }`,
			wantErr: "unsupported JSON tag option",
		},
		{
			name: "recursive type rejected",
			source: `import "context"
type Node struct { Next *Node }
type Input struct { Root Node }
type Output string
func Invoke(context.Context, Input) (ToolResponse[Output], error) { return ToolResponse[Output]{}, nil }`,
			wantErr: "recursive Go types are not supported",
		},
		{
			name: "imported named type rejected",
			source: `import (
  "context"
  "time"
)
type Input struct { At time.Time }
type Output string
func Invoke(context.Context, Input) (ToolResponse[Output], error) { return ToolResponse[Output]{}, nil }`,
			wantErr: "imported named Go types are not supported",
		},
		{
			name: "custom JSON method rejected",
			source: `import "context"
type Input struct { Name string }
func (Input) MarshalJSON() ([]byte, error) { return nil, nil }
type Output string
func Invoke(context.Context, Input) (ToolResponse[Output], error) { return ToolResponse[Output]{}, nil }`,
			wantErr: "custom JSON or text methods are not supported",
		},
		{
			name: "invalid dependency flag injection rejected",
			source: `import "context"
type Input struct{}
type Output string
func Invoke(context.Context, Input) (ToolResponse[Output], error) { return ToolResponse[Output]{}, nil }`,
			dependencies: []string{"-modfile=x"},
			wantErr:      "invalid dependency",
		},
		{
			name: "invalid dependency missing version rejected",
			source: `import "context"
type Input struct{}
type Output string
func Invoke(context.Context, Input) (ToolResponse[Output], error) { return ToolResponse[Output]{}, nil }`,
			dependencies: []string{"github.com/google/uuid"},
			wantErr:      "invalid dependency",
		},
		{
			name: "invalid dependency relative path rejected",
			source: `import "context"
type Input struct{}
type Output string
func Invoke(context.Context, Input) (ToolResponse[Output], error) { return ToolResponse[Output]{}, nil }`,
			dependencies: []string{"./local@v1.0.0"},
			wantErr:      "invalid dependency",
		},
		{
			name: "invalid dependency whitespace rejected",
			source: `import "context"
type Input struct{}
type Output string
func Invoke(context.Context, Input) (ToolResponse[Output], error) { return ToolResponse[Output]{}, nil }`,
			dependencies: []string{"github.com/google/uuid @v1.6.0"},
			wantErr:      "invalid dependency",
		},
		{
			name: "predeclared universe type string shadowing rejected",
			source: `import "context"
type string struct { X int }
type Input struct{}
type Output string
func Invoke(context.Context, Input) (ToolResponse[Output], error) { return ToolResponse[Output]{}, nil }`,
			wantErr: "predeclared Go identifier",
		},
		{
			name: "predeclared universe type error shadowing rejected",
			source: `import "context"
type error string
type Input struct{}
type Output string
func Invoke(context.Context, Input) (ToolResponse[Output], error) { return ToolResponse[Output]{}, nil }`,
			wantErr: "predeclared Go identifier",
		},
		{
			name: "dot import rejected",
			source: `import . "context"
type Input struct{}
type Output string
func Invoke(Context, Input) (ToolResponse[Output], error) { return ToolResponse[Output]{}, nil }`,
			wantErr: "dot import",
		},
		{
			name: "unsafe import rejected",
			source: `import (
  "context"
  "unsafe"
)
type Input struct{}
type Output string
func Invoke(context.Context, Input) (ToolResponse[Output], error) { _ = unsafe.Sizeof(0); return ToolResponse[Output]{}, nil }`,
			wantErr: `import "unsafe" is not allowed`,
		},
		{
			name: "C import rejected",
			source: `import (
  "context"
  "C"
)
type Input struct{}
type Output string
func Invoke(context.Context, Input) (ToolResponse[Output], error) { return ToolResponse[Output]{}, nil }`,
			wantErr: `import "C" is not allowed`,
		},
		{
			name: "colliding local import name rejected",
			source: `import (
  "context"
  context "math"
)
type Input struct{}
type Output string
func Invoke(context.Context, Input) (ToolResponse[Output], error) { return ToolResponse[Output]{}, nil }`,
			wantErr: "duplicate import local name",
		},
		{
			name: "Quote as map key rejected",
			source: `import "context"
type Quote string
type Input struct { Items map[Quote]string ` + "`json:\"items\"`" + ` }
type Output string
func Invoke(context.Context, Input) (ToolResponse[Output], error) { return ToolResponse[Output]{}, nil }`,
			wantErr: "Quote type is not allowed as map key",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := gotool.AnalyzeWithDependencies(tt.source, tt.dependencies)
			require.Error(t, err)
			assert.ErrorContains(t, err, tt.wantErr)
		})
	}
}
