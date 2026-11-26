package planner

import (
	"fmt"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/lonegunmanb/golden"
)

func validateWorkerFallbacks(blocks []*golden.HclBlock) error {
	workers := map[string]*golden.HclBlock{}
	for _, block := range blocks {
		if block != nil && block.Type == "worker" && len(block.Labels) == 2 {
			workers["worker."+block.Labels[1]] = block
		}
	}
	dependencies := make(map[string]map[string]struct{}, len(workers))
	for address, block := range workers {
		dependencies[address] = map[string]struct{}{}
		for _, attr := range block.Attributes() {
			collectWorkerReferences(attr.Expr.Variables(), address, dependencies)
		}
		collectNestedWorkerReferences(block, address, dependencies)
	}
	for address, block := range workers {
		for _, nested := range block.NestedBlocks() {
			if nested.Type != "fallback" {
				continue
			}
			targetAttr, ok := nested.Attributes()["target"]
			if !ok {
				continue
			}
			traversal, diags := hcl.AbsTraversalForExpr(targetAttr.Expr)
			if diags.HasErrors() {
				return fmt.Errorf("worker %s fallback target must be a block traversal", address)
			}
			target := traversalToAddress(traversal)
			if target == address {
				return fmt.Errorf("worker %s fallback target must not reference itself", address)
			}
			if !workerAncestor(address, target, dependencies, map[string]struct{}{}) {
				return fmt.Errorf("worker %s fallback target %s is not a positive ancestor", address, target)
			}
		}
	}
	return nil
}

func collectNestedWorkerReferences(
	block *golden.HclBlock,
	current string,
	dependencies map[string]map[string]struct{},
) {
	for _, nested := range block.NestedBlocks() {
		if nested.Type == "fallback" {
			continue
		}
		for _, attr := range nested.Attributes() {
			collectWorkerReferences(attr.Expr.Variables(), current, dependencies)
		}
		collectNestedWorkerReferences(nested, current, dependencies)
	}
}

func collectWorkerReferences(
	traversals []hcl.Traversal,
	current string,
	dependencies map[string]map[string]struct{},
) {
	for _, traversal := range traversals {
		parts := strings.Split(traversalToAddress(traversal), ".")
		if len(parts) >= 2 && parts[0] == "worker" {
			dependencies[current]["worker."+parts[1]] = struct{}{}
		}
	}
}

func traversalToAddress(traversal hcl.Traversal) string {
	if len(traversal) == 0 {
		return ""
	}
	var builder strings.Builder
	builder.WriteString(traversal.RootName())
	for _, step := range traversal[1:] {
		attr, ok := step.(hcl.TraverseAttr)
		if !ok {
			break
		}
		builder.WriteString(".")
		builder.WriteString(attr.Name)
	}
	return builder.String()
}

func workerAncestor(
	current string,
	target string,
	dependencies map[string]map[string]struct{},
	visited map[string]struct{},
) bool {
	if _, seen := visited[current]; seen {
		return false
	}
	visited[current] = struct{}{}
	for dependency := range dependencies[current] {
		if dependency == target {
			return true
		}
		if workerAncestor(dependency, target, dependencies, visited) {
			return true
		}
	}
	return false
}
