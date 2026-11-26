package planner

import (
	"fmt"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/lonegunmanb/golden"
	"github.com/zclconf/go-cty/cty"
)

type SingletonStatus uint8

const (
	SingletonNone SingletonStatus = iota
	SingletonUnique
	SingletonAmbiguous
)

type InheritedSingleton struct {
	Status SingletonStatus
	Name   string
	Value  cty.Value
	Names  []string
	Origin string
}

type InheritedSingletons struct {
	Sandbox       InheritedSingleton
	Harness       InheritedSingleton
	ModelProvider InheritedSingleton
}

type RootDefaults struct {
	Sources        []string
	Sandboxes      []string
	Harnesses      []string
	ModelProviders []string
}

func InspectRootDefaults(blocks []*golden.HclBlock) RootDefaults {
	var defaults RootDefaults
	for _, block := range blocks {
		if block == nil || len(block.Labels) != 2 || block.Labels[0] != "" {
			continue
		}
		switch block.Type {
		case "source":
			defaults.Sources = append(defaults.Sources, block.Labels[1])
		case "sandbox":
			defaults.Sandboxes = append(defaults.Sandboxes, block.Labels[1])
		case "harness":
			defaults.Harnesses = append(defaults.Harnesses, block.Labels[1])
		case "model_provider":
			defaults.ModelProviders = append(defaults.ModelProviders, block.Labels[1])
		}
	}
	return defaults
}

func (d RootDefaults) ResolveDefaultSandbox(consumerAddr string) (string, error) {
	switch len(d.Sandboxes) {
	case 0:
		return "", fmt.Errorf("%s requires a sandbox, but the root module declares no sandbox block", consumerAddr)
	case 1:
		return d.Sandboxes[0], nil
	default:
		return "", fmt.Errorf(
			"%s encounters multiple sandbox blocks in the root module (%s); set sandbox explicitly",
			consumerAddr,
			strings.Join(d.Sandboxes, ", "),
		)
	}
}

func (d RootDefaults) ResolveDefaultHarness(consumerAddr string) (string, error) {
	switch len(d.Harnesses) {
	case 0:
		return "", fmt.Errorf("%s requires a harness, but the root module declares no harness block", consumerAddr)
	case 1:
		return d.Harnesses[0], nil
	default:
		return "", fmt.Errorf(
			"%s encounters multiple harness blocks in the root module (%s); set harness explicitly",
			consumerAddr,
			strings.Join(d.Harnesses, ", "),
		)
	}
}

func (d RootDefaults) ResolveDefaultModelProvider(consumerAddr string) (*string, error) {
	switch len(d.ModelProviders) {
	case 0:
		return nil, nil
	case 1:
		name := d.ModelProviders[0]
		return &name, nil
	default:
		return nil, fmt.Errorf(
			"%s encounters multiple model_provider blocks in the root module (%s); set model_provider explicitly",
			consumerAddr,
			strings.Join(d.ModelProviders, ", "),
		)
	}
}

func applyRootDefaults(blocks []*golden.HclBlock) ([]*golden.HclBlock, error) {
	result, _, err := applyModuleDefaults(blocks, "", "root module", InheritedSingletons{})
	return result, err
}

func applyModuleDefaults(
	blocks []*golden.HclBlock,
	modulePrefix string,
	moduleLabel string,
	inherited InheritedSingletons,
) ([]*golden.HclBlock, InheritedSingletons, error) {
	defaults := InspectRootDefaults(blocks)
	var usedInherited InheritedSingletons
	if len(defaults.Sandboxes) == 0 && inherited.Sandbox.Status == SingletonUnique {
		usedInherited.Sandbox = inherited.Sandbox
	}
	if len(defaults.Harnesses) == 0 && inherited.Harness.Status == SingletonUnique {
		usedInherited.Harness = inherited.Harness
	}
	if len(defaults.ModelProviders) == 0 && inherited.ModelProvider.Status == SingletonUnique {
		usedInherited.ModelProvider = inherited.ModelProvider
	}

	result := append([]*golden.HclBlock(nil), blocks...)
	type omittedPreserve struct {
		name       string
		blockIndex int
	}
	var preserves []omittedPreserve
	localSingletons := map[string]*golden.HclBlock{}

	for index, block := range blocks {
		if block == nil || len(block.Labels) != 2 || block.Labels[0] != "" {
			continue
		}
		switch block.Type {
		case "sandbox", "harness", "model_provider":
			localSingletons[block.Type+"."+block.Labels[1]] = block
		case "preserve":
			if _, explicitURL := block.Body.Attributes["url"]; !explicitURL {
				preserves = append(preserves, omittedPreserve{
					name:       block.Labels[1],
					blockIndex: index,
				})
			}
		}
	}
	if len(preserves) > 0 {
		switch len(defaults.Sources) {
		case 0:
			return nil, InheritedSingletons{}, fmt.Errorf(
				"preserve %q omits url, but no source block is declared in %s",
				preserves[0].name,
				moduleLabel,
			)
		case 1:
		default:
			return nil, InheritedSingletons{}, fmt.Errorf(
				"preserve %q omits url, but multiple source blocks are declared in %s (%s); set url explicitly",
				preserves[0].name,
				moduleLabel,
				strings.Join(defaults.Sources, ", "),
			)
		}
		sourceName := defaults.Sources[0]
		for _, preserve := range preserves {
			original := blocks[preserve.blockIndex]
			traversal := hcl.Traversal{
				hcl.TraverseRoot{Name: "source", SrcRange: original.TypeRange},
				hcl.TraverseAttr{Name: sourceName, SrcRange: original.TypeRange},
				hcl.TraverseAttr{Name: "url", SrcRange: original.TypeRange},
			}
			result[preserve.blockIndex] = cloneBlockWithTraversalAttribute(original, "url", traversal)
		}
	}

	resolver := moduleDefaultResolver{
		modulePrefix: modulePrefix,
		moduleLabel:  moduleLabel,
		local:        defaults,
		inherited:    inherited,
	}

	for index, block := range result {
		if block == nil || len(block.Labels) != 2 {
			continue
		}
		switch block.Type {
		case "worker":
			worker, err := injectWorkerModuleDefaults(block, resolver)
			if err != nil {
				return nil, InheritedSingletons{}, err
			}
			result[index] = worker
		case "dynamic_worker":
			dynWorker, err := injectDynamicWorkerModuleDefaults(block, resolver)
			if err != nil {
				return nil, InheritedSingletons{}, err
			}
			result[index] = dynWorker
		case "merge":
			mergeBlock, err := injectMergeModuleDefaults(block, resolver)
			if err != nil {
				return nil, InheritedSingletons{}, err
			}
			result[index] = mergeBlock
		case "module":
			result[index] = injectModuleSingletonDeps(block, defaults, localSingletons)
		}
	}
	return result, usedInherited, nil
}

type moduleDefaultResolver struct {
	modulePrefix string
	moduleLabel  string
	local        RootDefaults
	inherited    InheritedSingletons
}

func (r moduleDefaultResolver) canonicalAddr(localAddr string) string {
	if r.modulePrefix == "" {
		return localAddr
	}
	return r.modulePrefix + "." + localAddr
}

func resolveDimensionDefault(
	dimName string,
	localNames []string,
	inh InheritedSingleton,
	moduleLabel string,
	consumerCanonicalAddr string,
	required bool,
	allowNull bool,
) (string, error) {
	nullHint := ""
	if allowNull {
		nullHint = " or null"
	}
	switch len(localNames) {
	case 1:
		return localNames[0], nil
	case 0:
		switch inh.Status {
		case SingletonUnique:
			return inh.Name, nil
		case SingletonAmbiguous:
			return "", fmt.Errorf(
				"%s encounters multiple %s blocks inherited from %s (%s); set %s explicitly%s",
				consumerCanonicalAddr,
				dimName,
				inh.Origin,
				strings.Join(inh.Names, ", "),
				dimName,
				nullHint,
			)
		default:
			if !required {
				return "", nil
			}
			if moduleLabel == "root module" {
				return "", fmt.Errorf("%s requires a %s, but the root module declares no %s block", consumerCanonicalAddr, dimName, dimName)
			}
			return "", fmt.Errorf("%s requires a %s, but neither %s nor any ancestor module declares a %s block", consumerCanonicalAddr, dimName, moduleLabel, dimName)
		}
	default:
		return "", fmt.Errorf(
			"%s encounters multiple %s blocks in %s (%s); set %s explicitly%s",
			consumerCanonicalAddr,
			dimName,
			moduleLabel,
			strings.Join(localNames, ", "),
			dimName,
			nullHint,
		)
	}
}

func resolveMissingWorkerDefaultTraversals(
	attrs map[string]*hclsyntax.Attribute,
	consumerAddr string,
	posRange hcl.Range,
	resolver moduleDefaultResolver,
) (map[string]hcl.Traversal, error) {
	injections := map[string]hcl.Traversal{}
	canonical := resolver.canonicalAddr(consumerAddr)
	if _, explicit := attrs["sandbox"]; !explicit {
		name, err := resolveDimensionDefault("sandbox", resolver.local.Sandboxes, resolver.inherited.Sandbox, resolver.moduleLabel, canonical, true, false)
		if err != nil {
			return nil, err
		}
		injections["sandbox"] = hcl.Traversal{
			hcl.TraverseRoot{Name: "sandbox", SrcRange: posRange},
			hcl.TraverseAttr{Name: name, SrcRange: posRange},
		}
	}
	if _, explicit := attrs["harness"]; !explicit {
		name, err := resolveDimensionDefault("harness", resolver.local.Harnesses, resolver.inherited.Harness, resolver.moduleLabel, canonical, true, false)
		if err != nil {
			return nil, err
		}
		injections["harness"] = hcl.Traversal{
			hcl.TraverseRoot{Name: "harness", SrcRange: posRange},
			hcl.TraverseAttr{Name: name, SrcRange: posRange},
		}
	}
	if _, explicit := attrs["model_provider"]; !explicit {
		name, err := resolveDimensionDefault("model_provider", resolver.local.ModelProviders, resolver.inherited.ModelProvider, resolver.moduleLabel, canonical, false, true)
		if err != nil {
			return nil, err
		}
		if name != "" {
			injections["model_provider"] = hcl.Traversal{
				hcl.TraverseRoot{Name: "model_provider", SrcRange: posRange},
				hcl.TraverseAttr{Name: name, SrcRange: posRange},
			}
		}
	}
	return injections, nil
}

func injectWorkerModuleDefaults(block *golden.HclBlock, resolver moduleDefaultResolver) (*golden.HclBlock, error) {
	address := block.Labels[1]
	injections, err := resolveMissingWorkerDefaultTraversals(
		block.Body.Attributes,
		"worker."+address,
		block.TypeRange,
		resolver,
	)
	if err != nil {
		return nil, err
	}
	if len(injections) == 0 {
		return block, nil
	}
	return cloneBlockWithTraversalAttributes(block, injections), nil
}

func injectDynamicWorkerModuleDefaults(block *golden.HclBlock, resolver moduleDefaultResolver) (*golden.HclBlock, error) {
	dynIdx := -1
	dynCount := 0
	for i, nested := range block.NestedBlocks() {
		if nested.Type == "dynamic" && len(nested.Labels) == 1 && nested.Labels[0] == "worker" {
			dynIdx = i
			dynCount++
		}
	}
	if dynCount != 1 {
		return block, nil
	}
	dynBlock := block.NestedBlocks()[dynIdx]
	contentIdx := -1
	contentCount := 0
	for i, nested := range dynBlock.NestedBlocks() {
		if nested.Type == "content" && len(nested.Labels) == 0 {
			contentIdx = i
			contentCount++
		}
	}
	if contentCount != 1 {
		return block, nil
	}

	contentBlock := dynBlock.NestedBlocks()[contentIdx]
	injections, err := resolveMissingWorkerDefaultTraversals(
		contentBlock.Body.Attributes,
		"dynamic_worker."+block.Labels[1],
		contentBlock.TypeRange,
		resolver,
	)
	if err != nil {
		return nil, err
	}
	if len(injections) == 0 {
		return block, nil
	}

	syntaxBlock := golden.CloneHclSyntaxBlock(block.Block)
	syntaxContent := syntaxBlock.Body.Blocks[dynIdx].Body.Blocks[contentIdx]
	writeBlock := cloneWriteBlock(block)
	writeContent := writeBlock.Body().Blocks()[dynIdx].Body().Blocks()[contentIdx]
	for attrName, traversal := range injections {
		setSyntaxAndWriteTraversalAttr(syntaxContent, writeContent, attrName, traversal)
	}
	return golden.NewHclBlock(syntaxBlock, writeBlock, block.ForEach), nil
}

func injectMergeModuleDefaults(block *golden.HclBlock, resolver moduleDefaultResolver) (*golden.HclBlock, error) {
	if _, explicit := block.Body.Attributes["sandbox"]; explicit {
		return block, nil
	}
	hasGatekeeper := false
	for _, nested := range block.NestedBlocks() {
		if nested.Type == "gatekeeper" || (nested.Type == "dynamic" && len(nested.Labels) == 1 && nested.Labels[0] == "gatekeeper") {
			hasGatekeeper = true
			break
		}
	}
	if !hasGatekeeper {
		return block, nil
	}
	name, err := resolveDimensionDefault(
		"sandbox",
		resolver.local.Sandboxes,
		resolver.inherited.Sandbox,
		resolver.moduleLabel,
		resolver.canonicalAddr("merge."+block.Labels[1]),
		true,
		false,
	)
	if err != nil {
		return nil, err
	}
	return cloneBlockWithTraversalAttribute(block, "sandbox", hcl.Traversal{
		hcl.TraverseRoot{Name: "sandbox", SrcRange: block.TypeRange},
		hcl.TraverseAttr{Name: name, SrcRange: block.TypeRange},
	}), nil
}

func injectModuleSingletonDeps(
	block *golden.HclBlock,
	defaults RootDefaults,
	localSingletons map[string]*golden.HclBlock,
) *golden.HclBlock {
	var candidates []hcl.Traversal
	addCandidate := func(kind, name string) {
		addr := kind + "." + name
		if sb := localSingletons[addr]; sb != nil && blockReferencesModule(sb, block.Labels[1]) {
			return
		}
		candidates = append(candidates, hcl.Traversal{
			hcl.TraverseRoot{Name: kind, SrcRange: block.TypeRange},
			hcl.TraverseAttr{Name: name, SrcRange: block.TypeRange},
		})
	}
	if len(defaults.Sandboxes) == 1 {
		addCandidate("sandbox", defaults.Sandboxes[0])
	}
	if len(defaults.Harnesses) == 1 {
		addCandidate("harness", defaults.Harnesses[0])
	}
	if len(defaults.ModelProviders) == 1 {
		addCandidate("model_provider", defaults.ModelProviders[0])
	}
	if len(candidates) == 0 {
		return block
	}

	var existingExprs []hclsyntax.Expression
	existingNames := map[string]struct{}{}
	var existingTokens hclwrite.Tokens
	if depAttr, hasDep := block.Body.Attributes["depends_on"]; hasDep {
		tupleExpr, ok := depAttr.Expr.(*hclsyntax.TupleConsExpr)
		if !ok {
			return block
		}
		existingExprs = append(existingExprs, tupleExpr.Exprs...)
		for _, expr := range tupleExpr.Exprs {
			if st, ok := expr.(*hclsyntax.ScopeTraversalExpr); ok {
				existingNames[traversalAddr(st.Traversal)] = struct{}{}
			}
		}
		if wrapped, hasWrapped := block.Attributes()["depends_on"]; hasWrapped {
			existingTokens = trimTupleTokens(wrapped.ExprTokens())
		}
	}

	var toAdd []hcl.Traversal
	for _, tr := range candidates {
		addr := traversalAddr(tr)
		if _, exists := existingNames[addr]; !exists {
			toAdd = append(toAdd, tr)
			existingNames[addr] = struct{}{}
		}
	}
	if len(toAdd) == 0 {
		return block
	}

	allExprs := append([]hclsyntax.Expression(nil), existingExprs...)
	for _, tr := range toAdd {
		allExprs = append(allExprs, &hclsyntax.ScopeTraversalExpr{
			Traversal: tr,
			SrcRange:  block.TypeRange,
		})
	}

	tokens := hclwrite.Tokens{{Type: hclsyntax.TokenOBrack, Bytes: []byte("[")}}
	wroteAny := false
	if len(existingTokens) > 0 {
		tokens = append(tokens, existingTokens...)
		wroteAny = true
	}
	for _, tr := range toAdd {
		if wroteAny {
			tokens = append(tokens, &hclwrite.Token{Type: hclsyntax.TokenComma, Bytes: []byte(",")})
		}
		tokens = append(tokens, hclwrite.TokensForTraversal(tr)...)
		wroteAny = true
	}
	tokens = append(tokens, &hclwrite.Token{Type: hclsyntax.TokenCBrack, Bytes: []byte("]")})

	syntaxBlock := golden.CloneHclSyntaxBlock(block.Block)
	syntaxBlock.Body.Attributes["depends_on"] = &hclsyntax.Attribute{
		Name: "depends_on",
		Expr: &hclsyntax.TupleConsExpr{
			Exprs:     allExprs,
			SrcRange:  block.TypeRange,
			OpenRange: block.TypeRange,
		},
		SrcRange:    block.TypeRange,
		NameRange:   block.TypeRange,
		EqualsRange: block.TypeRange,
	}
	writeBlock := cloneWriteBlock(block)
	writeBlock.Body().SetAttributeRaw("depends_on", tokens)
	return golden.NewHclBlock(syntaxBlock, writeBlock, block.ForEach)
}

func blockReferencesModule(block *golden.HclBlock, moduleName string) bool {
	referenced := false
	walkPositiveBlockTraversals(block, false, func(traversal hcl.Traversal) {
		if len(traversal) >= 2 && traversal.RootName() == "module" {
			if attr, ok := traversal[1].(hcl.TraverseAttr); ok && attr.Name == moduleName {
				referenced = true
			}
		}
	})
	return referenced
}

func traversalAddr(tr hcl.Traversal) string {
	var parts []string
	for _, step := range tr {
		switch s := step.(type) {
		case hcl.TraverseRoot:
			parts = append(parts, s.Name)
		case hcl.TraverseAttr:
			parts = append(parts, s.Name)
		}
	}
	return strings.Join(parts, ".")
}

func trimTupleTokens(tokens hclwrite.Tokens) hclwrite.Tokens {
	start := -1
	end := -1
	for i, tok := range tokens {
		if tok.Type == hclsyntax.TokenOBrack {
			start = i + 1
			break
		}
	}
	for i := len(tokens) - 1; i >= 0; i-- {
		if tokens[i].Type == hclsyntax.TokenCBrack {
			end = i
			break
		}
	}
	if start < 0 || end < start {
		return nil
	}
	inner := tokens[start:end]
	hasNonTrivia := false
	for _, tok := range inner {
		if tok.Type != hclsyntax.TokenNewline && tok.Type != hclsyntax.TokenComment {
			hasNonTrivia = true
			break
		}
	}
	if !hasNonTrivia {
		return nil
	}
	return inner
}

func cloneBlockWithTraversalAttributes(
	block *golden.HclBlock,
	injections map[string]hcl.Traversal,
) *golden.HclBlock {
	syntaxBlock := golden.CloneHclSyntaxBlock(block.Block)
	writeBlock := cloneWriteBlock(block)
	for attrName, traversal := range injections {
		setSyntaxAndWriteTraversalAttr(syntaxBlock, writeBlock, attrName, traversal)
	}
	return golden.NewHclBlock(syntaxBlock, writeBlock, block.ForEach)
}

func setSyntaxAndWriteTraversalAttr(
	syntaxBlock *hclsyntax.Block,
	writeBlock *hclwrite.Block,
	attributeName string,
	traversal hcl.Traversal,
) {
	syntaxBlock.Body.Attributes[attributeName] = &hclsyntax.Attribute{
		Name: attributeName,
		Expr: &hclsyntax.ScopeTraversalExpr{
			Traversal: traversal,
			SrcRange:  syntaxBlock.TypeRange,
		},
		SrcRange:    syntaxBlock.TypeRange,
		NameRange:   syntaxBlock.TypeRange,
		EqualsRange: syntaxBlock.TypeRange,
	}
	writeBlock.Body().SetAttributeRaw(attributeName, hclwrite.TokensForTraversal(traversal))
}

func cloneBlockWithTraversalAttribute(
	block *golden.HclBlock,
	attributeName string,
	traversal hcl.Traversal,
) *golden.HclBlock {
	return cloneBlockWithTraversalAttributes(block, map[string]hcl.Traversal{attributeName: traversal})
}

func cloneWriteBlock(block *golden.HclBlock) *hclwrite.Block {
	labels := append([]string(nil), block.Block.Labels...)
	if len(labels) > 0 && labels[0] == "" {
		labels = labels[1:]
	}
	writeBlock := hclwrite.NewBlock(block.Block.Type, labels)
	for name, attribute := range block.Attributes() {
		writeBlock.Body().SetAttributeRaw(name, attribute.ExprTokens())
	}
	for _, nested := range block.NestedBlocks() {
		writeBlock.Body().AppendBlock(cloneWriteBlock(nested))
	}
	return writeBlock
}
