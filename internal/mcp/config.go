package mcp

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	DefaultTimeout = 30 * time.Second
	MinTimeout     = time.Second
	MaxTimeout     = 5 * time.Minute
)

type Transport string

const (
	TransportHTTP  Transport = "http"
	TransportStdio Transport = "stdio"
)

var envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type Config struct {
	Name      string
	Transport Transport
	Tools     []string
	Resources []string
	Timeout   time.Duration
	HTTP      *HTTPConfig
	Stdio     *StdioConfig
}

type HTTPConfig struct {
	URL            string
	Headers        map[string]string
	BearerToken    *string
	BearerTokenRef *string
}

type StdioConfig struct {
	Command          string
	Args             []string
	Env              map[string]string
	EnvRefs          map[string]string
	WorkingDirectory string
}

func (c Config) Validate() error { return c.ValidateSelection() }

func (c Config) ValidateSelection() error {
	if strings.TrimSpace(c.Name) == "" {
		return fmt.Errorf("mcp server name is required")
	}
	if len(c.Tools) == 0 && len(c.Resources) == 0 {
		return fmt.Errorf("mcp server must select at least one tool or resource")
	}
	if err := validateUniqueNonEmpty(c.Tools, "tool"); err != nil {
		return err
	}
	if err := validateUniqueNonEmpty(c.Resources, "resource"); err != nil {
		return err
	}
	if c.Timeout < MinTimeout || c.Timeout > MaxTimeout {
		return fmt.Errorf("mcp server timeout must be between 1s and 5m")
	}
	switch c.Transport {
	case TransportHTTP:
		if c.HTTP == nil || c.Stdio != nil {
			return fmt.Errorf("mcp server must have exactly one http or stdio block")
		}
		return c.HTTP.Validate()
	case TransportStdio:
		if c.Stdio == nil || c.HTTP != nil {
			return fmt.Errorf("mcp server must have exactly one http or stdio block")
		}
		return c.Stdio.Validate()
	default:
		return fmt.Errorf("mcp server must have exactly one http or stdio block")
	}
}

func validateUniqueNonEmpty(values []string, kind string) error {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("mcp server %ss must not contain empty values", kind)
		}
		if _, exists := seen[value]; exists {
			return fmt.Errorf("mcp server %s %q is declared more than once", kind, value)
		}
		seen[value] = struct{}{}
	}
	return nil
}

func (c HTTPConfig) Validate() error {
	parsed, err := url.Parse(c.URL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return fmt.Errorf("mcp http url must be an absolute http or https url")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("mcp http url must use http or https")
	}
	if parsed.User != nil {
		return fmt.Errorf("mcp http url must not contain user information")
	}
	for name, value := range c.Headers {
		if strings.EqualFold(name, "Authorization") {
			return fmt.Errorf("mcp http headers must not contain authorization")
		}
		if strings.TrimSpace(name) == "" || strings.ContainsAny(name+value, "\r\n") {
			return fmt.Errorf("mcp http headers contain an invalid name or value")
		}
	}
	if c.BearerToken != nil && c.BearerTokenRef != nil {
		return fmt.Errorf("mcp http bearer_token and bearer_token_ref are mutually exclusive")
	}
	if c.BearerTokenRef != nil && !envNamePattern.MatchString(*c.BearerTokenRef) {
		return fmt.Errorf("mcp http bearer_token_ref must be a valid environment variable name")
	}
	if c.BearerToken != nil && strings.TrimSpace(*c.BearerToken) == "" {
		return fmt.Errorf("mcp http bearer_token must not be empty")
	}
	return nil
}

func (c StdioConfig) Validate() error {
	if strings.TrimSpace(c.Command) == "" {
		return fmt.Errorf("mcp stdio command is required")
	}
	for name, reference := range c.EnvRefs {
		if strings.TrimSpace(name) == "" || !envNamePattern.MatchString(reference) {
			return fmt.Errorf("mcp stdio env_refs must contain valid names and references")
		}
		if _, exists := c.Env[name]; exists {
			return fmt.Errorf("mcp stdio environment variable %q is configured by both env and env_refs", name)
		}
	}
	return nil
}
