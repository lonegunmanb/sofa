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
	if headers == cty.NilVal {
		return nil
	}
	unmarked, _ := headers.UnmarkDeep()
	if !unmarked.IsWhollyKnown() {
		return fmt.Errorf("headers must be wholly known during plan")
	}
	if unmarked.IsNull() {
		return nil
	}
	if _, err := convert.Convert(unmarked, cty.Map(cty.String)); err != nil {
		return fmt.Errorf("headers must be map(string)")
	}
	return nil
}

func HeadersSensitive(headers cty.Value) bool {
	if headers == cty.NilVal {
		return false
	}
	return specpkg.IsSensitive(headers)
}
