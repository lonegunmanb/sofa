package config

import (
	"sync"

	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
)

type ParsedFile struct {
	Path   string
	Syntax *hclsyntax.Body
	Write  *hclwrite.Body
}

type RootModule struct {
	Files []ParsedFile
}

type RootDefaultsHook func(*RootModule) error

var rootDefaultsHookState struct {
	sync.RWMutex
	hook RootDefaultsHook
}

// RegisterRootDefaultsHook installs or replaces the generic pre-adaptation
// hook. Passing nil clears the hook.
func RegisterRootDefaultsHook(hook RootDefaultsHook) {
	rootDefaultsHookState.Lock()
	defer rootDefaultsHookState.Unlock()
	rootDefaultsHookState.hook = hook
}

func runRootDefaultsHook(module *RootModule) error {
	rootDefaultsHookState.RLock()
	hook := rootDefaultsHookState.hook
	rootDefaultsHookState.RUnlock()
	if hook == nil {
		return nil
	}
	return hook(module)
}
