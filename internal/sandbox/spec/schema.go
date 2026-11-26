package spec

import (
	"fmt"
	"math"
	"path"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/lonegunmanb/golden"
	"github.com/zclconf/go-cty/cty"
)

var memoryPattern = regexp.MustCompile(`(?i)^[1-9][0-9]*(b|k|m|g|kb|mb|gb|kib|mib|gib)?$`)

type MountSpec struct {
	Source   string
	Target   string
	ReadOnly bool
}

type ResourcesSpec struct {
	CPUs   string
	Memory string
}

type SandboxConfig struct {
	Runtime   string
	Image     string
	Network   string
	Mounts    []MountSpec
	Resources *ResourcesSpec
}

func (c SandboxConfig) Validate() error {
	switch c.Runtime {
	case "podman", "docker", "nerdctl", "kirk":
	default:
		return fmt.Errorf("sandbox runtime must be podman, docker, nerdctl, or kirk")
	}
	if c.Image == "" {
		return fmt.Errorf("sandbox image is required")
	}
	if strings.IndexFunc(c.Image, unicode.IsSpace) >= 0 {
		return fmt.Errorf("sandbox image must not contain whitespace")
	}
	switch c.Network {
	case "none", "bridge", "host":
	default:
		return fmt.Errorf("sandbox network must be none, bridge, or host")
	}
	targets := map[string]struct{}{}
	for _, mount := range c.Mounts {
		if strings.TrimSpace(mount.Source) == "" {
			return fmt.Errorf("sandbox mount source is required")
		}
		if !strings.HasPrefix(mount.Target, "/") {
			return fmt.Errorf("sandbox mount target must be an absolute path")
		}
		target := path.Clean(mount.Target)
		if target == "/" {
			return fmt.Errorf("sandbox mount target must not be the container root")
		}
		if target == "/workspace" || strings.HasPrefix(target, "/workspace/") ||
			target == "/.sofa" || strings.HasPrefix(target, "/.sofa/") {
			return fmt.Errorf("sandbox mount target %q conflicts with a reserved mount", target)
		}
		if _, exists := targets[target]; exists {
			return fmt.Errorf("sandbox mount target %q is duplicated", target)
		}
		targets[target] = struct{}{}
		if !mount.ReadOnly {
			return fmt.Errorf("sandbox mount %q must set read_only = true", target)
		}
	}
	if c.Resources != nil {
		if c.Resources.CPUs != "" {
			cpus, err := strconv.ParseFloat(c.Resources.CPUs, 64)
			if err != nil || cpus <= 0 || math.IsNaN(cpus) || math.IsInf(cpus, 0) {
				return fmt.Errorf("sandbox resources cpus must be a positive number")
			}
		}
		if c.Resources.Memory != "" && !memoryPattern.MatchString(c.Resources.Memory) {
			return fmt.Errorf("sandbox resources memory must use a container memory size")
		}
	}
	return nil
}

type MountBlock struct {
	Source   string `hcl:"source"`
	Target   string `hcl:"target"`
	ReadOnly *bool  `hcl:"read_only,optional"`
}

type ResourcesBlock struct {
	CPUs   string `hcl:"cpus,optional"`
	Memory string `hcl:"memory,optional"`
}

type SandboxBlock struct {
	*golden.BaseBlock
	Runtime        string           `hcl:"runtime"`
	Image          string           `hcl:"image"`
	Network        *string          `hcl:"network,optional"`
	MountBlocks    []MountBlock     `hcl:"mount,block"`
	ResourceBlocks []ResourcesBlock `hcl:"resources,block"`
	planned        cty.Value
	plannedConfig  *SandboxConfig
}

func (b *SandboxBlock) Type() string            { return "" }
func (b *SandboxBlock) BlockType() string       { return "sandbox" }
func (b *SandboxBlock) AddressLength() int      { return 2 }
func (b *SandboxBlock) CanExecutePrePlan() bool { return false }

func (b *SandboxBlock) ExecuteDuringPlan() error {
	if len(b.ResourceBlocks) > 1 {
		return fmt.Errorf("sandbox must have at most one resources block")
	}
	network := "bridge"
	if b.Network != nil {
		network = *b.Network
	}
	mounts := make([]MountSpec, len(b.MountBlocks))
	for index, mount := range b.MountBlocks {
		readOnly := true
		if mount.ReadOnly != nil {
			readOnly = *mount.ReadOnly
		}
		mounts[index] = MountSpec{Source: mount.Source, Target: mount.Target, ReadOnly: readOnly}
	}
	var resources *ResourcesSpec
	if len(b.ResourceBlocks) == 1 {
		resources = &ResourcesSpec{
			CPUs:   b.ResourceBlocks[0].CPUs,
			Memory: b.ResourceBlocks[0].Memory,
		}
	}
	config := SandboxConfig{
		Runtime:   b.Runtime,
		Image:     b.Image,
		Network:   network,
		Mounts:    mounts,
		Resources: resources,
	}
	if err := config.Validate(); err != nil {
		return err
	}
	plannedConfig := cloneSandboxConfig(config)
	b.plannedConfig = &plannedConfig
	b.planned = cty.ObjectVal(map[string]cty.Value{
		"address": cty.StringVal(b.CanonicalAddress()),
		"kind":    cty.StringVal("sandbox"),
		"name":    cty.StringVal(b.Name()),
		"runtime": cty.StringVal(config.Runtime),
		"image":   cty.StringVal(config.Image),
		"network": cty.StringVal(config.Network),
	})
	return nil
}

// CanonicalAddress returns the module-qualified canonical address when nested in a child module.
func (b *SandboxBlock) CanonicalAddress() string {
	if b.BaseBlock != nil {
		if provider, ok := b.Config().(interface{ CanonicalAddress(string) string }); ok {
			return provider.CanonicalAddress(b.Address())
		}
	}
	return b.Address()
}

func (b *SandboxBlock) Value() cty.Value {
	if b.planned == cty.NilVal {
		return cty.DynamicVal
	}
	return b.planned
}

func (b *SandboxBlock) SandboxConfig() SandboxConfig {
	if b.plannedConfig != nil {
		return cloneSandboxConfig(*b.plannedConfig)
	}
	network := "bridge"
	if b.Network != nil {
		network = *b.Network
	}
	mounts := make([]MountSpec, len(b.MountBlocks))
	for index, mount := range b.MountBlocks {
		readOnly := true
		if mount.ReadOnly != nil {
			readOnly = *mount.ReadOnly
		}
		mounts[index] = MountSpec{Source: mount.Source, Target: mount.Target, ReadOnly: readOnly}
	}
	var resources *ResourcesSpec
	if len(b.ResourceBlocks) == 1 {
		resources = &ResourcesSpec{CPUs: b.ResourceBlocks[0].CPUs, Memory: b.ResourceBlocks[0].Memory}
	}
	return SandboxConfig{
		Runtime: b.Runtime, Image: b.Image,
		Network: network, Mounts: mounts, Resources: resources,
	}
}

func cloneSandboxConfig(config SandboxConfig) SandboxConfig {
	result := config
	result.Mounts = append([]MountSpec{}, config.Mounts...)
	if config.Resources != nil {
		resources := *config.Resources
		result.Resources = &resources
	}
	return result
}

var _ golden.PlanBlock = (*SandboxBlock)(nil)
var _ golden.SingleValueBlock = (*SandboxBlock)(nil)
