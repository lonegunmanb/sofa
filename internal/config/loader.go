package config

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/lonegunmanb/golden"
)

const sofaConfigSuffix = ".sofa.hcl"

type SourceFile struct {
	Path   string
	Source []byte
}

type Loaded struct {
	Files  []SourceFile
	Blocks []*golden.HclBlock
}

// LoadDirectoryContext loads all direct *.sofa.hcl files in deterministic
// order and converts valid declarations to golden blocks.
func LoadDirectoryContext(ctx context.Context, directory string) (Loaded, hcl.Diagnostics, error) {
	var loaded Loaded
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return loaded, nil, err
	}

	absoluteDir, err := filepath.Abs(directory)
	if err != nil {
		return loaded, nil, fmt.Errorf("resolve configuration directory %q: %w", directory, err)
	}
	absoluteDir = filepath.Clean(absoluteDir)

	info, err := os.Stat(absoluteDir)
	if err != nil {
		return loaded, nil, fmt.Errorf("inspect configuration directory %s: %w", absoluteDir, err)
	}
	if !info.IsDir() {
		return loaded, nil, fmt.Errorf("configuration path %s is not a directory", absoluteDir)
	}

	entries, err := os.ReadDir(absoluteDir)
	if err != nil {
		return loaded, nil, fmt.Errorf("read configuration directory %s: %w", absoluteDir, err)
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	var sourceFiles []SourceFile
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return loaded, nil, err
		}
		if !strings.HasSuffix(entry.Name(), sofaConfigSuffix) {
			continue
		}
		entryInfo, err := entry.Info()
		if err != nil {
			return loaded, nil, fmt.Errorf("inspect configuration file %s: %w", entry.Name(), err)
		}
		if !entryInfo.Mode().IsRegular() {
			continue
		}

		path := filepath.Join(absoluteDir, entry.Name())
		source, err := os.ReadFile(path)
		if err != nil {
			return loaded, nil, fmt.Errorf("read configuration file %s: %w", path, err)
		}
		sourceFiles = append(sourceFiles, SourceFile{Path: path, Source: source})
	}
	if len(sourceFiles) == 0 {
		return loaded, nil, fmt.Errorf("no .sofa.hcl files found in %s", absoluteDir)
	}
	loaded.Files = sourceFiles

	var diags hcl.Diagnostics
	var parsedFiles []ParsedFile
	for _, sourceFile := range sourceFiles {
		if err := ctx.Err(); err != nil {
			return loaded, diags, err
		}

		syntaxFile, syntaxDiags := hclsyntax.ParseConfig(
			sourceFile.Source,
			sourceFile.Path,
			hcl.InitialPos,
		)
		writeFile, writeDiags := hclwrite.ParseConfig(
			sourceFile.Source,
			sourceFile.Path,
			hcl.InitialPos,
		)
		diags = append(diags, syntaxDiags...)
		diags = append(diags, writeDiags...)
		if syntaxDiags.HasErrors() || writeDiags.HasErrors() {
			continue
		}

		syntaxBody, ok := syntaxFile.Body.(*hclsyntax.Body)
		if !ok {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  "Invalid HCL body",
				Detail:   "The configuration file did not produce an HCL syntax body.",
				Subject:  syntaxFile.Body.MissingItemRange().Ptr(),
			})
			continue
		}

		attributeNames := make([]string, 0, len(syntaxBody.Attributes))
		for name := range syntaxBody.Attributes {
			attributeNames = append(attributeNames, name)
		}
		sort.Strings(attributeNames)
		for _, name := range attributeNames {
			attribute := syntaxBody.Attributes[name]
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  "Top-level attributes are not allowed",
				Detail:   fmt.Sprintf("Only top-level blocks are allowed in sofa configuration files; move %q into a block.", name),
				Subject:  attribute.NameRange.Ptr(),
			})
		}
		if len(attributeNames) > 0 {
			continue
		}

		parsedFiles = append(parsedFiles, ParsedFile{
			Path:   sourceFile.Path,
			Syntax: syntaxBody,
			Write:  writeFile.Body(),
		})
	}

	if err := runRootDefaultsHook(&RootModule{Files: parsedFiles}); err != nil {
		return loaded, diags, err
	}

	for _, parsedFile := range parsedFiles {
		loaded.Blocks = append(
			loaded.Blocks,
			golden.AsHclBlocks(parsedFile.Syntax.Blocks, parsedFile.Write.Blocks())...,
		)
	}

	return loaded, diags, nil
}
