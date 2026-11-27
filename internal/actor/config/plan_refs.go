package config

import (
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"

	"github.com/lonegunmanb/sofa/internal/plan"
	"github.com/zclconf/go-cty/cty"
)

// ExtractPlanEnvRefs recursively scans the root Plan and all nested child Module Plans
// and returns a sorted, deduplicated slice of all referenced host environment/secret names.
func ExtractPlanEnvRefs(p *plan.Plan) []string {
	if p == nil {
		return nil
	}
	refs := make(map[string]struct{})
	collectPlanRefsRecursive(p, refs)
	return sortedKeys(refs)
}

// ExtractNodeEnvRefs extracts the minimal sorted, deduplicated slice of host environment/secret
// variable names required to dispatch the target node (by root address, canonical module path,
// or dynamic_worker expanded instance address).
func ExtractNodeEnvRefs(p *plan.Plan, nodeAddr string) ([]string, error) {
	if p == nil {
		return nil, fmt.Errorf("plan is nil")
	}
	target := strings.TrimSpace(nodeAddr)
	if target == "" {
		return nil, fmt.Errorf("node address is required")
	}

	idx := buildPlanIndex(p)
	entry, ok := idx.lookup(target)
	if !ok {
		return nil, fmt.Errorf("plan node %q not found", nodeAddr)
	}

	refs := make(map[string]struct{})
	idx.collectNodeRefs(entry, refs)
	return sortedKeys(refs), nil
}

func collectPlanRefsRecursive(p *plan.Plan, dst map[string]struct{}) {
	if p == nil {
		return
	}
	for _, node := range p.Nodes() {
		extractResourceNodeRefs(node, dst)
		if node.Kind == "module" && node.Module != nil && node.Module.Plan != nil {
			collectPlanRefsRecursive(node.Module.Plan, dst)
		}
	}
}

type indexedNode struct {
	node          plan.NodeSpec
	canonicalAddr string
	modulePlan    *plan.Plan
	modulePrefix  string
}

type planIndex struct {
	rootPlan        *plan.Plan
	byCanonical     map[string]indexedNode
	byLocalInPlan   map[*plan.Plan]map[string]indexedNode
	sourcesByPlan   map[*plan.Plan][]indexedNode
	sourcesByURL    map[string][]indexedNode
	allSources      []indexedNode
	mcpByToolID     map[string]indexedNode
	mcpByResourceID map[string]indexedNode
}

func buildPlanIndex(root *plan.Plan) *planIndex {
	idx := &planIndex{
		rootPlan:        root,
		byCanonical:     make(map[string]indexedNode),
		byLocalInPlan:   make(map[*plan.Plan]map[string]indexedNode),
		sourcesByPlan:   make(map[*plan.Plan][]indexedNode),
		sourcesByURL:    make(map[string][]indexedNode),
		mcpByToolID:     make(map[string]indexedNode),
		mcpByResourceID: make(map[string]indexedNode),
	}
	idx.walk(root, "")
	return idx
}

func (idx *planIndex) walk(p *plan.Plan, modulePrefix string) {
	if p == nil {
		return
	}
	localMap := make(map[string]indexedNode)
	idx.byLocalInPlan[p] = localMap

	for _, node := range p.Nodes() {
		canon := strings.TrimSpace(node.CanonicalAddress)
		if canon == "" {
			if modulePrefix == "" {
				canon = node.Address
			} else {
				canon = modulePrefix + "." + node.Address
			}
		}
		entry := indexedNode{
			node:          node,
			canonicalAddr: canon,
			modulePlan:    p,
			modulePrefix:  modulePrefix,
		}
		idx.byCanonical[canon] = entry
		localMap[node.Address] = entry

		switch node.Kind {
		case "source":
			idx.allSources = append(idx.allSources, entry)
			idx.sourcesByPlan[p] = append(idx.sourcesByPlan[p], entry)
			if srcURL, ok := ctyStringAttr(node.Config, "url"); ok {
				idx.sourcesByURL[srcURL] = append(idx.sourcesByURL[srcURL], entry)
			}
		case "mcp_server":
			idx.indexMCPServer(entry)
		case "module":
			if node.Module != nil && node.Module.Plan != nil {
				childPrefix := canon
				if node.Module.CanonicalAddress != "" {
					childPrefix = node.Module.CanonicalAddress
				}
				idx.walk(node.Module.Plan, childPrefix)
			}
		}
	}
}

func (idx *planIndex) indexMCPServer(entry indexedNode) {
	srvName, _ := ctyStringAttr(entry.node.Config, "name")
	runtimeName, ok := ctyStringAttr(entry.node.Config, "runtime_name")
	if !ok {
		runtimeName = entry.canonicalAddr
	}
	if srvName == "" {
		return
	}
	for _, tool := range ctyStringSliceAttr(entry.node.Config, "tools") {
		id := mcpToolIDFor(runtimeName, srvName, tool)
		idx.mcpByToolID[id] = entry
	}
	for _, res := range ctyStringSliceAttr(entry.node.Config, "resources") {
		id := mcpResourceIDFor(runtimeName, srvName, res)
		idx.mcpByResourceID[id] = entry
	}
}

func (idx *planIndex) lookup(addr string) (indexedNode, bool) {
	if entry, ok := idx.byCanonical[addr]; ok {
		return entry, true
	}
	if localMap := idx.byLocalInPlan[idx.rootPlan]; localMap != nil {
		if entry, ok := localMap[addr]; ok {
			return entry, true
		}
	}
	// Support expanded dynamic_worker instance addresses such as:
	// dynamic_worker.verify["key"] or module.child.dynamic_worker.verify[key]
	if bracket := strings.LastIndexByte(addr, '['); bracket > 0 && strings.HasSuffix(addr, "]") {
		base := addr[:bracket]
		if entry, ok := idx.byCanonical[base]; ok && entry.node.Kind == "dynamic_worker" {
			return entry, true
		}
		if localMap := idx.byLocalInPlan[idx.rootPlan]; localMap != nil {
			if entry, ok := localMap[base]; ok && entry.node.Kind == "dynamic_worker" {
				return entry, true
			}
		}
	}
	return indexedNode{}, false
}

func (idx *planIndex) collectNodeRefs(entry indexedNode, dst map[string]struct{}) {
	node := entry.node
	switch node.Kind {
	case "source", "model_provider", "harness", "mcp_server", "external_tool":
		extractResourceNodeRefs(node, dst)
	case "worker":
		idx.collectWorkerRefs(entry, node.Config, dst)
	case "dynamic_worker":
		if contentCfg, ok := ctyObjectAttr(node.Config, "content"); ok {
			idx.collectWorkerRefs(entry, contentCfg, dst)
		}
	case "merge":
		idx.collectSourceRefsForNode(entry, node.Config, dst)
	case "module":
		if node.Module != nil && node.Module.Plan != nil {
			for _, childNode := range node.Module.Plan.Nodes() {
				if childEntry, ok := idx.byLocalInPlan[node.Module.Plan][childNode.Address]; ok {
					idx.collectNodeRefs(childEntry, dst)
				}
			}
		}
	}
}

func (idx *planIndex) collectWorkerRefs(entry indexedNode, workerCfg cty.Value, dst map[string]struct{}) {
	hasExplicitResourceCfg := false

	if harnessObj, ok := ctyObjectAttr(workerCfg, "harness"); ok {
		if addr, ok := ctyStringAttr(harnessObj, "address"); ok {
			hasExplicitResourceCfg = true
			if target, exists := idx.resolveRefAddr(entry, addr); exists {
				extractResourceNodeRefs(target.node, dst)
			}
		}
	}
	if mpObj, ok := ctyObjectAttr(workerCfg, "model_provider"); ok {
		if addr, ok := ctyStringAttr(mpObj, "address"); ok {
			hasExplicitResourceCfg = true
			if target, exists := idx.resolveRefAddr(entry, addr); exists {
				extractResourceNodeRefs(target.node, dst)
			}
		}
	}
	if qcObj, ok := ctyObjectAttr(workerCfg, "qc"); ok {
		hasExplicitResourceCfg = true
		if qcHarness, ok := ctyObjectAttr(qcObj, "harness"); ok {
			if addr, ok := ctyStringAttr(qcHarness, "address"); ok {
				if target, exists := idx.resolveRefAddr(entry, addr); exists {
					extractResourceNodeRefs(target.node, dst)
				}
			}
		}
		if qcMP, ok := ctyObjectAttr(qcObj, "model_provider"); ok {
			if addr, ok := ctyStringAttr(qcMP, "address"); ok {
				if target, exists := idx.resolveRefAddr(entry, addr); exists {
					extractResourceNodeRefs(target.node, dst)
				}
			}
		}
	}

	if _, hasURL := ctyStringAttr(workerCfg, "source_url"); hasURL || ctyHasNonNullAttr(workerCfg, "based_source") {
		hasExplicitResourceCfg = true
		idx.collectSourceRefsForNode(entry, workerCfg, dst)
	}

	mcpIDs := append(ctyStringSliceAttr(workerCfg, "mcp_tool_ids"), ctyStringSliceAttr(workerCfg, "mcp_resource_ids")...)
	if len(mcpIDs) > 0 {
		hasExplicitResourceCfg = true
		for _, id := range mcpIDs {
			if srv, ok := idx.mcpByToolID[id]; ok {
				extractResourceNodeRefs(srv.node, dst)
			} else if srv, ok := idx.mcpByResourceID[id]; ok {
				extractResourceNodeRefs(srv.node, dst)
			}
		}
	}

	toolIDs := collectWorkerToolIDs(workerCfg)
	if len(toolIDs) > 0 {
		hasExplicitResourceCfg = true
		for _, toolID := range toolIDs {
			if ts, ok := idx.lookupToolSpec(entry.modulePlan, toolID); ok {
				if target, exists := idx.resolveToolNode(entry, ts); exists {
					extractResourceNodeRefs(target.node, dst)
				}
			}
		}
	}

	// Fallback for minimal test plans where Config omits explicit resource references
	// and wires dependencies directly via NodeSpec.Dependencies.
	if !hasExplicitResourceCfg {
		localMap := idx.byLocalInPlan[entry.modulePlan]
		for _, depAddr := range entry.node.Dependencies {
			if depEntry, ok := localMap[depAddr]; ok {
				switch depEntry.node.Kind {
				case "source", "model_provider", "harness", "mcp_server", "external_tool":
					extractResourceNodeRefs(depEntry.node, dst)
				}
			}
		}
	}
}

func (idx *planIndex) collectSourceRefsForNode(entry indexedNode, cfg cty.Value, dst map[string]struct{}) {
	sourceURL, hasURL := ctyStringAttr(cfg, "source_url")
	localMap := idx.byLocalInPlan[entry.modulePlan]

	matchedDirect := false
	for _, depAddr := range entry.node.Dependencies {
		depEntry, ok := localMap[depAddr]
		if !ok || depEntry.node.Kind != "source" {
			continue
		}
		if hasURL {
			depURL, _ := ctyStringAttr(depEntry.node.Config, "url")
			if depURL != sourceURL {
				continue
			}
		}
		extractResourceNodeRefs(depEntry.node, dst)
		matchedDirect = true
	}

	if !matchedDirect {
		if hasURL {
			for _, srcEntry := range idx.sourcesByURL[sourceURL] {
				extractResourceNodeRefs(srcEntry.node, dst)
			}
			return
		}
		// When source_url is empty (e.g. Apply-time unknown based_source), fall back to
		// candidate sources declared in the node's module, or across the plan tree if the
		// child module declares no source blocks of its own.
		if modSources := idx.sourcesByPlan[entry.modulePlan]; len(modSources) > 0 {
			for _, srcEntry := range modSources {
				extractResourceNodeRefs(srcEntry.node, dst)
			}
			return
		}
		for _, srcEntry := range idx.allSources {
			extractResourceNodeRefs(srcEntry.node, dst)
		}
	}
}

func (idx *planIndex) resolveRefAddr(entry indexedNode, addr string) (indexedNode, bool) {
	if target, ok := idx.byCanonical[addr]; ok {
		return target, true
	}
	if localMap := idx.byLocalInPlan[entry.modulePlan]; localMap != nil {
		if target, ok := localMap[addr]; ok {
			return target, true
		}
	}
	if entry.modulePrefix != "" {
		if target, ok := idx.byCanonical[entry.modulePrefix+"."+addr]; ok {
			return target, true
		}
	}
	return indexedNode{}, false
}

func (idx *planIndex) lookupToolSpec(modulePlan *plan.Plan, toolID string) (plan.ToolSpec, bool) {
	if modulePlan != nil {
		if ts, ok := modulePlan.Tools()[toolID]; ok {
			return ts, true
		}
	}
	if idx.rootPlan != nil {
		if ts, ok := idx.rootPlan.Tools()[toolID]; ok {
			return ts, true
		}
	}
	return plan.ToolSpec{}, false
}

func (idx *planIndex) resolveToolNode(entry indexedNode, ts plan.ToolSpec) (indexedNode, bool) {
	if ts.CanonicalAddress != "" {
		if target, ok := idx.byCanonical[ts.CanonicalAddress]; ok {
			return target, true
		}
	}
	if ts.Address != "" {
		if target, ok := idx.resolveRefAddr(entry, ts.Address); ok {
			return target, true
		}
	}
	return indexedNode{}, false
}

func collectWorkerToolIDs(workerCfg cty.Value) []string {
	var ids []string
	ids = append(ids, ctyStringSliceAttr(workerCfg, "tool_ids")...)
	if termID, ok := ctyStringAttr(workerCfg, "terminate_tool_id"); ok {
		ids = append(ids, termID)
	}
	if toolUsesVal, ok := ctySequenceAttr(workerCfg, "tool_uses"); ok {
		for it := toolUsesVal.ElementIterator(); it.Next(); {
			_, item := it.Element()
			if id, ok := ctyStringAttr(item, "tool_id"); ok {
				ids = append(ids, id)
			}
		}
	}
	if qcObj, ok := ctyObjectAttr(workerCfg, "qc"); ok {
		ids = append(ids, ctyStringSliceAttr(qcObj, "tool_ids")...)
	}
	return ids
}

func extractResourceNodeRefs(node plan.NodeSpec, dst map[string]struct{}) {
	cfg := node.Config
	switch node.Kind {
	case "source":
		addStringAttrRef(cfg, "token_ref", dst)
		addStringAttrRef(cfg, "ssh_key_ref", dst)
	case "model_provider":
		addStringAttrRef(cfg, "api_key_ref", dst)
		addStringAttrRef(cfg, "bearer_token_ref", dst)
	case "harness":
		collectEnvRefsMapAttr(cfg, "env_refs", dst)
	case "mcp_server":
		if httpObj, ok := ctyObjectAttr(cfg, "http"); ok {
			addStringAttrRef(httpObj, "bearer_token_ref", dst)
		}
		if stdioObj, ok := ctyObjectAttr(cfg, "stdio"); ok {
			collectEnvRefsMapAttr(stdioObj, "env_refs", dst)
		}
	case "external_tool":
		collectEnvRefsMapAttr(cfg, "env_refs", dst)
	}
}

func addStringAttrRef(obj cty.Value, attr string, dst map[string]struct{}) {
	if v, ok := ctyStringAttr(obj, attr); ok {
		dst[v] = struct{}{}
	}
}

func collectEnvRefsMapAttr(obj cty.Value, attr string, dst map[string]struct{}) {
	m, ok := ctyObjectAttr(obj, attr)
	if !ok {
		return
	}
	for it := m.ElementIterator(); it.Next(); {
		_, elem := it.Element()
		unmarked, _ := elem.UnmarkDeep()
		if unmarked == cty.NilVal || unmarked.IsNull() || !unmarked.IsKnown() || !unmarked.Type().Equals(cty.String) {
			continue
		}
		if val := strings.TrimSpace(unmarked.AsString()); val != "" {
			dst[val] = struct{}{}
		}
	}
}

func unmarkKnownValue(val cty.Value) (cty.Value, bool) {
	if val == cty.NilVal {
		return cty.NilVal, false
	}
	unmarked, _ := val.UnmarkDeep()
	if unmarked == cty.NilVal || unmarked.IsNull() || !unmarked.IsKnown() {
		return cty.NilVal, false
	}
	return unmarked, true
}

func ctyObjectAttr(obj cty.Value, attr string) (cty.Value, bool) {
	unmarked, ok := unmarkKnownValue(obj)
	if !ok {
		return cty.NilVal, false
	}
	if unmarked.Type().IsObjectType() {
		if !unmarked.Type().HasAttribute(attr) {
			return cty.NilVal, false
		}
		v, ok := unmarkKnownValue(unmarked.GetAttr(attr))
		if !ok || (!v.Type().IsObjectType() && !v.Type().IsMapType()) {
			return cty.NilVal, false
		}
		return v, true
	}
	if unmarked.Type().IsMapType() {
		if !unmarked.HasIndex(cty.StringVal(attr)).True() {
			return cty.NilVal, false
		}
		v, ok := unmarkKnownValue(unmarked.Index(cty.StringVal(attr)))
		if !ok || (!v.Type().IsObjectType() && !v.Type().IsMapType()) {
			return cty.NilVal, false
		}
		return v, true
	}
	return cty.NilVal, false
}

func ctyHasNonNullAttr(obj cty.Value, attr string) bool {
	unmarked, ok := unmarkKnownValue(obj)
	if !ok {
		return false
	}
	var raw cty.Value
	switch {
	case unmarked.Type().IsObjectType():
		if !unmarked.Type().HasAttribute(attr) {
			return false
		}
		raw = unmarked.GetAttr(attr)
	case unmarked.Type().IsMapType():
		has := unmarked.HasIndex(cty.StringVal(attr))
		if !has.IsKnown() || has.IsNull() || !has.True() {
			return false
		}
		raw = unmarked.Index(cty.StringVal(attr))
	default:
		return false
	}
	if raw == cty.NilVal {
		return false
	}
	val, _ := raw.UnmarkDeep()
	return val != cty.NilVal && !val.IsNull()
}

func ctyStringAttr(obj cty.Value, attr string) (string, bool) {
	unmarked, ok := unmarkKnownValue(obj)
	if !ok {
		return "", false
	}
	var raw cty.Value
	switch {
	case unmarked.Type().IsObjectType():
		if !unmarked.Type().HasAttribute(attr) {
			return "", false
		}
		raw = unmarked.GetAttr(attr)
	case unmarked.Type().IsMapType():
		if !unmarked.HasIndex(cty.StringVal(attr)).True() {
			return "", false
		}
		raw = unmarked.Index(cty.StringVal(attr))
	default:
		return "", false
	}
	v, ok := unmarkKnownValue(raw)
	if !ok || !v.Type().Equals(cty.String) {
		return "", false
	}
	s := strings.TrimSpace(v.AsString())
	if s == "" {
		return "", false
	}
	return s, true
}

func ctySequenceAttr(obj cty.Value, attr string) (cty.Value, bool) {
	unmarked, ok := unmarkKnownValue(obj)
	if !ok || !unmarked.Type().IsObjectType() || !unmarked.Type().HasAttribute(attr) {
		return cty.NilVal, false
	}
	seq, ok := unmarkKnownValue(unmarked.GetAttr(attr))
	if !ok {
		return cty.NilVal, false
	}
	t := seq.Type()
	if !t.IsListType() && !t.IsTupleType() && !t.IsSetType() {
		return cty.NilVal, false
	}
	return seq, true
}

func ctyStringSliceAttr(obj cty.Value, attr string) []string {
	seq, ok := ctySequenceAttr(obj, attr)
	if !ok {
		return nil
	}
	out := make([]string, 0, seq.LengthInt())
	for it := seq.ElementIterator(); it.Next(); {
		_, elem := it.Element()
		v, ok := unmarkKnownValue(elem)
		if !ok || !v.Type().Equals(cty.String) {
			continue
		}
		if s := strings.TrimSpace(v.AsString()); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func sortedKeys(m map[string]struct{}) []string {
	if len(m) == 0 {
		return []string{}
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Mirrors internal/mcp/spec deterministic ID derivation without modifying mcp/spec.
func mcpToolIDFor(canonical, name, tool string) string {
	digest := sha256.Sum256([]byte("sofa/mcp-tool-id/v1\x00" + canonical + "\x00" + tool))
	return "mcp_tool_" + mcpIDPart(name) + "__" + mcpIDPart(tool) + "_" + mcpUUIDFromDigest(digest)
}

func mcpResourceIDFor(canonical, name, resource string) string {
	digest := sha256.Sum256([]byte("sofa/mcp-resource-id/v1\x00" + canonical + "\x00" + resource))
	return "mcp_resource_" + mcpIDPart(name) + "__" + mcpIDPart(resource) + "_" + mcpUUIDFromDigest(digest)
}

func mcpUUIDFromDigest(digest [sha256.Size]byte) string {
	digest[6] = (digest[6] & 0x0f) | 0x80
	digest[8] = (digest[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", digest[0:4], digest[4:6], digest[6:8], digest[8:10], digest[10:16])
}

func mcpIDPart(value string) string {
	var result strings.Builder
	for _, ch := range value {
		switch {
		case ch >= 'a' && ch <= 'z',
			ch >= 'A' && ch <= 'Z',
			ch >= '0' && ch <= '9',
			ch == '_', ch == '-':
			result.WriteRune(ch)
		default:
			result.WriteByte('_')
		}
	}
	if result.Len() == 0 {
		return "tool"
	}
	return result.String()
}
