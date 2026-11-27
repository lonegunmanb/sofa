package plan

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/lonegunmanb/sofa/internal/module"
	"github.com/zclconf/go-cty/cty"
)

// NormalizeSkillDirectories validates and normalizes all known skill_directories
// entries on worker, dynamic_worker, and qc configs across planned (and child modules),
// without mutating the input Plan.
func NormalizeSkillDirectories(planned *Plan, source ArchiveSource) (*Plan, error) {
	if planned == nil {
		return nil, fmt.Errorf("plan is nil")
	}
	_, bundleDirs, err := collectBundleEntries(planned, source)
	if err != nil {
		return nil, err
	}
	normalized, _, err := normalizeAndCollectSkillDirectories(planned, source, bundleDirs)
	if err != nil {
		return nil, err
	}
	return normalized, nil
}

type skillNormalizer struct {
	rootSourceDir string
	moduleSources map[string]string // moduleBundlePath -> abs source dir
	bundleDirs    map[string]bool
	extCollected  map[string]string // hash -> cleaned abs path
	extEntries    []archiveEntry
}

func normalizeAndCollectSkillDirectories(
	planned *Plan,
	source ArchiveSource,
	bundleDirs map[string]bool,
) (*Plan, []archiveEntry, error) {
	rootSourceDir, err := resolveRootSourceDir(planned, source)
	if err != nil {
		return nil, nil, err
	}
	n := &skillNormalizer{
		rootSourceDir: rootSourceDir,
		moduleSources: make(map[string]string),
		bundleDirs:    bundleDirs,
		extCollected:  make(map[string]string),
	}
	n.collectModuleSourceDirs(planned, "config", rootSourceDir, source)

	normalized, err := n.normalizePlan(planned, "config", rootSourceDir, source)
	if err != nil {
		return nil, nil, err
	}
	return normalized, n.extEntries, nil
}

func resolveRootSourceDir(planned *Plan, source ArchiveSource) (string, error) {
	raw := strings.TrimSpace(source.RootSourceDir)
	if raw == "" {
		raw = strings.TrimSpace(source.ConfigDir)
	}
	if raw == "" && planned != nil {
		raw = strings.TrimSpace(planned.directory)
	}
	if raw == "" {
		return "", fmt.Errorf("config directory is required")
	}
	abs, err := filepath.Abs(raw)
	if err != nil {
		return "", fmt.Errorf("resolve root source directory %q: %w", raw, err)
	}
	return filepath.Clean(abs), nil
}

func (n *skillNormalizer) collectModuleSourceDirs(p *Plan, defaultBundleBase, parentSourceDir string, source ArchiveSource) {
	if p == nil {
		return
	}
	for _, node := range p.nodes {
		if node.Kind != "module" || node.Module == nil || node.Module.Plan == nil {
			continue
		}
		childBase := node.Module.BundlePath
		if childBase == "" {
			childBase = node.Module.Plan.BundlePath()
		}
		childSourceDir := resolveChildModuleSourceDir(node.Module, childBase, parentSourceDir, source)
		if childBase != "" && childSourceDir != "" {
			n.moduleSources[path.Clean(filepath.ToSlash(childBase))] = childSourceDir
		}
		n.collectModuleSourceDirs(node.Module.Plan, childBase, childSourceDir, source)
	}
}

func resolveChildModuleSourceDir(mod *ModuleSpec, childBundlePath, parentSourceDir string, source ArchiveSource) string {
	if mod == nil {
		return ""
	}
	if strings.TrimSpace(source.RootSourceDir) != "" {
		if module.IsLocalSource(mod.Source) {
			cand := filepath.FromSlash(mod.Source)
			if !filepath.IsAbs(cand) && parentSourceDir != "" {
				cand = filepath.Join(parentSourceDir, cand)
			}
			if abs, err := filepath.Abs(cand); err == nil {
				return filepath.Clean(abs)
			}
		}
		if strings.HasPrefix(childBundlePath, "modules/") && strings.TrimSpace(source.ModulesDir) != "" {
			rel := strings.TrimPrefix(childBundlePath, "modules/")
			if abs, err := filepath.Abs(filepath.Join(source.ModulesDir, filepath.FromSlash(rel))); err == nil {
				return filepath.Clean(abs)
			}
		}
	}
	if mod.Plan != nil && strings.TrimSpace(mod.Plan.Directory()) != "" {
		if abs, err := filepath.Abs(mod.Plan.Directory()); err == nil {
			return filepath.Clean(abs)
		}
	}
	if strings.HasPrefix(childBundlePath, "modules/") && strings.TrimSpace(source.ModulesDir) != "" {
		rel := strings.TrimPrefix(childBundlePath, "modules/")
		if abs, err := filepath.Abs(filepath.Join(source.ModulesDir, filepath.FromSlash(rel))); err == nil {
			return filepath.Clean(abs)
		}
	}
	if module.IsLocalSource(mod.Source) && parentSourceDir != "" {
		cand := filepath.FromSlash(mod.Source)
		if !filepath.IsAbs(cand) {
			cand = filepath.Join(parentSourceDir, cand)
		}
		if abs, err := filepath.Abs(cand); err == nil {
			return filepath.Clean(abs)
		}
	}
	return ""
}

func (n *skillNormalizer) normalizePlan(p *Plan, defaultBundleBase, moduleSourceDir string, source ArchiveSource) (*Plan, error) {
	if p == nil {
		return nil, nil
	}
	base := p.bundlePath
	if base == "" {
		base = defaultBundleBase
	}
	nodes := p.Nodes()
	for i := range nodes {
		node := &nodes[i]
		switch node.Kind {
		case "worker":
			updatedCfg, changed, err := n.normalizeWorkerConfig(node.Address, node.CanonicalAddress, node.Config, base, moduleSourceDir)
			if err != nil {
				return nil, err
			}
			if changed {
				node.Config = updatedCfg
			}
		case "dynamic_worker":
			if node.Config != cty.NilVal && node.Config.IsKnown() && !node.Config.IsNull() {
				unmarked, cfgMarks := node.Config.Unmark()
				if unmarked.Type().IsObjectType() && unmarked.Type().HasAttribute("content") {
					contentVal := unmarked.GetAttr("content")
					updatedContent, changed, err := n.normalizeWorkerConfig(node.Address, node.CanonicalAddress, contentVal, base, moduleSourceDir)
					if err != nil {
						return nil, err
					}
					if changed {
						attrs := unmarked.AsValueMap()
						attrs["content"] = updatedContent
						node.Config = cty.ObjectVal(attrs).WithMarks(cfgMarks)
					}
				}
			}
		case "module":
			if node.Module != nil && node.Module.Plan != nil {
				childBase := node.Module.BundlePath
				if childBase == "" {
					childBase = node.Module.Plan.BundlePath()
				}
				childSourceDir := resolveChildModuleSourceDir(node.Module, childBase, moduleSourceDir, source)
				childPlan, err := n.normalizePlan(node.Module.Plan, childBase, childSourceDir, source)
				if err != nil {
					return nil, err
				}
				node.Module.Plan = childPlan
			}
		}
	}
	return &Plan{
		directory:           p.directory,
		bundlePath:          p.bundlePath,
		nodes:               nodes,
		outputs:             cloneOutputs(p.outputs),
		context:             cloneContext(p.context),
		localExpressions:    maps.Clone(p.localExpressions),
		deferredValidations: cloneDeferredValidations(p.deferredValidations),
		tools:               cloneTools(p.tools),
		pluginLock:          clonePluginLock(p.pluginLock),
	}, nil
}

func (n *skillNormalizer) normalizeWorkerConfig(
	addr, canonicalAddr string,
	cfg cty.Value,
	moduleBundlePath, moduleSourceDir string,
) (cty.Value, bool, error) {
	if cfg == cty.NilVal || !cfg.IsKnown() || cfg.IsNull() {
		return cfg, false, nil
	}
	unmarked, cfgMarks := cfg.Unmark()
	if !unmarked.Type().IsObjectType() {
		return cfg, false, nil
	}

	displayAddr := canonicalAddr
	if displayAddr == "" {
		displayAddr = addr
	}

	var attrs map[string]cty.Value
	changed := false

	if unmarked.Type().HasAttribute("skill_directories") {
		updatedList, listChanged, err := n.normalizeSkillDirList(displayAddr, unmarked.GetAttr("skill_directories"), moduleBundlePath, moduleSourceDir)
		if err != nil {
			return cty.NilVal, false, err
		}
		if listChanged {
			attrs = unmarked.AsValueMap()
			attrs["skill_directories"] = updatedList
			changed = true
		}
	}

	if unmarked.Type().HasAttribute("qc") {
		qcVal := unmarked.GetAttr("qc")
		if qcVal != cty.NilVal && qcVal.IsKnown() && !qcVal.IsNull() {
			qcUnmarked, qcMarks := qcVal.Unmark()
			if qcUnmarked.Type().IsObjectType() && qcUnmarked.Type().HasAttribute("skill_directories") {
				updatedQCList, qcChanged, err := n.normalizeSkillDirList(displayAddr, qcUnmarked.GetAttr("skill_directories"), moduleBundlePath, moduleSourceDir)
				if err != nil {
					return cty.NilVal, false, err
				}
				if qcChanged {
					if attrs == nil {
						attrs = unmarked.AsValueMap()
					}
					qcAttrs := qcUnmarked.AsValueMap()
					qcAttrs["skill_directories"] = updatedQCList
					attrs["qc"] = cty.ObjectVal(qcAttrs).WithMarks(qcMarks)
					changed = true
				}
			}
		}
	}

	if !changed {
		return cfg, false, nil
	}
	return cty.ObjectVal(attrs).WithMarks(cfgMarks), true, nil
}

func (n *skillNormalizer) normalizeSkillDirList(
	nodeAddr string,
	list cty.Value,
	moduleBundlePath, moduleSourceDir string,
) (cty.Value, bool, error) {
	if list == cty.NilVal || !list.IsKnown() || list.IsNull() {
		return list, false, nil
	}
	unmarkedList, listMarks := list.Unmark()
	if !unmarkedList.CanIterateElements() || unmarkedList.LengthInt() == 0 {
		return list, false, nil
	}

	elems := make([]cty.Value, 0, unmarkedList.LengthInt())
	changed := false
	for it := unmarkedList.ElementIterator(); it.Next(); {
		_, item := it.Element()
		if !item.IsKnown() || item.IsNull() {
			elems = append(elems, item)
			continue
		}
		unmarkedItem, itemMarks := item.Unmark()
		if !unmarkedItem.Type().Equals(cty.String) {
			elems = append(elems, item)
			continue
		}
		raw := unmarkedItem.AsString()
		normalized, err := n.classifyAndCollectSkillDir(nodeAddr, raw, moduleBundlePath, moduleSourceDir)
		if err != nil {
			return cty.NilVal, false, err
		}
		if normalized != raw {
			changed = true
		}
		elems = append(elems, cty.StringVal(normalized).WithMarks(itemMarks))
	}

	if !changed {
		return list, false, nil
	}
	var rebuilt cty.Value
	if unmarkedList.Type().IsTupleType() {
		rebuilt = cty.TupleVal(elems)
	} else {
		rebuilt = cty.ListVal(elems)
	}
	return rebuilt.WithMarks(listMarks), true, nil
}

func (n *skillNormalizer) classifyAndCollectSkillDir(
	nodeAddr, raw, moduleBundlePath, moduleSourceDir string,
) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", fmt.Errorf("%s skill_directories entry %q is invalid: must not be empty", nodeAddr, raw)
	}
	slash := filepath.ToSlash(trimmed)

	// 1. Container paths: /workspace, /workspace/..., /.sofa/imports/...
	if slash == "/workspace" || strings.HasPrefix(slash, "/workspace/") ||
		slash == "/.sofa/imports" || strings.HasPrefix(slash, "/.sofa/imports/") {
		cleaned := path.Clean(slash)
		if cleaned != "/workspace" && !strings.HasPrefix(cleaned, "/workspace/") &&
			cleaned != "/.sofa/imports" && !strings.HasPrefix(cleaned, "/.sofa/imports/") {
			return "", fmt.Errorf("%s skill_directories entry %q escapes container path", nodeAddr, raw)
		}
		return raw, nil
	}

	// 2a. Home-expanded external paths: ~ or ~/...
	if trimmed == "~" || strings.HasPrefix(trimmed, "~/") || strings.HasPrefix(trimmed, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil || strings.TrimSpace(home) == "" {
			return "", fmt.Errorf("%s skill_directories entry %q cannot resolve user home directory: %w", nodeAddr, raw, err)
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(slash, "~"), "/")
		absClient := filepath.Clean(filepath.Join(home, filepath.FromSlash(rel)))
		return n.collectExternalSkillDir(nodeAddr, raw, absClient)
	}

	// 2b. Absolute host paths (e.g. D:/x, C:\x, /home/u/x when not /workspace or /.sofa/imports).
	if filepath.IsAbs(trimmed) || strings.HasPrefix(slash, "/") || (len(slash) >= 2 && isASCIIAlpha(slash[0]) && slash[1] == ':') {
		hostPath := filepath.FromSlash(trimmed)
		absClient, err := filepath.Abs(hostPath)
		if err != nil {
			return "", fmt.Errorf("%s skill_directories entry %q is invalid: %w", nodeAddr, raw, err)
		}
		return n.collectExternalSkillDir(nodeAddr, raw, filepath.Clean(absClient))
	}

	// 3. Bundle-prefixed paths ("config/...", "modules/<name>/...") when not explicitly relative ("./", "../").
	modBase := moduleBundlePath
	if modBase == "" {
		modBase = "config"
	}
	if !strings.HasPrefix(slash, "./") && !strings.HasPrefix(slash, "../") && slash != "." && slash != ".." {
		if slash == modBase || strings.HasPrefix(slash, modBase+"/") {
			relFromMod := strings.TrimPrefix(strings.TrimPrefix(slash, modBase), "/")
			if relFromMod == "" {
				relFromMod = "."
			}
			return n.resolveClientRelativeSkillDir(nodeAddr, raw, relFromMod, modBase, moduleSourceDir)
		}
		if slash == "config" || strings.HasPrefix(slash, "config/") ||
			slash == "modules" || strings.HasPrefix(slash, "modules/") {
			cleaned := path.Clean(slash)
			if cleaned == "config" || strings.HasPrefix(cleaned, "config/") ||
				cleaned == "modules" || strings.HasPrefix(cleaned, "modules/") {
				if err := n.requireBundleDir(nodeAddr, raw, cleaned); err != nil {
					return "", err
				}
				return cleaned, nil
			}
		}
	}

	// 4. Relative paths resolved on the client against the module's source directory.
	return n.resolveClientRelativeSkillDir(nodeAddr, raw, slash, modBase, moduleSourceDir)
}

func (n *skillNormalizer) resolveClientRelativeSkillDir(
	nodeAddr, raw, relSlash, modBase, moduleSourceDir string,
) (string, error) {
	if moduleSourceDir == "" {
		moduleSourceDir = n.rootSourceDir
	}
	candidate := filepath.Join(moduleSourceDir, filepath.FromSlash(relSlash))
	absClient, err := filepath.Abs(candidate)
	if err != nil {
		return "", fmt.Errorf("%s skill_directories entry %q is invalid: %w", nodeAddr, raw, err)
	}
	absClient = filepath.Clean(absClient)

	// Check current module's bundle tree first.
	if rel, ok := relativeWithinDir(moduleSourceDir, absClient); ok {
		bundleRel := joinBundleRel(modBase, rel)
		if err := n.requireBundleDir(nodeAddr, raw, bundleRel); err != nil {
			return "", err
		}
		return bundleRel, nil
	}

	// Check root bundle tree (e.g. child module referencing ../shared-skills inside config/).
	if rel, ok := relativeWithinDir(n.rootSourceDir, absClient); ok {
		bundleRel := joinBundleRel("config", rel)
		if err := n.requireBundleDir(nodeAddr, raw, bundleRel); err != nil {
			return "", err
		}
		return bundleRel, nil
	}

	// Check any active child module's bundle tree in deterministic order.
	modKeys := make([]string, 0, len(n.moduleSources))
	for k := range n.moduleSources {
		modKeys = append(modKeys, k)
	}
	sort.Strings(modKeys)
	for _, modBundle := range modKeys {
		if rel, ok := relativeWithinDir(n.moduleSources[modBundle], absClient); ok {
			bundleRel := joinBundleRel(modBundle, rel)
			if err := n.requireBundleDir(nodeAddr, raw, bundleRel); err != nil {
				return "", err
			}
			return bundleRel, nil
		}
	}

	// Escaped the bundle tree -> external local path.
	return n.collectExternalSkillDir(nodeAddr, raw, absClient)
}

func (n *skillNormalizer) requireBundleDir(nodeAddr, raw, bundleRel string) error {
	expected := "bundle/" + bundleRel
	if !n.bundleDirs[expected] {
		return fmt.Errorf("%s references missing skill directory %q (expected %q in archive bundle)", nodeAddr, raw, expected)
	}
	return nil
}

func (n *skillNormalizer) collectExternalSkillDir(nodeAddr, raw, absClient string) (string, error) {
	cleaned := filepath.Clean(absClient)
	linfo, err := os.Lstat(cleaned)
	if err != nil {
		return "", fmt.Errorf("%s skill_directories entry %q (%s) does not exist: %w", nodeAddr, raw, cleaned, err)
	}
	if linfo.Mode()&fs.ModeSymlink != 0 {
		return "", fmt.Errorf("%s skill_directories entry %q (%s) is a symlink, which is not allowed", nodeAddr, raw, cleaned)
	}
	if !linfo.IsDir() {
		return "", fmt.Errorf("%s skill_directories entry %q (%s) is not a directory", nodeAddr, raw, cleaned)
	}

	sum := sha256.Sum256([]byte(cleaned))
	hash := hex.EncodeToString(sum[:])[:16]
	bundleRel := "external_skills/" + hash

	if prev, exists := n.extCollected[hash]; exists {
		if prev != cleaned {
			return "", fmt.Errorf("%s skill_directories entry %q (%s) hash collision with %s", nodeAddr, raw, cleaned, prev)
		}
		return bundleRel, nil
	}

	bundlePrefix := "bundle/" + bundleRel
	walked, _, err := walkCleanBundleDir(cleaned, bundlePrefix, false, nil)
	if err != nil {
		return "", fmt.Errorf("%s skill_directories entry %q (%s): %w", nodeAddr, raw, cleaned, err)
	}
	n.extCollected[hash] = cleaned
	n.extEntries = append(n.extEntries, archiveEntry{
		name:  bundlePrefix + "/",
		isDir: true,
		mode:  archiveDirMode,
	})
	n.extEntries = append(n.extEntries, walked...)
	return bundleRel, nil
}

func relativeWithinDir(baseDir, targetPath string) (string, bool) {
	if baseDir == "" || targetPath == "" {
		return "", false
	}
	rel, err := filepath.Rel(baseDir, targetPath)
	if err != nil {
		if evalBase, errB := filepath.EvalSymlinks(baseDir); errB == nil {
			if evalTarget, errT := filepath.EvalSymlinks(targetPath); errT == nil {
				rel, err = filepath.Rel(evalBase, evalTarget)
			}
		}
	}
	if err != nil {
		return "", false
	}
	slashRel := filepath.ToSlash(rel)
	if slashRel == ".." || strings.HasPrefix(slashRel, "../") || filepath.IsAbs(rel) {
		return "", false
	}
	return slashRel, true
}

func joinBundleRel(base, rel string) string {
	if rel == "" || rel == "." {
		return base
	}
	return path.Clean(base + "/" + rel)
}
