package module

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	getter "github.com/hashicorp/go-getter/v2"
	"github.com/hashicorp/hcl/v2"
	"github.com/lonegunmanb/sofa/internal/config"
	"github.com/zclconf/go-cty/cty"
)

// FetchFunc downloads a remote configuration or module source into destination.
type FetchFunc func(ctx context.Context, source, destination, workingDirectory string) error

// InitOptions configures recursive module installation.
type InitOptions struct {
	ModulesDirectory string
	Upgrade          bool
	Fetch            FetchFunc
}

type moduleCall struct {
	label  string
	source string
}

// Init scans directory for `module` blocks and recursively installs local and remote modules.
func Init(ctx context.Context, directory string, options InitOptions) error {
	if ctx == nil {
		ctx = context.Background()
	}
	root, err := absoluteDirectory(directory)
	if err != nil {
		return fmt.Errorf("read root module: %w", err)
	}
	modules, err := absolutePath(options.ModulesDirectory)
	if err != nil {
		return fmt.Errorf("resolve modules directory: %w", err)
	}
	if options.Upgrade {
		return upgrade(ctx, root, modules, options)
	}
	if err = os.MkdirAll(modules, 0o700); err != nil {
		return fmt.Errorf("create modules directory %q: %w", modules, err)
	}
	inst := installer{modules: modules, options: options}
	return inst.installChildren(ctx, root, root, "", map[string]struct{}{"local:" + root: {}}, false)
}

func upgrade(ctx context.Context, root, modules string, options InitOptions) error {
	info, err := os.Stat(modules)
	switch {
	case err == nil && !info.IsDir():
		return fmt.Errorf("modules directory %q is not a directory", modules)
	case err != nil && !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("inspect modules directory %q: %w", modules, err)
	}
	parent := filepath.Dir(modules)
	if err = os.MkdirAll(parent, 0o700); err != nil {
		return fmt.Errorf("create modules parent directory: %w", err)
	}
	temporary, err := os.MkdirTemp(parent, ".modules-tmp-")
	if err != nil {
		return fmt.Errorf("create modules staging directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(temporary) }()
	staging := filepath.Join(temporary, "content")
	if err = os.Mkdir(staging, 0o700); err != nil {
		return fmt.Errorf("create modules staging root: %w", err)
	}
	inst := installer{modules: staging, options: options}
	if err = inst.installChildren(ctx, root, root, "", map[string]struct{}{"local:" + root: {}}, false); err != nil {
		return err
	}
	if info == nil {
		if err = os.Rename(staging, modules); err != nil {
			return fmt.Errorf("activate modules: %w", err)
		}
		return nil
	}
	return replaceDirectory(staging, modules)
}

// Directory returns the canonical on-disk path under modulesDirectory for a module address.
func Directory(modulesDirectory, canonicalAddress string) (string, error) {
	parts := strings.Split(canonicalAddress, ".")
	if len(parts) == 0 || len(parts)%2 != 0 {
		return "", fmt.Errorf("module address %q is invalid", canonicalAddress)
	}
	labels := make([]string, 0, len(parts)/2)
	for index := 0; index < len(parts); index += 2 {
		if parts[index] != "module" || !validPathLabel(parts[index+1]) {
			return "", fmt.Errorf("module address %q is invalid", canonicalAddress)
		}
		labels = append(labels, parts[index+1])
	}
	return filepath.Join(append([]string{modulesDirectory}, labels...)...), nil
}

type installer struct {
	modules string
	options InitOptions
}

func (i installer) installChildren(
	ctx context.Context,
	scanDirectory string,
	sourceDirectory string,
	addressPrefix string,
	stack map[string]struct{},
	replaceCopiedDestination bool,
) error {
	calls, err := moduleCalls(ctx, scanDirectory)
	if err != nil {
		return err
	}
	for _, call := range calls {
		address := "module." + call.label
		if addressPrefix != "" {
			address = addressPrefix + "." + address
		}
		destination, err := Directory(i.modules, address)
		if err != nil {
			return err
		}
		source, key, local, err := resolveSource(sourceDirectory, call.source)
		if err != nil {
			return fmt.Errorf("module %s: %w", address, err)
		}
		if _, exists := stack[key]; exists {
			return fmt.Errorf("module source cycle at %s", address)
		}
		installed, installErr := i.install(
			ctx, source, sourceDirectory, destination, local, replaceCopiedDestination,
		)
		if installErr != nil {
			return fmt.Errorf("install module %s: %w", address, installErr)
		}
		nextStack := cloneSet(stack)
		nextStack[key] = struct{}{}
		nextSourceDirectory := destination
		if local {
			nextSourceDirectory = source
		} else {
			nextStack["local:"+destination] = struct{}{}
		}
		if err = i.installChildren(
			ctx, destination, nextSourceDirectory, address, nextStack, installed,
		); err != nil {
			return err
		}
	}
	return nil
}

func (i installer) install(
	ctx context.Context,
	source string,
	workingDirectory string,
	destination string,
	local bool,
	replaceCopiedDestination bool,
) (bool, error) {
	info, err := os.Stat(destination)
	switch {
	case err == nil && !info.IsDir():
		return false, fmt.Errorf("destination %q is not a directory", destination)
	case err == nil && !i.options.Upgrade && !replaceCopiedDestination:
		return false, nil
	case err != nil && !errors.Is(err, os.ErrNotExist):
		return false, fmt.Errorf("inspect destination %q: %w", destination, err)
	}
	if err = os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return false, fmt.Errorf("create module parent directory: %w", err)
	}
	temporary, err := os.MkdirTemp(filepath.Dir(destination), ".sofa-module-")
	if err != nil {
		return false, fmt.Errorf("create module staging directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(temporary) }()
	staging := filepath.Join(temporary, "content")
	if local {
		if err = CopyCleanDirectory(ctx, source, staging); err != nil {
			return false, fmt.Errorf("copy local module: %w", err)
		}
	} else {
		fetched := filepath.Join(temporary, "fetched")
		if err = os.Mkdir(fetched, 0o700); err != nil {
			return false, fmt.Errorf("create fetch destination: %w", err)
		}
		fetch := i.options.Fetch
		if fetch == nil {
			fetch = Fetch
		}
		if err = fetch(ctx, source, fetched, workingDirectory); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return false, ctxErr
			}
			return false, errors.New(RedactSourceInError(err.Error(), source))
		}
		if err = CopyCleanDirectory(ctx, fetched, staging); err != nil {
			return false, fmt.Errorf("copy remote module: %w", err)
		}
	}
	if info == nil {
		if err = os.Rename(staging, destination); err != nil {
			return false, fmt.Errorf("activate module: %w", err)
		}
		return true, nil
	}
	if err = replaceDirectory(staging, destination); err != nil {
		return false, err
	}
	return true, nil
}

func replaceDirectory(staging, destination string) error {
	backup, err := os.MkdirTemp(filepath.Dir(destination), ".modules-backup-")
	if err != nil {
		return fmt.Errorf("reserve module backup path: %w", err)
	}
	if err = os.Remove(backup); err != nil {
		return fmt.Errorf("prepare module backup path: %w", err)
	}
	defer func() { _ = os.RemoveAll(backup) }()
	if err = os.Rename(destination, backup); err != nil {
		return fmt.Errorf("back up existing module: %w", err)
	}
	if err = os.Rename(staging, destination); err != nil {
		restoreErr := os.Rename(backup, destination)
		return errors.Join(fmt.Errorf("activate upgraded module: %w", err), restoreErr)
	}
	if err = os.RemoveAll(backup); err != nil {
		return fmt.Errorf("remove previous module: %w", err)
	}
	return nil
}

// CopyCleanDirectory safely copies regular files and directories from source to destination
// using os.OpenRoot, skipping .sofa, .git, and *.sofaplan files and rejecting symlinks or
// other non-regular entries. All file and root handles are closed before returning.
func CopyCleanDirectory(ctx context.Context, source, destination string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := os.MkdirAll(destination, 0o700); err != nil {
		return fmt.Errorf("create snapshot directory: %w", err)
	}
	sourceRoot, err := os.OpenRoot(source)
	if err != nil {
		return fmt.Errorf("open source directory: %w", err)
	}
	defer func() { _ = sourceRoot.Close() }()

	destinationRoot, err := os.OpenRoot(destination)
	if err != nil {
		return fmt.Errorf("open destination directory: %w", err)
	}
	defer func() { _ = destinationRoot.Close() }()

	err = fs.WalkDir(sourceRoot.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if path == "." {
			return nil
		}
		linfo, lerr := sourceRoot.Lstat(path)
		if lerr != nil {
			return fmt.Errorf("inspect %q: %w", path, lerr)
		}
		if linfo.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("entry %q is not a regular file", path)
		}
		if linfo.IsDir() {
			if entry.Name() == ".sofa" || entry.Name() == ".git" {
				return fs.SkipDir
			}
			if err := destinationRoot.MkdirAll(path, 0o700); err != nil {
				return fmt.Errorf("create directory %q: %w", path, err)
			}
			return nil
		}
		if !linfo.Mode().IsRegular() {
			return fmt.Errorf("entry %q is not a regular file", path)
		}
		if entry.Name() == ".git" || strings.HasSuffix(entry.Name(), ".sofaplan") {
			return nil
		}
		return copyRegularFile(sourceRoot, destinationRoot, path)
	})
	if err != nil {
		return err
	}
	return nil
}

func copyRegularFile(source, destination *os.Root, path string) error {
	input, err := source.Open(path)
	if err != nil {
		return fmt.Errorf("open %q: %w", path, err)
	}
	info, statErr := input.Stat()
	if statErr != nil {
		_ = input.Close()
		return fmt.Errorf("inspect %q: %w", path, statErr)
	}
	if !info.Mode().IsRegular() {
		_ = input.Close()
		return fmt.Errorf("entry %q is not a regular file", path)
	}
	output, err := destination.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
	if err != nil {
		_ = input.Close()
		return fmt.Errorf("create %q: %w", path, err)
	}
	_, copyErr := io.Copy(output, input)
	outCloseErr := output.Close()
	inCloseErr := input.Close()
	if err = errors.Join(copyErr, outCloseErr, inCloseErr); err != nil {
		return fmt.Errorf("copy %q: %w", path, err)
	}
	return nil
}

func moduleCalls(ctx context.Context, directory string) ([]moduleCall, error) {
	loaded, diagnostics, err := config.LoadDirectoryContext(ctx, directory)
	if err != nil {
		return nil, fmt.Errorf("read module directory %q: %w", directory, err)
	}
	if diagnostics.HasErrors() {
		return nil, diagnostics
	}
	calls := make([]moduleCall, 0)
	seen := make(map[string]struct{})
	for _, block := range loaded.Blocks {
		if block.Type != "module" {
			continue
		}
		label, ok := SingleLabel(block.Labels)
		if !ok || !validPathLabel(label) {
			return nil, fmt.Errorf("module label is invalid")
		}
		for previous := range seen {
			if !strings.EqualFold(previous, label) {
				continue
			}
			if previous == label {
				return nil, fmt.Errorf("module %q is declared more than once", label)
			}
			return nil, fmt.Errorf("module %q conflicts with module %q", label, previous)
		}
		seen[label] = struct{}{}
		attribute, ok := block.Attributes()["source"]
		if !ok || len(attribute.Expr.Variables()) != 0 {
			return nil, fmt.Errorf("module source must be a literal string")
		}
		value, valueDiagnostics := attribute.Expr.Value(&hcl.EvalContext{})
		if valueDiagnostics.HasErrors() || value.IsNull() || !value.IsWhollyKnown() || !value.Type().Equals(cty.String) {
			return nil, fmt.Errorf("module source must be a literal string")
		}
		source := value.AsString()
		if strings.TrimSpace(source) == "" {
			return nil, fmt.Errorf("module source must not be empty")
		}
		calls = append(calls, moduleCall{label: label, source: source})
	}
	sort.Slice(calls, func(left, right int) bool { return calls[left].label < calls[right].label })
	return calls, nil
}

// SingleLabel extracts the block label from either a raw HCL block or a golden-wrapped HclBlock.
func SingleLabel(labels []string) (string, bool) {
	switch {
	case len(labels) == 1:
		return labels[0], true
	case len(labels) == 2 && labels[0] == "":
		return labels[1], true
	default:
		return "", false
	}
}

func resolveSource(directory, source string) (resolved, key string, local bool, err error) {
	if !IsLocalSource(source) {
		return source, "remote:" + source, false, nil
	}
	resolved = source
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(directory, resolved)
	}
	resolved, err = absoluteDirectory(resolved)
	if err != nil {
		return "", "", false, err
	}
	return resolved, "local:" + resolved, true, nil
}

// IsLocalSource reports whether source refers to a local directory path.
func IsLocalSource(source string) bool {
	if filepath.IsAbs(source) || source == "." || source == ".." {
		return true
	}
	return strings.HasPrefix(source, "./") || strings.HasPrefix(source, "../") ||
		strings.HasPrefix(source, `.\`) || strings.HasPrefix(source, `..\`)
}

func absoluteDirectory(path string) (string, error) {
	absolute, err := absolutePath(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%q is not a directory", path)
	}
	return absolute, nil
}

func absolutePath(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("path is required")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.Clean(absolute), nil
}

func validPathLabel(label string) bool {
	if label == "" || label == "." || label == ".." || strings.ContainsAny(label, `/\`) {
		return false
	}
	for _, r := range label {
		if unicode.IsControl(r) {
			return false
		}
	}
	return filepath.IsLocal(label)
}

func cloneSet(source map[string]struct{}) map[string]struct{} {
	result := make(map[string]struct{}, len(source))
	for key := range source {
		result[key] = struct{}{}
	}
	return result
}

// Fetch downloads a remote module or configuration source using go-getter with symlinks disabled.
// Any credentials, fragments, or non-ref query parameters are redacted from returned errors.
func Fetch(ctx context.Context, source, destination, workingDirectory string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	client := getter.Client{DisableSymlinks: true}
	_, err := client.Get(ctx, &getter.Request{
		Src:             source,
		Dst:             destination,
		Pwd:             workingDirectory,
		Umask:           0o022,
		GetMode:         getter.ModeDir,
		Copy:            true,
		DisableSymlinks: true,
	})
	if err != nil {
		return fmt.Errorf("go-getter: %s", RedactSourceInError(err.Error(), source))
	}
	return nil
}

// RedactSource strips credentials, fragments, and non-ref query parameters from a remote source string.
func RedactSource(source string) string {
	prefix := ""
	if getterPrefix, locator, found := strings.Cut(source, "::"); found {
		prefix = getterPrefix + "::"
		source = locator
	}
	parsed, err := url.Parse(source)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return prefix + redactShorthand(source)
	}
	parsed.User = nil
	parsed.Fragment = ""
	query := make(url.Values)
	if ref := parsed.Query().Get("ref"); ref != "" {
		query.Set("ref", ref)
	}
	parsed.RawQuery = query.Encode()
	return prefix + parsed.String()
}

func redactShorthand(source string) string {
	location, rawQuery, found := strings.Cut(source, "?")
	location, _, _ = strings.Cut(location, "#")
	if strings.Contains(location, "@") {
		return "<redacted-remote-source>"
	}
	if !found {
		return location
	}
	rawQuery, _, _ = strings.Cut(rawQuery, "#")
	query, err := url.ParseQuery(rawQuery)
	if err != nil || query.Get("ref") == "" {
		return location
	}
	return location + "?ref=" + url.QueryEscape(query.Get("ref"))
}

// RedactSourceInError removes source and any embedded credentials, fragments, or non-ref query values from message.
func RedactSourceInError(message, source string) string {
	redacted := RedactSource(source)
	out := strings.ReplaceAll(message, source, redacted)

	locator := source
	if _, after, found := strings.Cut(source, "::"); found {
		locator = after
		out = strings.ReplaceAll(out, locator, RedactSource(locator))
	}
	if parsed, err := url.Parse(locator); err == nil {
		if parsed.User != nil {
			if rawUser := parsed.User.String(); rawUser != "" {
				out = strings.ReplaceAll(out, rawUser+"@", "<redacted>@")
			}
			if password, ok := parsed.User.Password(); ok && password != "" {
				out = strings.ReplaceAll(out, password, "<redacted>")
			}
			if user := parsed.User.Username(); user != "" {
				out = strings.ReplaceAll(out, user+"@", "<redacted>@")
			}
		}
		if escapedFrag := parsed.EscapedFragment(); escapedFrag != "" {
			out = strings.ReplaceAll(out, "#"+escapedFrag, "")
			out = strings.ReplaceAll(out, escapedFrag, "<redacted>")
		}
		if parsed.Fragment != "" {
			out = strings.ReplaceAll(out, "#"+parsed.Fragment, "")
			out = strings.ReplaceAll(out, parsed.Fragment, "<redacted>")
		}
		for _, pair := range strings.Split(parsed.RawQuery, "&") {
			if pair == "" {
				continue
			}
			rawKey, rawVal, hasEq := strings.Cut(pair, "=")
			if rawKey == "ref" || !hasEq || rawVal == "" {
				continue
			}
			out = strings.ReplaceAll(out, rawKey+"="+rawVal, rawKey+"=<redacted>")
			out = strings.ReplaceAll(out, rawVal, "<redacted>")
		}
		for key, values := range parsed.Query() {
			if key == "ref" {
				continue
			}
			for _, value := range values {
				if value == "" {
					continue
				}
				out = strings.ReplaceAll(out, key+"="+value, key+"=<redacted>")
				out = strings.ReplaceAll(out, value, "<redacted>")
			}
		}
	}
	return out
}
