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
		if block == nil || block.Type != "worker" || len(block.Labels) != 2 {
			continue
		}
		worker, err := injectWorkerRootDefaults(block, defaults)
		if err != nil {
			return nil, err
		}
		result[index] = worker
	}
	return result, nil
}

func injectWorkerRootDefaults(block *golden.HclBlock, defaults RootDefaults) (*golden.HclBlock, error) {
	address := block.Labels[1]
	result := block
	if _, explicit := result.Body.Attributes["sandbox"]; !explicit {
		name, err := defaults.ResolveDefaultSandbox("worker." + address)
		if err != nil {
			return nil, err
		}
		result = cloneBlockWithTraversalAttribute(result, "sandbox", hcl.Traversal{
			hcl.TraverseRoot{Name: "sandbox", SrcRange: result.TypeRange},
			hcl.TraverseAttr{Name: name, SrcRange: result.TypeRange},
		})
	}
	if _, explicit := result.Body.Attributes["harness"]; !explicit {
		name, err := defaults.ResolveDefaultHarness("worker." + address)
		if err != nil {
			return nil, err
		}
		result = cloneBlockWithTraversalAttribute(result, "harness", hcl.Traversal{
			hcl.TraverseRoot{Name: "harness", SrcRange: result.TypeRange},
			hcl.TraverseAttr{Name: name, SrcRange: result.TypeRange},
		})
	}
	if _, explicit := result.Body.Attributes["model_provider"]; !explicit {
		name, err := defaults.ResolveDefaultModelProvider("worker." + address)
		if err != nil {
			return nil, err
		}
		if name != nil {
			result = cloneBlockWithTraversalAttribute(result, "model_provider", hcl.Traversal{
				hcl.TraverseRoot{Name: "model_provider", SrcRange: result.TypeRange},
				hcl.TraverseAttr{Name: *name, SrcRange: result.TypeRange},
			})
		}
	}
	return result, nil
}

func cloneBlockWithTraversalAttribute(
	block *golden.HclBlock,
	attributeName string,
	traversal hcl.Traversal,
) *golden.HclBlock {
	syntaxBlock := golden.CloneHclSyntaxBlock(block.Block)
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

	writeBlock := cloneWriteBlock(block)
	writeBlock.Body().SetAttributeRaw(attributeName, hclwrite.TokensForTraversal(traversal))
	return golden.NewHclBlock(syntaxBlock, writeBlock, block.ForEach)
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
