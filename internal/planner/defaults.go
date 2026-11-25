package planner

import (
	"fmt"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/lonegunmanb/sofa/internal/config"
)

type rootSourceDeclaration struct {
	name string
}

type omittedPreserveDeclaration struct {
	name        string
	syntaxBlock *hclsyntax.Block
	writeBlock  *hclwrite.Block
}

func applyRootDefaults(root *config.RootModule) error {
	var sources []rootSourceDeclaration
	var preserves []omittedPreserveDeclaration
	for _, file := range root.Files {
		writeBlocks := file.Write.Blocks()
		for index, block := range file.Syntax.Blocks {
			if len(block.Labels) != 1 {
				continue
			}
			switch block.Type {
			case "source":
				sources = append(sources, rootSourceDeclaration{name: block.Labels[0]})
			case "preserve":
				if _, explicitURL := block.Body.Attributes["url"]; explicitURL {
					continue
				}
				preserves = append(preserves, omittedPreserveDeclaration{
					name:        block.Labels[0],
					syntaxBlock: block,
					writeBlock:  writeBlocks[index],
				})
			}
		}
	}
	if len(preserves) == 0 {
		return nil
	}

	switch len(sources) {
	case 0:
		return fmt.Errorf(
			"preserve %q omits url, but no source block is declared in root module",
			preserves[0].name,
		)
	case 1:
	default:
		names := make([]string, 0, len(sources))
		for _, source := range sources {
			names = append(names, source.name)
		}
		return fmt.Errorf(
			"preserve %q omits url, but multiple source blocks are declared in root module (%s); set url explicitly",
			preserves[0].name,
			strings.Join(names, ", "),
		)
	}

	sourceName := sources[0].name
	for _, preserve := range preserves {
		injectPreserveURL(preserve, sourceName)
	}
	return nil
}

func injectPreserveURL(preserve omittedPreserveDeclaration, sourceName string) {
	traversal := hcl.Traversal{
		hcl.TraverseRoot{Name: "source", SrcRange: preserve.syntaxBlock.TypeRange},
		hcl.TraverseAttr{Name: sourceName, SrcRange: preserve.syntaxBlock.TypeRange},
		hcl.TraverseAttr{Name: "url", SrcRange: preserve.syntaxBlock.TypeRange},
	}
	preserve.syntaxBlock.Body.Attributes["url"] = &hclsyntax.Attribute{
		Name: "url",
		Expr: &hclsyntax.ScopeTraversalExpr{
			Traversal: traversal,
			SrcRange:  preserve.syntaxBlock.TypeRange,
		},
		SrcRange:    preserve.syntaxBlock.TypeRange,
		NameRange:   preserve.syntaxBlock.TypeRange,
		EqualsRange: preserve.syntaxBlock.TypeRange,
	}
	preserve.writeBlock.Body().SetAttributeRaw("url", hclwrite.TokensForTraversal(traversal))
}
