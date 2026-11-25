package provider

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"

	specpkg "github.com/lonegunmanb/sofa/internal/spec"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/convert"
)

type Type string

const (
	TypeOpenAI    Type = "openai"
	TypeAzure     Type = "azure"
	TypeAnthropic Type = "anthropic"
)

type WireAPI string

const (
	WireAPICompletions WireAPI = "completions"
	WireAPIResponses   WireAPI = "responses"
)

type Transport string

const (
	TransportHTTP       Transport = "http"
	TransportWebSockets Transport = "websockets"
)

var environmentVariablePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type Config struct {
	Type           Type
	Endpoint       string
	WireAPI        *WireAPI
	Transport      *Transport
	Headers        cty.Value
	APIKey         *string
	APIKeyRef      *string
	BearerToken    *string
	BearerTokenRef *string
	Retry          RetryOverride
}

func (c Config) Validate() error {
	switch c.Type {
	case TypeOpenAI, TypeAzure, TypeAnthropic:
	default:
		return fmt.Errorf("provider type must be one of openai, azure, or anthropic")
	}
	parsedEndpoint, err := url.Parse(c.Endpoint)
	if err != nil || parsedEndpoint.Scheme == "" || parsedEndpoint.Host == "" {
		return fmt.Errorf("provider endpoint must be an absolute HTTP(S) URL")
	}
	if parsedEndpoint.Scheme != "http" && parsedEndpoint.Scheme != "https" {
		return fmt.Errorf("provider endpoint must use http or https")
	}
	cleanedPath := strings.TrimSuffix(parsedEndpoint.Path, "/")
	if strings.HasSuffix(cleanedPath, "/chat/completions") || strings.HasSuffix(cleanedPath, "/responses") {
		return fmt.Errorf("provider endpoint must be an API root, not a chat/responses endpoint path")
	}
	if c.WireAPI != nil && *c.WireAPI != WireAPICompletions && *c.WireAPI != WireAPIResponses {
		return fmt.Errorf("wire_api must be completions or responses")
	}
	if c.Transport != nil && *c.Transport != TransportHTTP && *c.Transport != TransportWebSockets {
		return fmt.Errorf("transport must be http or websockets")
	}
	if c.Type == TypeAnthropic && (c.WireAPI != nil || c.Transport != nil) {
		return fmt.Errorf("anthropic provider must not set wire_api or transport")
	}
	if c.Transport != nil && *c.Transport == TransportWebSockets {
		wireAPI := WireAPICompletions
		if c.WireAPI != nil {
			wireAPI = *c.WireAPI
		}
		if wireAPI != WireAPIResponses {
			return fmt.Errorf("websockets transport requires responses wire_api")
		}
	}
	if err := validateAuthentication(c); err != nil {
		return err
	}
	if err := ValidateHeaders(c.Headers); err != nil {
		return err
	}
	_, err = MergeRetry(DefaultRetryPolicy(), c.Retry)
	return err
}

func validateAuthentication(c Config) error {
	values := []*string{c.APIKey, c.APIKeyRef, c.BearerToken, c.BearerTokenRef}
	configured := 0
	for _, value := range values {
		if value == nil {
			continue
		}
		configured++
		if strings.TrimSpace(*value) == "" {
			return fmt.Errorf("authentication values must not be empty")
		}
	}
	if configured > 1 {
		return fmt.Errorf("at most one authentication field may be set")
	}
	for _, ref := range []*string{c.APIKeyRef, c.BearerTokenRef} {
		if ref != nil && !environmentVariablePattern.MatchString(*ref) {
			return fmt.Errorf("authentication reference %q is not a valid environment variable name", *ref)
		}
	}
	return nil
}

func ValidateHeaders(headers cty.Value) error {
	_, err := NormalizeHeaders(headers)
	return err
}

func NormalizeHeaders(headers cty.Value) (cty.Value, error) {
	if headers == cty.NilVal {
		return cty.NilVal, nil
	}
	unmarked, marks := headers.UnmarkDeepWithPaths()
	if !unmarked.IsWhollyKnown() {
		return cty.NilVal, fmt.Errorf("headers must be wholly known during plan")
	}
	if unmarked.IsNull() {
		return cty.NilVal, nil
	}
	converted, err := convert.Convert(unmarked, cty.Map(cty.String))
	if err != nil {
		return cty.NilVal, fmt.Errorf("headers must be map(string)")
	}
	iterator := converted.ElementIterator()
	for iterator.Next() {
		key, value := iterator.Element()
		if value.IsNull() {
			return cty.NilVal, fmt.Errorf("headers value for %q must not be null", key.AsString())
		}
	}
	if len(marks) > 0 {
		converted = converted.MarkWithPaths(objectMarksToMapPaths(marks))
	}
	return converted, nil
}

func HeadersSensitive(headers cty.Value) bool {
	if headers == cty.NilVal {
		return false
	}
	return specpkg.IsSensitive(headers)
}

func objectMarksToMapPaths(marks []cty.PathValueMarks) []cty.PathValueMarks {
	result := make([]cty.PathValueMarks, len(marks))
	for index, pathMarks := range marks {
		path := make(cty.Path, len(pathMarks.Path))
		for stepIndex, step := range pathMarks.Path {
			if attribute, ok := step.(cty.GetAttrStep); ok {
				step = cty.IndexStep{Key: cty.StringVal(attribute.Name)}
			}
			path[stepIndex] = step
		}
		result[index] = cty.PathValueMarks{Path: path, Marks: pathMarks.Marks}
	}
	return result
}

func (c Config) Clone() Config {
	result := c
	result.WireAPI = clonePointer(c.WireAPI)
	result.Transport = clonePointer(c.Transport)
	result.APIKey = clonePointer(c.APIKey)
	result.APIKeyRef = clonePointer(c.APIKeyRef)
	result.BearerToken = clonePointer(c.BearerToken)
	result.BearerTokenRef = clonePointer(c.BearerTokenRef)
	result.Retry.LifecycleRetries = clonePointer(c.Retry.LifecycleRetries)
	result.Retry.ModelCallRetries = clonePointer(c.Retry.ModelCallRetries)
	result.Retry.Interval = clonePointer(c.Retry.Interval)
	result.Retry.MaxInterval = clonePointer(c.Retry.MaxInterval)
	result.Retry.ErrorMessageRegex = append([]string{}, c.Retry.ErrorMessageRegex...)
	return result
}
