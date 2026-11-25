package planner

import (
	"fmt"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/lonegunmanb/golden"
)

type rootSourceDeclaration struct {
	name string
}

func applyRootDefaults(blocks []*golden.HclBlock) ([]*golden.HclBlock, error) {
	var sources []rootSourceDeclaration
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
		case "source":
			sources = append(sources, rootSourceDeclaration{name: block.Labels[1]})
		case "preserve":
			if _, explicitURL := block.Body.Attributes["url"]; !explicitURL {
				preserves = append(preserves, omittedPreserve{
					name:       block.Labels[1],
					blockIndex: index,
				})
			}
		}
	}
	if len(preserves) == 0 {
		return blocks, nil
	}

	switch len(sources) {
	case 0:
		return nil, fmt.Errorf(
			"preserve %q omits url, but no source block is declared in root module",
			preserves[0].name,
		)
	case 1:
	default:
		names := make([]string, 0, len(sources))
		for _, source := range sources {
			names = append(names, source.name)
		}
		return nil, fmt.Errorf(
			"preserve %q omits url, but multiple source blocks are declared in root module (%s); set url explicitly",
			preserves[0].name,
			strings.Join(names, ", "),
		)
	}

	result := append([]*golden.HclBlock(nil), blocks...)
	sourceName := sources[0].name
	for _, preserve := range preserves {
		original := blocks[preserve.blockIndex]
		traversal := hcl.Traversal{
			hcl.TraverseRoot{Name: "source", SrcRange: original.TypeRange},
			hcl.TraverseAttr{Name: sourceName, SrcRange: original.TypeRange},
			hcl.TraverseAttr{Name: "url", SrcRange: original.TypeRange},
		}
		result[preserve.blockIndex] = cloneBlockWithTraversalAttribute(original, "url", traversal)
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
