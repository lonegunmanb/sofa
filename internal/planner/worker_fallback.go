package planner

import (
	"fmt"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/lonegunmanb/golden"
	workerspec "github.com/lonegunmanb/sofa/internal/worker/spec"
	"github.com/zclconf/go-cty/cty"
)

func validateWorkerFallbacks(blocks []*golden.HclBlock) error {
	blockMap := map[string]*golden.HclBlock{}
	workers := map[string]*golden.HclBlock{}
	dynamicWorkers := map[string]*golden.HclBlock{}
	for _, block := range blocks {
		if block == nil || len(block.Labels) != 2 {
			continue
		}
		address := rawBlockAddress(block)
		blockMap[address] = block
		switch block.Type {
		case "worker":
			workers[address] = block
		case "dynamic_worker":
			dynamicWorkers[address] = block
		}
	}

	dependencies := make(map[string]map[string]struct{}, len(blockMap))
	for address, block := range blockMap {
		dependencies[address] = map[string]struct{}{}
		skipFallback := block.Type == "worker" || block.Type == "dynamic_worker"
		walkPositiveBlockTraversals(block, skipFallback, func(traversal hcl.Traversal) {
			for _, target := range resolveInitTraversalTargets(traversal, blockMap) {
				if target != address {
					dependencies[address][target] = struct{}{}
				}
			}
		})
	}

	for address, block := range workers {
		if err := validateBlockFallbacks(address, block, dependencies); err != nil {
			return err
		}
	}
	for address, block := range dynamicWorkers {
		for _, dyn := range block.NestedBlocks() {
			if dyn.Type != "dynamic" || len(dyn.Labels) != 1 || dyn.Labels[0] != "worker" {
				continue
			}
			for _, content := range dyn.NestedBlocks() {
				if content.Type != "content" || len(content.Labels) != 0 {
					continue
				}
				if err := validateBlockFallbacks(address, content, dependencies); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func validateBlockFallbacks(address string, block *golden.HclBlock, dependencies map[string]map[string]struct{}) error {
	for _, nested := range block.NestedBlocks() {
		if nested.Type != "fallback" {
			continue
		}
		targetAttr, ok := nested.Attributes()["target"]
		if !ok {
			continue
		}
		target, err := workerspec.ParseFallbackTargetExpr(targetAttr.Expr, targetAttr.ExprString())
		if err != nil {
			return fmt.Errorf("worker %s: %w", address, err)
		}
		baseTarget := baseWorkerAddress(target)
		if baseTarget == address {
			return fmt.Errorf("worker %s fallback target must not reference itself", address)
		}
		if !workerAncestor(address, baseTarget, dependencies, map[string]struct{}{}) {
			return fmt.Errorf("worker %s fallback target %s is not a positive ancestor", address, target)
		}
	}
	return nil
}

// ValidateWorkerFallbackTargets checks expanded fallback target existence and
// positive ancestry (including locals, depends_on, intermediate blocks, and
// for_each instances) after golden DAG expansion.
func (c *SofaConfig) ValidateWorkerFallbackTargets(workerAddress string, targets []string) error {
	vertices := c.GetVertices()
	baseToInstances := map[string][]string{}
	workerBlocks := map[string]*workerspec.WorkerBlock{}
	allBlocks := map[string]golden.Block{}

	for addr, v := range vertices {
		b, ok := v.(golden.Block)
		if !ok {
			continue
		}
		allBlocks[addr] = b
		base := baseWorkerAddress(addr)
		if base != addr {
			baseToInstances[base] = append(baseToInstances[base], addr)
		}
		if wb, isWorker := v.(*workerspec.WorkerBlock); isWorker {
			workerBlocks[addr] = wb
		}
	}

	dependencies := make(map[string]map[string]struct{}, len(allBlocks))
	for addr, b := range allBlocks {
		dependencies[addr] = map[string]struct{}{}
		_, isWorker := b.(*workerspec.WorkerBlock)
		_, isDynamicWorker := b.(*workerspec.DynamicWorkerBlock)
		walkPositiveBlockTraversals(b.HclBlock(), isWorker || isDynamicWorker, func(traversal hcl.Traversal) {
			for _, dep := range resolveExpandedTraversalTargets(traversal, allBlocks, baseToInstances) {
				if dep != addr {
					dependencies[addr][dep] = struct{}{}
				}
			}
		})
	}

	for _, target := range targets {
		if target == workerAddress {
			return fmt.Errorf("worker %s fallback target must not reference itself", workerAddress)
		}
		if _, exists := workerBlocks[target]; !exists {
			return fmt.Errorf("worker %s fallback target %s does not exist", workerAddress, target)
		}
		if !workerAncestor(workerAddress, target, dependencies, map[string]struct{}{}) {
			return fmt.Errorf("worker %s fallback target %s is not a positive ancestor", workerAddress, target)
		}
	}
	return nil
}

func walkPositiveBlockTraversals(block *golden.HclBlock, skipFallback bool, visit func(hcl.Traversal)) {
	if block == nil {
		return
	}
	for _, attr := range block.Attributes() {
		for _, traversal := range attr.Expr.Variables() {
			visit(traversal)
		}
	}
	for _, nested := range block.NestedBlocks() {
		if skipFallback && (nested.Type == "fallback" || (nested.Type == "dynamic" && len(nested.Labels) > 0 && nested.Labels[0] == "fallback")) {
			continue
		}
		walkPositiveBlockTraversals(nested, skipFallback, visit)
	}
}

func resolveInitTraversalTargets(traversal hcl.Traversal, blockMap map[string]*golden.HclBlock) []string {
	if len(traversal) < 2 {
		return nil
	}
	root := traversal.RootName()
	attr1, ok := traversal[1].(hcl.TraverseAttr)
	if !ok || attr1.Name == "" {
		return nil
	}
	var targets []string
	addr2 := root + "." + attr1.Name
	if _, exists := blockMap[addr2]; exists {
		targets = append(targets, addr2)
	}
	if len(traversal) >= 3 {
		if attr2Step, ok := traversal[2].(hcl.TraverseAttr); ok && attr2Step.Name != "" {
			addr3 := addr2 + "." + attr2Step.Name
			if _, exists := blockMap[addr3]; exists {
				targets = append(targets, addr3)
			}
		}
	}
	return targets
}

func resolveExpandedTraversalTargets(
	traversal hcl.Traversal,
	allBlocks map[string]golden.Block,
	baseToInstances map[string][]string,
) []string {
	if len(traversal) < 2 {
		return nil
	}
	root := traversal.RootName()
	attr1, ok := traversal[1].(hcl.TraverseAttr)
	if !ok || attr1.Name == "" {
		return nil
	}
	var candidates []string
	addr2 := root + "." + attr1.Name
	candidates = append(candidates, matchExpandedAddress(addr2, traversal, 2, allBlocks, baseToInstances)...)
	if len(traversal) >= 3 {
		if attr2Step, ok := traversal[2].(hcl.TraverseAttr); ok && attr2Step.Name != "" {
			addr3 := addr2 + "." + attr2Step.Name
			candidates = append(candidates, matchExpandedAddress(addr3, traversal, 3, allBlocks, baseToInstances)...)
		}
	}
	return candidates
}

func matchExpandedAddress(
	base string,
	traversal hcl.Traversal,
	indexPos int,
	allBlocks map[string]golden.Block,
	baseToInstances map[string][]string,
) []string {
	if len(traversal) > indexPos {
		if idx, ok := traversal[indexPos].(hcl.TraverseIndex); ok {
			key, _ := idx.Key.UnmarkDeep()
			if key.IsKnown() && !key.IsNull() && (key.Type().Equals(cty.String) || key.Type().Equals(cty.Number)) {
				instanceAddr := fmt.Sprintf("%s[%s]", base, golden.CtyValueToString(key))
				if _, exists := allBlocks[instanceAddr]; exists {
					return []string{instanceAddr}
				}
			}
		}
	}
	if _, exists := allBlocks[base]; exists {
		return []string{base}
	}
	return baseToInstances[base]
}

func rawBlockAddress(block *golden.HclBlock) string {
	if len(block.Labels) == 2 && block.Labels[0] == "" {
		return block.Type + "." + block.Labels[1]
	}
	return block.Type + "." + strings.Join(block.Labels, ".")
}

func baseWorkerAddress(address string) string {
	if idx := strings.IndexByte(address, '['); idx >= 0 {
		return address[:idx]
	}
	return address
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
