package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lonegunmanb/golden"
	"github.com/stretchr/testify/require"
)

func TestLoadDirectoryContextFiltersAndOrdersFiles(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "z.sofa.hcl"), `variable "z" { type = string }`)
	writeTestFile(t, filepath.Join(dir, "a.sofa.hcl"), `variable "a" { type = string }`)
	writeTestFile(t, filepath.Join(dir, "ignored.hcl"), `variable "ignored_hcl" { type = string }`)
	writeTestFile(t, filepath.Join(dir, "ignored.txt"), "not HCL")
	writeTestFile(t, filepath.Join(dir, "upper.SOFA.HCL"), `variable "upper" { type = string }`)
	require.NoError(t, os.Mkdir(filepath.Join(dir, "nested"), 0o755))
	writeTestFile(t, filepath.Join(dir, "nested", "nested.sofa.hcl"), `variable "nested" { type = string }`)

	loaded, diags, err := LoadDirectoryContext(context.Background(), dir)
	require.NoError(t, err)
	require.False(t, diags.HasErrors(), diags.Error())
	require.Len(t, loaded.Files, 2)
	require.True(t, strings.HasSuffix(loaded.Files[0].Path, "a.sofa.hcl"))
	require.True(t, strings.HasSuffix(loaded.Files[1].Path, "z.sofa.hcl"))
	require.Len(t, loaded.Blocks, 2)
	require.Equal(t, "a", loaded.Blocks[0].Labels[1])
	require.Equal(t, "z", loaded.Blocks[1].Labels[1])
	require.Equal(t, `variable "a" { type = string }`, string(loaded.Files[0].Source))
}

func TestLoadDirectoryContextRejectsEmptyDirectory(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "ignored.hcl"), "")

	_, _, err := LoadDirectoryContext(context.Background(), dir)
	require.Error(t, err)
	require.Contains(t, err.Error(), filepath.Clean(dir))
	require.Contains(t, err.Error(), "no .sofa.hcl files found")
}

func TestLoadDirectoryContextRejectsMissingAndNonDirectoryPaths(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "missing")
		_, _, err := LoadDirectoryContext(context.Background(), missing)
		require.Error(t, err)
		require.Contains(t, err.Error(), missing)
	})

	t.Run("regular file", func(t *testing.T) {
		fileName := filepath.Join(t.TempDir(), "plain.sofa.hcl")
		writeTestFile(t, fileName, "")
		_, _, err := LoadDirectoryContext(context.Background(), fileName)
		require.Error(t, err)
		require.Contains(t, err.Error(), fileName)
		require.Contains(t, err.Error(), "not a directory")
	})
}

func TestLoadDirectoryContextRejectsTopLevelAttributes(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "bad.sofa.hcl"), "x = 1\n")

	loaded, diags, err := LoadDirectoryContext(context.Background(), dir)
	require.NoError(t, err)
	require.True(t, diags.HasErrors())
	require.Contains(t, diags.Error(), "top-level")
	require.Contains(t, diags.Error(), "blocks")
	require.Len(t, loaded.Files, 1)
	require.Empty(t, loaded.Blocks)
}

func TestLoadDirectoryContextCollectsSyntaxDiagnosticsAndKeepsGoodFiles(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "a-good.sofa.hcl"), `variable "good" { type = string }`)
	writeTestFile(t, filepath.Join(dir, "b-bad.sofa.hcl"), "variable \"bad\" {\n  type =\n}\n")

	loaded, diags, err := LoadDirectoryContext(context.Background(), dir)
	require.NoError(t, err)
	require.True(t, diags.HasErrors())
	require.Contains(t, diags.Error(), "b-bad.sofa.hcl")
	require.Len(t, loaded.Files, 2)
	require.Len(t, loaded.Blocks, 1)
	require.Equal(t, "good", loaded.Blocks[0].Labels[1])
}

func TestLoadDirectoryContextKeepsAlreadyAbsolutePaths(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "a.sofa.hcl")
	writeTestFile(t, filePath, `variable "a" { type = string }`)

	loaded, diags, err := LoadDirectoryContext(context.Background(), dir)
	require.NoError(t, err)
	require.False(t, diags.HasErrors(), diags.Error())
	require.Equal(t, filepath.Clean(filePath), filepath.Clean(loaded.Files[0].Path))
}

func TestLoadDirectoryContextDoesNotApplyRootDefaultsOrHideSyntaxErrors(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "a-preserve.sofa.hcl"), `
preserve "delivery" {
  branch = "release/v1"
}
`)
	writeTestFile(t, filepath.Join(dir, "b-bad-source.sofa.hcl"), `
source "project" {
  url =
}
`)

	loaded, diags, err := LoadDirectoryContext(context.Background(), dir)
	require.NoError(t, err)
	require.True(t, diags.HasErrors())
	require.Contains(t, diags.Error(), "b-bad-source.sofa.hcl")
	require.Len(t, loaded.Blocks, 1)
	require.Equal(t, "preserve", loaded.Blocks[0].Type)
}

func TestLoadDirectoryContextIsDeterministic(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "c.sofa.hcl"), `variable "c" { type = string }`)
	writeTestFile(t, filepath.Join(dir, "a.sofa.hcl"), `variable "a" { type = string }`)
	writeTestFile(t, filepath.Join(dir, "b.sofa.hcl"), `variable "b" { type = string }`)

	first, diags, err := LoadDirectoryContext(context.Background(), dir)
	require.NoError(t, err)
	require.False(t, diags.HasErrors(), diags.Error())
	for i := 0; i < 25; i++ {
		next, nextDiags, err := LoadDirectoryContext(context.Background(), dir)
		require.NoError(t, err)
		require.False(t, nextDiags.HasErrors(), nextDiags.Error())
		require.Equal(t, first.Files, next.Files)
		require.Equal(t, blockNames(first.Blocks), blockNames(next.Blocks))
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

func blockNames(blocks []*golden.HclBlock) []string {
	names := make([]string, 0, len(blocks))
	for _, block := range blocks {
		names = append(names, block.Type+":"+strings.Join(block.Labels, "."))
	}
	return names
}
