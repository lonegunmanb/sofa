package spec

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"time"

	"github.com/lonegunmanb/golden"
	mcppkg "github.com/lonegunmanb/sofa/internal/mcp"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/convert"
)

type ServerBlock struct {
	*golden.BaseBlock
	Tools       []string     `hcl:"tools,optional"`
	Resources   []string     `hcl:"resources,optional"`
	Timeout     *string      `hcl:"timeout,optional"`
	HTTPBlocks  []HTTPBlock  `hcl:"http,block"`
	StdioBlocks []StdioBlock `hcl:"stdio,block"`
	planned     mcppkg.Config
}

type HTTPBlock struct {
	URL            string    `hcl:"url"`
	Headers        cty.Value `hcl:"headers,optional"`
	BearerToken    *string   `hcl:"bearer_token,optional"`
	BearerTokenRef *string   `hcl:"bearer_token_ref,optional"`
}

type StdioBlock struct {
	Command          *string   `hcl:"command,optional"`
	Args             []string  `hcl:"args,optional"`
	Env              cty.Value `hcl:"env,optional"`
	EnvRefs          cty.Value `hcl:"env_refs,optional"`
	WorkingDirectory string    `hcl:"working_directory,optional"`
}

func (b *ServerBlock) Type() string            { return "" }
func (b *ServerBlock) BlockType() string       { return "mcp_server" }
func (b *ServerBlock) AddressLength() int      { return 2 }
func (b *ServerBlock) CanExecutePrePlan() bool { return false }

func (b *ServerBlock) ExecuteDuringPlan() error {
	if len(b.HTTPBlocks)+len(b.StdioBlocks) != 1 {
		return fmt.Errorf("mcp server must have exactly one http or stdio block")
	}
	timeout := mcppkg.DefaultTimeout
	if b.Timeout != nil {
		parsed, err := time.ParseDuration(*b.Timeout)
		if err != nil {
			return fmt.Errorf("mcp server timeout: %w", err)
		}
		timeout = parsed
	}
	config := mcppkg.Config{
		Name:      b.Name(),
		Tools:     append([]string{}, b.Tools...),
		Resources: append([]string{}, b.Resources...),
		Timeout:   timeout,
	}
	if len(b.HTTPBlocks) == 1 {
		httpBlock := b.HTTPBlocks[0]
		headers, err := stringMap(httpBlock.Headers, "mcp http headers")
		if err != nil {
			return err
		}
		config.Transport = mcppkg.TransportHTTP
		config.HTTP = &mcppkg.HTTPConfig{
			URL:            httpBlock.URL,
			Headers:        headers,
			BearerToken:    clonePointer(httpBlock.BearerToken),
			BearerTokenRef: clonePointer(httpBlock.BearerTokenRef),
		}
	} else {
		stdioBlock := b.StdioBlocks[0]
		env, err := stringMap(stdioBlock.Env, "mcp stdio env")
		if err != nil {
			return err
		}
		envRefs, err := stringMap(stdioBlock.EnvRefs, "mcp stdio env_refs")
		if err != nil {
			return err
		}
		command := ""
		if stdioBlock.Command != nil {
			command = *stdioBlock.Command
		}
		config.Transport = mcppkg.TransportStdio
		config.Stdio = &mcppkg.StdioConfig{
			Command:          command,
			Args:             append([]string{}, stdioBlock.Args...),
			Env:              env,
			EnvRefs:          envRefs,
			WorkingDirectory: stdioBlock.WorkingDirectory,
		}
	}
	if err := config.Validate(); err != nil {
		return err
	}
	b.planned = config
	return nil
}

func (b *ServerBlock) ServerConfig() mcppkg.Config {
	result := b.planned.Clone()
	result.RuntimeName = b.CanonicalAddress()
	return result
}

func (b *ServerBlock) Value() cty.Value {
	toolIDs := make(map[string]cty.Value, len(b.Tools))
	for _, tool := range b.Tools {
		toolIDs[tool] = cty.StringVal(b.ToolID(tool))
	}
	resourceIDs := make(map[string]cty.Value, len(b.Resources))
	for _, resource := range b.Resources {
		resourceIDs[resource] = cty.StringVal(b.ResourceID(resource))
	}
	return cty.ObjectVal(map[string]cty.Value{
		"address":      cty.StringVal(b.Address()),
		"kind":         cty.StringVal("mcp_server"),
		"name":         cty.StringVal(b.Name()),
		"tool_ids":     stringMapValue(toolIDs),
		"resource_ids": stringMapValue(resourceIDs),
	})
}

func (b *ServerBlock) CanonicalAddress() string {
	if b.BaseBlock != nil {
		if provider, ok := b.Config().(interface{ CanonicalAddress(string) string }); ok {
			return provider.CanonicalAddress(b.Address())
		}
	}
	return b.Address()
}

func (b *ServerBlock) ToolID(tool string) string {
	return toolIDFor(b.CanonicalAddress(), b.Name(), tool)
}

func (b *ServerBlock) ResourceID(resource string) string {
	return resourceIDFor(b.CanonicalAddress(), b.Name(), resource)
}

func toolIDFor(canonical, name, tool string) string {
	digest := sha256.Sum256([]byte("sofa/mcp-tool-id/v1\x00" + canonical + "\x00" + tool))
	return "mcp_tool_" + idPart(name) + "__" + idPart(tool) + "_" + uuidFromDigest(digest)
}

func resourceIDFor(canonical, name, resource string) string {
	digest := sha256.Sum256([]byte("sofa/mcp-resource-id/v1\x00" + canonical + "\x00" + resource))
	return "mcp_resource_" + idPart(name) + "__" + idPart(resource) + "_" + uuidFromDigest(digest)
}

func uuidFromDigest(digest [sha256.Size]byte) string {
	digest[6] = (digest[6] & 0x0f) | 0x80
	digest[8] = (digest[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", digest[0:4], digest[4:6], digest[6:8], digest[8:10], digest[10:16])
}

func idPart(value string) string {
	var result strings.Builder
	for _, character := range value {
		switch {
		case character >= 'a' && character <= 'z',
			character >= 'A' && character <= 'Z',
			character >= '0' && character <= '9',
			character == '_', character == '-':
			result.WriteRune(character)
		default:
			result.WriteByte('_')
		}
	}
	if result.Len() == 0 {
		return "tool"
	}
	return result.String()
}

func stringMap(value cty.Value, field string) (map[string]string, error) {
	if value == cty.NilVal {
		return map[string]string{}, nil
	}
	unmarked, _ := value.UnmarkDeep()
	if unmarked.IsNull() {
		return map[string]string{}, nil
	}
	if !unmarked.IsWhollyKnown() {
		return nil, fmt.Errorf("%s must be wholly known during plan", field)
	}
	converted, err := convert.Convert(unmarked, cty.Map(cty.String))
	if err != nil {
		return nil, fmt.Errorf("%s must be map(string)", field)
	}
	result := make(map[string]string, converted.LengthInt())
	iterator := converted.ElementIterator()
	for iterator.Next() {
		key, item := iterator.Element()
		if item.IsNull() {
			return nil, fmt.Errorf("%s value for %q must not be null", field, key.AsString())
		}
		result[key.AsString()] = item.AsString()
	}
	return result, nil
}

func stringMapValue(values map[string]cty.Value) cty.Value {
	if len(values) == 0 {
		return cty.MapValEmpty(cty.String)
	}
	return cty.MapVal(values)
}

func clonePointer[T any](value *T) *T {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

var _ golden.PlanBlock = (*ServerBlock)(nil)
var _ golden.SingleValueBlock = (*ServerBlock)(nil)
