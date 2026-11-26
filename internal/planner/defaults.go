package planner

import (
	"fmt"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/lonegunmanb/golden"
)

type SingletonStatus uint8

const (
	SingletonNone SingletonStatus = iota
	SingletonUnique
	SingletonAmbiguous
)

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
	defaults := InspectRootDefaults(blocks)
	result := append([]*golden.HclBlock(nil), blocks...)
	type omittedPreserve struct {
		name       string
		blockIndex int
	}
	var preserves []omittedPreserve

	for index, block := range blocks {
		if block == nil || len(block.Labels) != 2 || block.Labels[0] != "" {
			continue
		}
		switch block.Type {
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
			return nil, fmt.Errorf(
				"preserve %q omits url, but no source block is declared in root module",
				preserves[0].name,
			)
		case 1:
		default:
			return nil, fmt.Errorf(
				"preserve %q omits url, but multiple source blocks are declared in root module (%s); set url explicitly",
				preserves[0].name,
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

	for index, block := range result {
		if block == nil || len(block.Labels) != 2 {
			continue
		}
		switch block.Type {
		case "worker":
			worker, err := injectWorkerRootDefaults(block, defaults)
			if err != nil {
				return nil, err
			}
			result[index] = worker
		case "dynamic_worker":
			dynWorker, err := injectDynamicWorkerRootDefaults(block, defaults)
			if err != nil {
				return nil, err
			}
			result[index] = dynWorker
		case "merge":
			mergeBlock, err := injectMergeRootDefaults(block, defaults)
			if err != nil {
				return nil, err
			}
			result[index] = mergeBlock
		}
	}
	return result, nil
}

func resolveMissingWorkerDefaultTraversals(
	attrs map[string]*hclsyntax.Attribute,
	consumerAddr string,
	posRange hcl.Range,
	defaults RootDefaults,
) (map[string]hcl.Traversal, error) {
	injections := map[string]hcl.Traversal{}
	if _, explicit := attrs["sandbox"]; !explicit {
		name, err := defaults.ResolveDefaultSandbox(consumerAddr)
		if err != nil {
			return nil, err
		}
		injections["sandbox"] = hcl.Traversal{
			hcl.TraverseRoot{Name: "sandbox", SrcRange: posRange},
			hcl.TraverseAttr{Name: name, SrcRange: posRange},
		}
	}
	if _, explicit := attrs["harness"]; !explicit {
		name, err := defaults.ResolveDefaultHarness(consumerAddr)
		if err != nil {
			return nil, err
		}
		injections["harness"] = hcl.Traversal{
			hcl.TraverseRoot{Name: "harness", SrcRange: posRange},
			hcl.TraverseAttr{Name: name, SrcRange: posRange},
		}
	}
	if _, explicit := attrs["model_provider"]; !explicit {
		name, err := defaults.ResolveDefaultModelProvider(consumerAddr)
		if err != nil {
			return nil, err
		}
		if name != nil {
			injections["model_provider"] = hcl.Traversal{
				hcl.TraverseRoot{Name: "model_provider", SrcRange: posRange},
				hcl.TraverseAttr{Name: *name, SrcRange: posRange},
			}
		}
	}
	return injections, nil
}

func injectWorkerRootDefaults(block *golden.HclBlock, defaults RootDefaults) (*golden.HclBlock, error) {
	address := block.Labels[1]
	injections, err := resolveMissingWorkerDefaultTraversals(
		block.Body.Attributes,
		"worker."+address,
		block.TypeRange,
		defaults,
	)
	if err != nil {
		return nil, err
	}
	if len(injections) == 0 {
		return block, nil
	}
	return cloneBlockWithTraversalAttributes(block, injections), nil
}

func injectDynamicWorkerRootDefaults(block *golden.HclBlock, defaults RootDefaults) (*golden.HclBlock, error) {
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
		defaults,
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

func injectMergeRootDefaults(block *golden.HclBlock, defaults RootDefaults) (*golden.HclBlock, error) {
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
	name, err := defaults.ResolveDefaultSandbox("merge." + block.Labels[1])
	if err != nil {
		return nil, err
	}
	return cloneBlockWithTraversalAttribute(block, "sandbox", hcl.Traversal{
		hcl.TraverseRoot{Name: "sandbox", SrcRange: block.TypeRange},
		hcl.TraverseAttr{Name: name, SrcRange: block.TypeRange},
	}), nil
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
