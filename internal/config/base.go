package config

import (
	"sync"

	"github.com/lonegunmanb/go-defaults"
	"github.com/lonegunmanb/golden"
)

var defaultsOnce sync.Once

// NewBaseConfig constructs a golden configuration configured for sofa.
func NewBaseConfig(args golden.NewBaseConfigArgs) *golden.BaseConfig {
	defaultsOnce.Do(func() {
		defaults.SetDefaults(&struct{}{})
	})

	cfg := golden.NewBasicConfigFromArgs(args)
	cfg.OverrideFunctions = Functions(args.Basedir)
	return cfg
}
