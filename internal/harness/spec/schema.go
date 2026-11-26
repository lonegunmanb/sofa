package spec

import (
	"fmt"
	"maps"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/hashicorp/hcl/v2"
	"github.com/lonegunmanb/golden"
	"github.com/zclconf/go-cty/cty"
)

// SignatureMode specifies how downloaded GitHub Release harness artifacts are verified.
type SignatureMode string

const (
	SignatureModeAuto        SignatureMode = "auto"
	SignatureModeAttestation SignatureMode = "attestation"
	SignatureModePGP         SignatureMode = "pgp"
	SignatureModeNone        SignatureMode = "none"
)

var (
	namePattern          = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)
	envVarPattern        = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	semverPattern        = regexp.MustCompile(`^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-((?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*)(?:\.(?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*))*))?(?:\+([0-9a-zA-Z-]+(?:\.[0-9a-zA-Z-]+)*))?$`)
	hostLabelPattern     = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?$`)
	sourceSegmentPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
)

// HarnessConfig holds the validated offline configuration snapshot for a harness block.
type HarnessConfig struct {
	Name        string
	Source      string
	SourceHost  string
	SourceOwner string
	SourceRepo  string
	Version     string
	Signature   SignatureMode
	SigningKey  string
	EnvRefs     map[string]string
	Options     cty.Value
}

// Validate checks all harness invariants and normalizes derived fields such as
// Signature defaults and parsed SourceHost/SourceOwner/SourceRepo.
func (c *HarnessConfig) Validate() error {
	if !namePattern.MatchString(c.Name) {
		return fmt.Errorf("harness name %q must start with an ASCII alphanumeric character and contain only ASCII letters, digits, '_', or '-'", c.Name)
	}
	if (c.Source == "") != (c.Version == "") {
		return fmt.Errorf("harness source and version must be set together or omitted together")
	}

	if c.Signature == "" {
		c.Signature = SignatureModeAuto
	}
	switch c.Signature {
	case SignatureModeAuto, SignatureModeAttestation, SignatureModePGP, SignatureModeNone:
	default:
		return fmt.Errorf("harness signature must be auto, attestation, pgp, or none")
	}

	if c.Source == "" {
		c.SourceHost = ""
		c.SourceOwner = ""
		c.SourceRepo = ""
		if c.Signature != SignatureModeAuto {
			return fmt.Errorf("harness signature %q is not valid in local plugin mode (omit source and version only with default signature)", c.Signature)
		}
		if strings.TrimSpace(c.SigningKey) != "" {
			return fmt.Errorf("harness signing_key cannot be set in local plugin mode")
		}
	} else {
		host, owner, repo, err := parseSource(c.Source)
		if err != nil {
			return err
		}
		c.SourceHost = host
		c.SourceOwner = owner
		c.SourceRepo = repo

		if err := validateVersion(c.Version); err != nil {
			return err
		}
		if c.Signature == SignatureModePGP && strings.TrimSpace(c.SigningKey) == "" {
			return fmt.Errorf("harness signing_key is required when signature is %q", SignatureModePGP)
		}
		if strings.TrimSpace(c.SigningKey) != "" && (c.Signature == SignatureModeAttestation || c.Signature == SignatureModeNone) {
			return fmt.Errorf("harness signing_key cannot be used when signature is %q", c.Signature)
		}
	}

	for key, value := range c.EnvRefs {
		if !envVarPattern.MatchString(key) {
			return fmt.Errorf("harness env_refs key %q is not a valid environment variable name", key)
		}
		if !envVarPattern.MatchString(value) {
			return fmt.Errorf("harness env_refs value %q for key %q is not a valid environment variable name", value, key)
		}
	}

	if c.Options != cty.NilVal {
		unmarked, _ := c.Options.UnmarkDeep()
		if !unmarked.IsNull() {
			if !unmarked.IsWhollyKnown() {
				return fmt.Errorf("harness options must be wholly known during plan")
			}
			if !unmarked.Type().IsObjectType() && !unmarked.Type().IsMapType() {
				return fmt.Errorf("harness options must be an object or map")
			}
			if err := validateOptionsValue(unmarked); err != nil {
				return fmt.Errorf("harness options: %w", err)
			}
		}
	}

	return nil
}

// Clone returns a deep copy of HarnessConfig with an isolated EnvRefs map.
func (c HarnessConfig) Clone() HarnessConfig {
	result := c
	if c.EnvRefs != nil {
		result.EnvRefs = maps.Clone(c.EnvRefs)
	} else {
		result.EnvRefs = map[string]string{}
	}
	return result
}

func parseSource(source string) (string, string, string, error) {
	if strings.IndexFunc(source, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}) >= 0 {
		return "", "", "", fmt.Errorf("harness source must not contain whitespace or control characters")
	}
	if strings.Contains(source, "://") {
		return "", "", "", fmt.Errorf("harness source must not include a URL scheme")
	}
	if strings.ContainsAny(source, "?#") {
		return "", "", "", fmt.Errorf("harness source must not contain query or fragment characters")
	}
	if strings.HasSuffix(strings.ToLower(source), ".git") {
		return "", "", "", fmt.Errorf("harness source must not end with .git")
	}

	parts := strings.Split(source, "/")
	if len(parts) != 3 {
		return "", "", "", fmt.Errorf("harness source %q must be in <host>/<owner>/<repo> format", source)
	}
	for _, part := range parts {
		if part == "" {
			return "", "", "", fmt.Errorf("harness source %q must be in <host>/<owner>/<repo> format", source)
		}
		if part == "." || part == ".." || strings.Contains(part, "..") {
			return "", "", "", fmt.Errorf("harness source %q must not contain . or .. path segments", source)
		}
	}

	host, owner, repo := parts[0], parts[1], parts[2]
	if !isValidHost(host) {
		return "", "", "", fmt.Errorf("harness source %q has an invalid host %q", source, host)
	}
	if !sourceSegmentPattern.MatchString(owner) || !sourceSegmentPattern.MatchString(repo) {
		return "", "", "", fmt.Errorf("harness source %q contains invalid owner or repo characters", source)
	}
	return host, owner, repo, nil
}

func isValidHost(host string) bool {
	if strings.Contains(host, "@") {
		return false
	}
	hostName := host
	if colon := strings.IndexByte(host, ':'); colon >= 0 {
		if strings.IndexByte(host[colon+1:], ':') >= 0 {
			return false
		}
		hostName = host[:colon]
		if !isValidPort(host[colon+1:]) {
			return false
		}
	}
	if hostName == "localhost" {
		return true
	}
	labels := strings.Split(hostName, ".")
	if len(labels) < 2 {
		return false
	}
	for _, label := range labels {
		if !hostLabelPattern.MatchString(label) {
			return false
		}
	}
	return true
}

func isValidPort(port string) bool {
	if len(port) == 0 || port[0] == '0' {
		return false
	}
	for i := 0; i < len(port); i++ {
		if port[i] < '0' || port[i] > '9' {
			return false
		}
	}
	n, err := strconv.Atoi(port)
	return err == nil && n >= 1 && n <= 65535
}

func validateOptionsValue(value cty.Value) error {
	if value == cty.NilVal {
		return nil
	}
	if err := validateOptionsType(value.Type()); err != nil {
		return err
	}
	if value.IsNull() {
		return nil
	}
	switch {
	case value.Type().IsObjectType():
		for name, attr := range value.AsValueMap() {
			if err := validateOptionsValue(attr); err != nil {
				return fmt.Errorf("attribute %q: %w", name, err)
			}
		}
	case value.Type().IsMapType():
		for it := value.ElementIterator(); it.Next(); {
			k, v := it.Element()
			if err := validateOptionsValue(v); err != nil {
				return fmt.Errorf("key %q: %w", k.AsString(), err)
			}
		}
	case value.Type().IsListType(), value.Type().IsSetType(), value.Type().IsTupleType():
		idx := 0
		for it := value.ElementIterator(); it.Next(); {
			_, v := it.Element()
			if err := validateOptionsValue(v); err != nil {
				return fmt.Errorf("element %d: %w", idx, err)
			}
			idx++
		}
	}
	return nil
}

func validateOptionsType(valueType cty.Type) error {
	if valueType == cty.NilType {
		return fmt.Errorf("type cannot be nil")
	}
	if valueType.IsCapsuleType() {
		return fmt.Errorf("capsule type %q is not allowed", valueType.FriendlyName())
	}
	switch {
	case valueType.IsPrimitiveType(), valueType == cty.DynamicPseudoType:
		return nil
	case valueType.IsListType(), valueType.IsSetType(), valueType.IsMapType():
		return validateOptionsType(valueType.ElementType())
	case valueType.IsTupleType():
		for i, elemType := range valueType.TupleElementTypes() {
			if err := validateOptionsType(elemType); err != nil {
				return fmt.Errorf("tuple element %d: %w", i, err)
			}
		}
		return nil
	case valueType.IsObjectType():
		for name, attrType := range valueType.AttributeTypes() {
			if err := validateOptionsType(attrType); err != nil {
				return fmt.Errorf("object attribute %q: %w", name, err)
			}
		}
		return nil
	default:
		return fmt.Errorf("unsupported type %q", valueType.FriendlyName())
	}
}

func validateVersion(version string) error {
	if strings.HasPrefix(version, "v") || strings.HasPrefix(version, "V") {
		return fmt.Errorf(
			"harness version %q must not start with %q; omit the leading \"v\" (tag v%s is resolved automatically)",
			version,
			version[:1],
			version[1:],
		)
	}
	if strings.ContainsAny(version, " \t\r\n<>=~^*|!") {
		return fmt.Errorf("harness version %q must be an exact semantic version without range operators", version)
	}
	if !semverPattern.MatchString(version) {
		return fmt.Errorf("harness version %q must be a valid semantic version (e.g. 0.1.0)", version)
	}
	return nil
}

// HarnessBlock implements the top-level `harness "<name>"` HCL block.
type HarnessBlock struct {
	*golden.BaseBlock
	Source        *string   `hcl:"source,optional"`
	Version       *string   `hcl:"version,optional"`
	Signature     *string   `hcl:"signature,optional"`
	SigningKey    *string   `hcl:"signing_key,optional"`
	EnvRefs       cty.Value `hcl:"env_refs,optional"`
	Options       cty.Value `hcl:"options,optional"`
	sourceVal     cty.Value
	versionVal    cty.Value
	signingKeyVal cty.Value
	planned       cty.Value
	plannedCfg    *HarnessConfig
}

func (b *HarnessBlock) Type() string            { return "" }
func (b *HarnessBlock) BlockType() string       { return "harness" }
func (b *HarnessBlock) AddressLength() int      { return 2 }
func (b *HarnessBlock) CanExecutePrePlan() bool { return false }

// BaseDecode enforces strict string type checking and captures marked source/version
// values before gohcl decodes fields.
func (b *HarnessBlock) BaseDecode(block *golden.HclBlock, ctx *hcl.EvalContext) error {
	b.Source = nil
	b.Version = nil
	b.Signature = nil
	b.SigningKey = nil
	b.EnvRefs = cty.NilVal
	b.Options = cty.NilVal
	b.sourceVal = cty.NilVal
	b.versionVal = cty.NilVal
	b.signingKeyVal = cty.NilVal
	b.planned = cty.NilVal
	b.plannedCfg = nil

	for _, name := range []string{"source", "version", "signature", "signing_key"} {
		attr, exists := block.Body.Attributes[name]
		if !exists {
			continue
		}
		value, diags := attr.Expr.Value(ctx)
		if diags.HasErrors() {
			return diags
		}
		_, outerMarks := value.Unmark()
		unmarked, _ := value.UnmarkDeep()
		if unmarked.IsNull() {
			switch name {
			case "source":
				b.sourceVal = cty.NullVal(cty.String).WithMarks(outerMarks)
			case "version":
				b.versionVal = cty.NullVal(cty.String).WithMarks(outerMarks)
			case "signing_key":
				b.signingKeyVal = cty.NullVal(cty.String).WithMarks(outerMarks)
			}
			continue
		}
		if !unmarked.IsWhollyKnown() || !unmarked.Type().Equals(cty.String) {
			return fmt.Errorf("harness %s must be a known string or null", name)
		}
		switch name {
		case "source":
			b.sourceVal = value
		case "version":
			b.versionVal = value
		case "signing_key":
			b.signingKeyVal = value
		}
	}
	return nil
}

// ExecuteDuringPlan validates all harness attributes and publishes the planned block value.
func (b *HarnessBlock) ExecuteDuringPlan() error {
	if b.Source != nil && *b.Source == "" {
		return fmt.Errorf("harness source must not be empty")
	}
	if b.Version != nil && *b.Version == "" {
		return fmt.Errorf("harness version must not be empty")
	}
	if b.Signature != nil && *b.Signature == "" {
		return fmt.Errorf("harness signature must not be empty")
	}
	if b.SigningKey != nil && strings.TrimSpace(*b.SigningKey) == "" {
		return fmt.Errorf("harness signing_key must not be empty")
	}

	envRefs, err := decodeEnvRefs(b.EnvRefs)
	if err != nil {
		return err
	}
	options := normalizeOptions(b.Options)

	cfg := HarnessConfig{
		Name:       b.Name(),
		Source:     derefString(b.Source),
		Version:    derefString(b.Version),
		Signature:  SignatureMode(derefString(b.Signature)),
		SigningKey: derefString(b.SigningKey),
		EnvRefs:    envRefs,
		Options:    options,
	}
	if err := cfg.Validate(); err != nil {
		return err
	}

	cloned := cfg.Clone()
	b.plannedCfg = &cloned
	b.planned = cty.ObjectVal(map[string]cty.Value{
		"address": cty.StringVal(b.CanonicalAddress()),
		"kind":    cty.StringVal("harness"),
		"name":    cty.StringVal(b.Name()),
		"source":  optionalStringValue(b.sourceVal, cfg.Source),
		"version": optionalStringValue(b.versionVal, cfg.Version),
	})
	return nil
}

// CanonicalAddress returns the module-qualified canonical address when nested in a child module.
func (b *HarnessBlock) CanonicalAddress() string {
	if b.BaseBlock != nil {
		if provider, ok := b.Config().(interface{ CanonicalAddress(string) string }); ok {
			return provider.CanonicalAddress(b.Address())
		}
	}
	return b.Address()
}

// Value returns the planned cty.Object value, or cty.DynamicVal before planning.
func (b *HarnessBlock) Value() cty.Value {
	if b.planned == cty.NilVal {
		return cty.DynamicVal
	}
	return b.planned
}

// SigningKeyValue returns the signing_key string value preserving sensitivity marks.
func (b *HarnessBlock) SigningKeyValue() cty.Value {
	if b.SigningKey == nil {
		if b.signingKeyVal != cty.NilVal {
			return b.signingKeyVal
		}
		return cty.NullVal(cty.String)
	}
	if b.signingKeyVal != cty.NilVal {
		return b.signingKeyVal
	}
	return cty.StringVal(*b.SigningKey)
}

// HarnessConfig returns a deep-copied snapshot of the harness configuration.
func (b *HarnessBlock) HarnessConfig() HarnessConfig {
	if b.plannedCfg != nil {
		return b.plannedCfg.Clone()
	}
	envRefs, _ := decodeEnvRefs(b.EnvRefs)
	sig := SignatureMode(derefString(b.Signature))
	if sig == "" {
		sig = SignatureModeAuto
	}
	return HarnessConfig{
		Name:       b.Name(),
		Source:     derefString(b.Source),
		Version:    derefString(b.Version),
		Signature:  sig,
		SigningKey: derefString(b.SigningKey),
		EnvRefs:    envRefs,
		Options:    normalizeOptions(b.Options),
	}.Clone()
}

func decodeEnvRefs(value cty.Value) (map[string]string, error) {
	if value == cty.NilVal {
		return map[string]string{}, nil
	}
	unmarked, _ := value.UnmarkDeep()
	if unmarked.IsNull() {
		return map[string]string{}, nil
	}
	if !unmarked.IsWhollyKnown() {
		return nil, fmt.Errorf("harness env_refs must be wholly known during plan")
	}
	if !unmarked.Type().IsObjectType() && !unmarked.Type().IsMapType() {
		return nil, fmt.Errorf("harness env_refs must be map(string)")
	}
	result := make(map[string]string, unmarked.LengthInt())
	iterator := unmarked.ElementIterator()
	for iterator.Next() {
		key, element := iterator.Element()
		if element.IsNull() {
			return nil, fmt.Errorf("harness env_refs value for %q must not be null", key.AsString())
		}
		if !element.Type().Equals(cty.String) {
			return nil, fmt.Errorf("harness env_refs value for %q must be a string", key.AsString())
		}
		result[key.AsString()] = element.AsString()
	}
	return result, nil
}

func normalizeOptions(value cty.Value) cty.Value {
	if value == cty.NilVal {
		return cty.NilVal
	}
	unmarked, _ := value.UnmarkDeep()
	if unmarked.IsNull() {
		return cty.NilVal
	}
	return value
}

func optionalStringValue(raw cty.Value, value string) cty.Value {
	if value == "" {
		if raw != cty.NilVal {
			_, marks := raw.Unmark()
			return cty.NullVal(cty.String).WithMarks(marks)
		}
		return cty.NullVal(cty.String)
	}
	if raw != cty.NilVal {
		return raw
	}
	return cty.StringVal(value)
}

func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

var _ golden.PlanBlock = (*HarnessBlock)(nil)
var _ golden.SingleValueBlock = (*HarnessBlock)(nil)
var _ golden.BaseDecode = (*HarnessBlock)(nil)
