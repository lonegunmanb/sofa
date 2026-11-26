package variableschema_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lonegunmanb/sofa/internal/variableschema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type protocolVariable struct {
	Name            string          `json:"name"`
	Description     *string         `json:"description"`
	Type            string          `json:"type"`
	Required        bool            `json:"required"`
	Nullable        bool            `json:"nullable"`
	Sensitive       bool            `json:"sensitive"`
	HasDefault      bool            `json:"has_default"`
	Default         json.RawMessage `json:"default"`
	DefaultRedacted bool            `json:"default_redacted"`
}

func TestInspectReturnsStableRootVariableSchemaAndRedactsSecrets(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	writeSchemaFile(t, directory, "variables.sofa.hcl", `
variable "z_required" {
  description = "Required input"
  type        = string
}

variable "bool_value" {
  type    = bool
  default = true
}

variable "number_value" {
  type    = number
  default = 2
}

variable "list_value" {
  type    = list(string)
  default = ["one", "two"]
}

variable "set_value" {
  type    = set(string)
  default = ["two", "one"]
}

variable "map_value" {
  type    = map(number)
  default = { b = 2, a = 1 }
}

variable "tuple_value" {
  type    = tuple([string, number, bool])
  default = ["one", 2, true]
}

variable "object_value" {
  type = object({
    z_required = string
    a_optional = optional(string)
    label      = optional(string, "fallback")
    nested = optional(object({
      enabled = optional(bool, true)
    }), {})
  })
  default = {
    z_required = "present"
  }
}

variable "nullable_value" {
  type     = number
  default  = null
  nullable = true
}

variable "non_nullable_value" {
  type     = string
  default  = "fallback"
  nullable = false
}

variable "secret_value" {
  type      = string
  default   = "never-print-this-secret"
  sensitive = true
}
`)

	document, err := variableschema.Inspect(t.Context(), directory)
	require.NoError(t, err)

	first, err := variableschema.Marshal(document)
	require.NoError(t, err)
	second, err := variableschema.Marshal(document)
	require.NoError(t, err)
	assert.Equal(t, first, second)
	assert.NotContains(t, string(first), "never-print-this-secret")

	text := variableschema.FormatText(document)
	assert.NotContains(t, text, "never-print-this-secret")
	assert.Contains(t, text, "<sensitive>")
	assert.Contains(t, text, "z_required")

	var decoded struct {
		SchemaVersion int                `json:"schema_version"`
		Variables     []protocolVariable `json:"variables"`
	}
	require.NoError(t, json.Unmarshal(first, &decoded))
	assert.Equal(t, 1, decoded.SchemaVersion)
	require.Len(t, decoded.Variables, 11)

	byName := make(map[string]protocolVariable, len(decoded.Variables))
	for _, v := range decoded.Variables {
		byName[v.Name] = v
	}

	reqVar := byName["z_required"]
	assert.True(t, reqVar.Required)
	assert.False(t, reqVar.HasDefault)
	assert.Equal(t, "string", reqVar.Type)
	require.NotNil(t, reqVar.Description)
	assert.Equal(t, "Required input", *reqVar.Description)

	objVar := byName["object_value"]
	assert.Contains(t, objVar.Type, "optional(string)")
	assert.Contains(t, objVar.Type, `optional(string,"fallback")`)
	assert.JSONEq(t, `{"z_required":"present","a_optional":null,"label":"fallback","nested":{"enabled":true}}`, string(objVar.Default))

	secVar := byName["secret_value"]
	assert.True(t, secVar.Sensitive)
	assert.True(t, secVar.HasDefault)
	assert.True(t, secVar.DefaultRedacted)
	assert.Equal(t, "null", string(secVar.Default))
}

func TestInspectRejectsInvalidVariableDeclarations(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		hcl     string
		wantErr string
	}{
		{
			name: "duplicate variable",
			hcl: `
variable "dup" { type = string }
variable "dup" { type = number }
`,
			wantErr: "more than once",
		},
		{
			name: "missing type",
			hcl: `
variable "no_type" { default = "abc" }
`,
			wantErr: "type",
		},
		{
			name: "dynamic type rejected",
			hcl: `
variable "dyn" { type = any }
`,
			wantErr: "dynamic type",
		},
		{
			name: "unknown attribute",
			hcl: `
variable "bad_attr" {
  type  = string
  extra = "nope"
}
`,
			wantErr: "extra",
		},
		{
			name: "nullable false with null default",
			hcl: `
variable "non_null" {
  type     = string
  nullable = false
  default  = null
}
`,
			wantErr: "nullable",
		},
		{
			name: "incompatible default",
			hcl: `
variable "bad_default" {
  type    = number
  default = "not-a-number"
}
`,
			wantErr: "incompatible",
		},
		{
			name: "sensitive invalid default redacts secret",
			hcl: `
variable "bad_secret" {
  type      = number
  sensitive = true
  default   = "do-not-leak-me"
}
`,
			wantErr: "incompatible",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			writeSchemaFile(t, dir, "variables.sofa.hcl", tt.hcl)
			_, err := variableschema.Inspect(t.Context(), dir)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
			assert.NotContains(t, err.Error(), "do-not-leak-me")
		})
	}
}

func TestFormatTextEmpty(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeSchemaFile(t, dir, "empty.sofa.hcl", `
sandbox "s" {
  runtime = "podman"
  image   = "img:1"
}
`)
	doc, err := variableschema.Inspect(t.Context(), dir)
	require.NoError(t, err)
	assert.Empty(t, doc.Variables)
	assert.Contains(t, variableschema.FormatText(doc), "No variables")
}

func writeSchemaFile(t *testing.T, dir, name, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(strings.TrimLeft(content, "\n")), 0o600))
}
