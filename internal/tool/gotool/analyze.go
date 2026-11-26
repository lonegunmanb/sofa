package gotool

import (
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/scanner"
	"go/token"
	"go/types"
	"path"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/zclconf/go-cty/cty"
)

// Analysis captures the Plan-time cty contract derived from an inline Go tool.
type Analysis struct {
	InputType  cty.Type
	OutputType cty.Type
	// QuotePaths identifies input fields whose defined Go type is Quote.
	// Each path uses field names, [] for list/array elements, and * for map values.
	QuotePaths [][]string
}

// Analyze performs pure offline AST analysis on inline Go tool source without dependencies.
func Analyze(source string) (Analysis, error) {
	return AnalyzeWithDependencies(source, nil)
}

// AnalyzeWithDependencies performs pure offline AST analysis on inline Go tool
// source and declared module@version dependencies without running go get.
func AnalyzeWithDependencies(source string, dependencies []string) (Analysis, error) {
	for _, dependency := range dependencies {
		if err := ValidateDependency(dependency); err != nil {
			return Analysis{}, err
		}
	}
	if containsPackageClause(source) {
		return Analysis{}, fmt.Errorf("package clause is not allowed")
	}

	fileSet := token.NewFileSet()
	file, err := parser.ParseFile(fileSet, "tool.go", "package gotoolwrap\n"+source, parser.AllErrors)
	if err != nil {
		return Analysis{}, fmt.Errorf("parsing inline Go tool: %w", adjustParseErrorLines(err))
	}

	typeDecls, wireMethods, err := inspectDeclarations(file)
	if err != nil {
		return Analysis{}, err
	}
	contextAlias, err := validateImports(file, dependencies)
	if err != nil {
		return Analysis{}, err
	}

	inputSpec, err := requiredTypeSpec(typeDecls, "Input")
	if err != nil {
		return Analysis{}, err
	}
	outputSpec, err := requiredTypeSpec(typeDecls, "Output")
	if err != nil {
		return Analysis{}, err
	}
	if !resolvesToStruct(typeDecls, inputSpec.Type, make(map[string]bool)) {
		return Analysis{}, fmt.Errorf("Input must be a struct")
	}
	if err := validateInvoke(file, contextAlias); err != nil {
		return Analysis{}, err
	}

	quotePaths := make([][]string, 0)
	inputMapper := astTypeMapper{
		typeDecls:   typeDecls,
		wireMethods: wireMethods,
		visiting:    make(map[string]bool),
		quotePaths:  &quotePaths,
	}
	inputType, err := inputMapper.mapNamedSpec(inputSpec, "Input", nil)
	if err != nil {
		return Analysis{}, err
	}

	outputMapper := astTypeMapper{
		typeDecls:   typeDecls,
		wireMethods: wireMethods,
		visiting:    make(map[string]bool),
	}
	outputType, err := outputMapper.mapNamedSpec(outputSpec, "Output", nil)
	if err != nil {
		return Analysis{}, err
	}

	return Analysis{
		InputType:  inputType,
		OutputType: outputType,
		QuotePaths: quotePaths,
	}, nil
}

// ValidateDependency validates a Go module@version declaration offline.
func ValidateDependency(dependency string) error {
	if dependency == "" {
		return fmt.Errorf("invalid dependency %q: entry must not be empty", dependency)
	}
	if strings.IndexFunc(dependency, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}) >= 0 {
		return fmt.Errorf("invalid dependency %q: must not contain whitespace or control characters", dependency)
	}
	if strings.HasPrefix(dependency, "-") {
		return fmt.Errorf("invalid dependency %q: must not start with '-'", dependency)
	}
	if strings.Contains(dependency, "=>") || strings.Contains(dependency, `\`) {
		return fmt.Errorf("invalid dependency %q: local path replacements are not allowed", dependency)
	}
	if strings.Count(dependency, "@") != 1 {
		return fmt.Errorf("invalid dependency %q: must use module@version format", dependency)
	}
	modulePath, version, _ := strings.Cut(dependency, "@")
	if modulePath == "" || version == "" {
		return fmt.Errorf("invalid dependency %q: module and version are required", dependency)
	}
	if strings.HasPrefix(modulePath, ".") || strings.HasPrefix(modulePath, "/") ||
		strings.HasSuffix(modulePath, "/") || strings.Contains(modulePath, "..") ||
		strings.Contains(modulePath, "//") {
		return fmt.Errorf("invalid dependency %q: invalid module path", dependency)
	}
	firstSegment, _, _ := strings.Cut(modulePath, "/")
	if !strings.Contains(firstSegment, ".") {
		return fmt.Errorf("invalid dependency %q: module path must include a domain", dependency)
	}
	for _, r := range modulePath {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._~/-", r)) {
			return fmt.Errorf("invalid dependency %q: invalid character in module path", dependency)
		}
	}
	if strings.HasPrefix(version, "-") || strings.ContainsAny(version, `/\`) {
		return fmt.Errorf("invalid dependency %q: invalid version", dependency)
	}
	if !strings.HasPrefix(version, "v") || len(version) < 2 || version[1] < '0' || version[1] > '9' {
		return fmt.Errorf("invalid dependency %q: version must be a Go module version starting with v", dependency)
	}
	return nil
}

func adjustParseErrorLines(err error) error {
	errorList, ok := err.(scanner.ErrorList)
	if !ok {
		return err
	}
	adjusted := make(scanner.ErrorList, len(errorList))
	for i, item := range errorList {
		copyItem := *item
		if copyItem.Pos.Line > 1 {
			copyItem.Pos.Line--
		}
		adjusted[i] = &copyItem
	}
	return adjusted
}

func containsPackageClause(source string) bool {
	fileSet := token.NewFileSet()
	file := fileSet.AddFile("tool.go", -1, len(source))
	var sourceScanner scanner.Scanner
	sourceScanner.Init(file, []byte(source), nil, scanner.ScanComments)
	for {
		_, scanned, _ := sourceScanner.Scan()
		if scanned == token.PACKAGE {
			return true
		}
		if scanned == token.EOF {
			return false
		}
	}
}

func inspectDeclarations(file *ast.File) (map[string]*ast.TypeSpec, map[string]bool, error) {
	typeDecls := make(map[string]*ast.TypeSpec)
	wireMethods := make(map[string]bool)

	for _, declaration := range file.Decls {
		switch decl := declaration.(type) {
		case *ast.FuncDecl:
			if decl.Recv == nil {
				if decl.Name.Name == "main" {
					return nil, nil, fmt.Errorf("main function is not allowed")
				}
				continue
			}
			recvName := receiverTypeName(decl.Recv)
			if isWireMethod(decl.Name.Name) && recvName != "" {
				if recvName == "Issue" || recvName == "ToolResponse" {
					return nil, nil, fmt.Errorf("%s: custom JSON or text methods are not supported", recvName)
				}
				wireMethods[recvName] = true
			}
		case *ast.GenDecl:
			if decl.Tok != token.TYPE {
				continue
			}
			for _, spec := range decl.Specs {
				typeSpec, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				name := typeSpec.Name.Name
				if types.Universe.Lookup(name) != nil {
					return nil, nil, fmt.Errorf("cannot redeclare predeclared Go identifier %s", name)
				}
				if name == "Issue" || name == "ToolResponse" {
					return nil, nil, fmt.Errorf("type %s is reserved by go_tool wrapper", name)
				}
				if _, exists := typeDecls[name]; exists {
					return nil, nil, fmt.Errorf("duplicate type declaration %s", name)
				}
				typeDecls[name] = typeSpec
			}
		}
	}
	return typeDecls, wireMethods, nil
}

func receiverTypeName(recv *ast.FieldList) string {
	if recv == nil || len(recv.List) == 0 {
		return ""
	}
	expr := unwrapParens(recv.List[0].Type)
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = unwrapParens(star.X)
	}
	switch typed := expr.(type) {
	case *ast.Ident:
		return typed.Name
	case *ast.IndexExpr:
		if ident, ok := unwrapParens(typed.X).(*ast.Ident); ok {
			return ident.Name
		}
	case *ast.IndexListExpr:
		if ident, ok := unwrapParens(typed.X).(*ast.Ident); ok {
			return ident.Name
		}
	}
	return ""
}

func isWireMethod(name string) bool {
	switch name {
	case "MarshalJSON", "UnmarshalJSON", "MarshalText", "UnmarshalText":
		return true
	default:
		return false
	}
}

func validateImports(file *ast.File, dependencies []string) (string, error) {
	contextAlias := ""
	seenLocalNames := make(map[string]string)
	for _, importSpec := range file.Imports {
		importPath, err := strconv.Unquote(importSpec.Path.Value)
		if err != nil {
			return "", fmt.Errorf("invalid import path %s: %w", importSpec.Path.Value, err)
		}
		if importPath == "C" || importPath == "unsafe" {
			return "", fmt.Errorf("import %q is not allowed", importPath)
		}

		localName := path.Base(importPath)
		if importSpec.Name != nil {
			localName = importSpec.Name.Name
		}
		if localName == "." {
			return "", fmt.Errorf("dot import %q is not allowed", importPath)
		}
		if localName != "_" {
			if previous, exists := seenLocalNames[localName]; exists {
				return "", fmt.Errorf("duplicate import local name %q (%q and %q)", localName, previous, importPath)
			}
			seenLocalNames[localName] = importPath
		}
		if importPath == "context" && localName != "_" {
			contextAlias = localName
		}

		imported, err := build.Default.Import(importPath, "", build.FindOnly)
		if err == nil && imported.Goroot {
			continue
		}
		if dependencyDeclared(importPath, dependencies) {
			continue
		}
		return "", fmt.Errorf("import must be from the Go standard library or declared dependencies: %q", importPath)
	}
	return contextAlias, nil
}

func dependencyDeclared(importPath string, dependencies []string) bool {
	for _, dependency := range dependencies {
		module, _, _ := strings.Cut(dependency, "@")
		if importPath == module || strings.HasPrefix(importPath, module+"/") {
			return true
		}
	}
	return false
}

func requiredTypeSpec(typeDecls map[string]*ast.TypeSpec, name string) (*ast.TypeSpec, error) {
	spec, ok := typeDecls[name]
	if !ok {
		return nil, fmt.Errorf("type %s is required", name)
	}
	if spec.Assign.IsValid() || (spec.TypeParams != nil && spec.TypeParams.NumFields() > 0) {
		return nil, fmt.Errorf("%s must be a defined type", name)
	}
	return spec, nil
}

func resolvesToStruct(typeDecls map[string]*ast.TypeSpec, expr ast.Expr, visiting map[string]bool) bool {
	expr = unwrapParens(expr)
	switch typed := expr.(type) {
	case *ast.StructType:
		return true
	case *ast.Ident:
		if visiting[typed.Name] {
			return false
		}
		spec, ok := typeDecls[typed.Name]
		if !ok || spec.Assign.IsValid() {
			return false
		}
		visiting[typed.Name] = true
		defer delete(visiting, typed.Name)
		return resolvesToStruct(typeDecls, spec.Type, visiting)
	default:
		return false
	}
}

func validateInvoke(file *ast.File, contextAlias string) error {
	var invoke *ast.FuncDecl
	for _, declaration := range file.Decls {
		fn, ok := declaration.(*ast.FuncDecl)
		if ok && fn.Recv == nil && fn.Name.Name == "Invoke" {
			if invoke != nil {
				return fmt.Errorf("duplicate Invoke declaration")
			}
			invoke = fn
		}
	}
	if invoke == nil {
		return fmt.Errorf("Invoke must have signature func(context.Context, Input) (ToolResponse[Output], error)")
	}
	if invoke.Type.TypeParams != nil && invoke.Type.TypeParams.NumFields() > 0 {
		return fmt.Errorf("Invoke must have signature func(context.Context, Input) (ToolResponse[Output], error)")
	}
	if hasVariadicParam(invoke.Type.Params) {
		return fmt.Errorf("Invoke must have signature func(context.Context, Input) (ToolResponse[Output], error)")
	}

	params := expandFieldTypes(invoke.Type.Params)
	results := expandFieldTypes(invoke.Type.Results)
	if len(params) != 2 || len(results) != 2 || contextAlias == "" {
		return fmt.Errorf("Invoke must have signature func(context.Context, Input) (ToolResponse[Output], error)")
	}
	if !isContextTypeExpr(params[0], contextAlias) || !isIdentExpr(params[1], "Input") || !isIdentExpr(results[1], "error") {
		return fmt.Errorf("Invoke must have signature func(context.Context, Input) (ToolResponse[Output], error)")
	}
	if !isToolResponseOfOutput(results[0]) {
		return fmt.Errorf("Invoke must return ToolResponse[Output]")
	}
	return nil
}

func hasVariadicParam(fields *ast.FieldList) bool {
	if fields == nil {
		return false
	}
	for _, field := range fields.List {
		if _, ok := field.Type.(*ast.Ellipsis); ok {
			return true
		}
	}
	return false
}

func expandFieldTypes(fields *ast.FieldList) []ast.Expr {
	if fields == nil {
		return nil
	}
	var out []ast.Expr
	for _, field := range fields.List {
		count := len(field.Names)
		if count == 0 {
			count = 1
		}
		for range count {
			out = append(out, field.Type)
		}
	}
	return out
}

func isContextTypeExpr(expr ast.Expr, contextAlias string) bool {
	sel, ok := unwrapParens(expr).(*ast.SelectorExpr)
	if !ok {
		return false
	}
	return isIdentExpr(sel.X, contextAlias) && sel.Sel.Name == "Context"
}

func isIdentExpr(expr ast.Expr, name string) bool {
	ident, ok := unwrapParens(expr).(*ast.Ident)
	return ok && ident.Name == name
}

func isToolResponseOfOutput(expr ast.Expr) bool {
	switch typed := unwrapParens(expr).(type) {
	case *ast.IndexExpr:
		return isIdentExpr(typed.X, "ToolResponse") && isIdentExpr(typed.Index, "Output")
	case *ast.IndexListExpr:
		return isIdentExpr(typed.X, "ToolResponse") && len(typed.Indices) == 1 && isIdentExpr(typed.Indices[0], "Output")
	default:
		return false
	}
}

type astTypeMapper struct {
	typeDecls   map[string]*ast.TypeSpec
	wireMethods map[string]bool
	visiting    map[string]bool
	quotePaths  *[][]string
}

func (m astTypeMapper) mapNamedSpec(spec *ast.TypeSpec, path string, jsonPath []string) (cty.Type, error) {
	name := spec.Name.Name
	if m.visiting[name] {
		return cty.NilType, fmt.Errorf("%s: recursive Go types are not supported", path)
	}
	if m.wireMethods[name] {
		return cty.NilType, fmt.Errorf("%s: custom JSON or text methods are not supported", path)
	}
	if name == "Quote" {
		if !isStringUnderlying(m.typeDecls, spec.Type, make(map[string]bool)) {
			return cty.NilType, fmt.Errorf("%s: Quote must have string underlying type", path)
		}
		if m.quotePaths != nil {
			*m.quotePaths = append(*m.quotePaths, slices.Clone(jsonPath))
		}
	}
	m.visiting[name] = true
	defer delete(m.visiting, name)
	return m.mapExpr(spec.Type, path, jsonPath)
}

func isStringUnderlying(typeDecls map[string]*ast.TypeSpec, expr ast.Expr, visiting map[string]bool) bool {
	expr = unwrapParens(expr)
	ident, ok := expr.(*ast.Ident)
	if !ok {
		return false
	}
	if ident.Name == "string" {
		return true
	}
	if visiting[ident.Name] {
		return false
	}
	spec, ok := typeDecls[ident.Name]
	if !ok {
		return false
	}
	visiting[ident.Name] = true
	defer delete(visiting, ident.Name)
	return isStringUnderlying(typeDecls, spec.Type, visiting)
}

func usesDefinedType(typeDecls map[string]*ast.TypeSpec, expr ast.Expr, target string, visiting map[string]bool) bool {
	ident, ok := unwrapParens(expr).(*ast.Ident)
	if !ok {
		return false
	}
	if ident.Name == target {
		return true
	}
	if visiting[ident.Name] {
		return false
	}
	spec, ok := typeDecls[ident.Name]
	if !ok {
		return false
	}
	visiting[ident.Name] = true
	defer delete(visiting, ident.Name)
	return usesDefinedType(typeDecls, spec.Type, target, visiting)
}

func (m astTypeMapper) mapExpr(expr ast.Expr, path string, jsonPath []string) (cty.Type, error) {
	expr = unwrapParens(expr)
	switch typed := expr.(type) {
	case *ast.Ident:
		if mapped, ok := mapBasicIdent(typed.Name); ok {
			return mapped, nil
		}
		if spec, ok := m.typeDecls[typed.Name]; ok {
			if spec.Assign.IsValid() {
				return cty.NilType, fmt.Errorf("%s: type aliases are not supported (%s)", path, typed.Name)
			}
			return m.mapNamedSpec(spec, path, jsonPath)
		}
		return cty.NilType, fmt.Errorf("%s: unsupported Go type %s", path, typed.Name)
	case *ast.SelectorExpr:
		return cty.NilType, fmt.Errorf("%s: imported named Go types are not supported", path)
	case *ast.ArrayType:
		if typed.Len == nil {
			if isByteTypeExpr(m.typeDecls, typed.Elt, make(map[string]bool)) {
				return cty.NilType, fmt.Errorf("%s: []byte has JSON string semantics and is not supported", path)
			}
			elemType, err := m.mapExpr(typed.Elt, path+"[]", appendJSONPath(jsonPath, "[]"))
			if err != nil {
				return cty.NilType, err
			}
			return cty.List(elemType), nil
		}
		length, ok := arrayLen(typed.Len)
		if !ok {
			return cty.NilType, fmt.Errorf("%s: array length must be a non-negative integer literal", path)
		}
		elemType, err := m.mapExpr(typed.Elt, path+"[]", appendJSONPath(jsonPath, "[]"))
		if err != nil {
			return cty.NilType, err
		}
		elements := make([]cty.Type, length)
		for index := range elements {
			elements[index] = elemType
		}
		return cty.Tuple(elements), nil
	case *ast.MapType:
		if usesDefinedType(m.typeDecls, typed.Key, "Quote", make(map[string]bool)) {
			return cty.NilType, fmt.Errorf("%s: Quote type is not allowed as map key", path)
		}
		keyMapper := m
		keyMapper.quotePaths = nil
		keyType, err := keyMapper.mapExpr(typed.Key, path+".<key>", appendJSONPath(jsonPath, "*"))
		if err != nil {
			return cty.NilType, err
		}
		if !keyType.Equals(cty.String) {
			return cty.NilType, fmt.Errorf("%s: map keys must be strings", path)
		}
		elemType, err := m.mapExpr(typed.Value, path+".*", appendJSONPath(jsonPath, "*"))
		if err != nil {
			return cty.NilType, err
		}
		return cty.Map(elemType), nil
	case *ast.StructType:
		return m.mapStruct(typed, path, jsonPath)
	default:
		return cty.NilType, fmt.Errorf("%s: unsupported Go type %T", path, expr)
	}
}

func (m astTypeMapper) mapStruct(structType *ast.StructType, path string, jsonPath []string) (cty.Type, error) {
	attributes := make(map[string]cty.Type)
	optional := make([]string, 0)
	if structType.Fields == nil {
		return cty.ObjectWithOptionalAttrs(attributes, optional), nil
	}

	for _, field := range structType.Fields.List {
		if len(field.Names) == 0 {
			return cty.NilType, fmt.Errorf("%s.%s: fields must be exported and non-embedded", path, embeddedFieldLabel(field.Type))
		}
		tagString := ""
		if field.Tag != nil {
			unquoted, err := strconv.Unquote(field.Tag.Value)
			if err != nil {
				return cty.NilType, fmt.Errorf("%s: invalid struct tag: %w", path, err)
			}
			tagString = unquoted
		}
		for _, ident := range field.Names {
			if !ast.IsExported(ident.Name) {
				return cty.NilType, fmt.Errorf("%s.%s: fields must be exported and non-embedded", path, ident.Name)
			}
			name, omitted, omitempty, err := jsonField(ident.Name, tagString, path)
			if err != nil {
				return cty.NilType, err
			}
			if omitted {
				continue
			}
			fieldExpr := unwrapParens(field.Type)
			if star, ok := fieldExpr.(*ast.StarExpr); ok {
				fieldExpr = star.X
				omitempty = true
			}
			if _, exists := attributes[name]; exists {
				return cty.NilType, fmt.Errorf("%s: duplicate JSON field %q", path, name)
			}
			mapped, err := m.mapExpr(fieldExpr, path+"."+name, appendJSONPath(jsonPath, name))
			if err != nil {
				return cty.NilType, err
			}
			attributes[name] = mapped
			if omitempty {
				optional = append(optional, name)
			}
		}
	}
	return cty.ObjectWithOptionalAttrs(attributes, optional), nil
}

func embeddedFieldLabel(expr ast.Expr) string {
	expr = unwrapParens(expr)
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = unwrapParens(star.X)
	}
	switch typed := expr.(type) {
	case *ast.Ident:
		return typed.Name
	case *ast.SelectorExpr:
		return typed.Sel.Name
	default:
		return "embedded"
	}
}

func mapBasicIdent(name string) (cty.Type, bool) {
	switch name {
	case "bool":
		return cty.Bool, true
	case "string":
		return cty.String, true
	case "int", "int8", "int16", "int32", "int64",
		"uint", "uint8", "uint16", "uint32", "uint64", "uintptr",
		"float32", "float64", "byte", "rune":
		return cty.Number, true
	default:
		return cty.NilType, false
	}
}

func isByteTypeExpr(typeDecls map[string]*ast.TypeSpec, expr ast.Expr, visiting map[string]bool) bool {
	ident, ok := unwrapParens(expr).(*ast.Ident)
	if !ok {
		return false
	}
	if ident.Name == "byte" || ident.Name == "uint8" {
		return true
	}
	if visiting[ident.Name] {
		return false
	}
	spec, ok := typeDecls[ident.Name]
	if !ok {
		return false
	}
	visiting[ident.Name] = true
	defer delete(visiting, ident.Name)
	return isByteTypeExpr(typeDecls, spec.Type, visiting)
}

func arrayLen(expr ast.Expr) (int, bool) {
	lit, ok := unwrapParens(expr).(*ast.BasicLit)
	if !ok || lit.Kind != token.INT {
		return 0, false
	}
	n, err := strconv.Atoi(lit.Value)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

func unwrapParens(expr ast.Expr) ast.Expr {
	for {
		paren, ok := expr.(*ast.ParenExpr)
		if !ok {
			return expr
		}
		expr = paren.X
	}
}

func appendJSONPath(path []string, segment string) []string {
	result := make([]string, 0, len(path)+1)
	result = append(result, path...)
	return append(result, segment)
}

func jsonField(fallback, tag, path string) (string, bool, bool, error) {
	value, ok := reflect.StructTag(tag).Lookup("json")
	if !ok {
		return fallback, false, false, nil
	}
	if value == "-" {
		return "", true, false, nil
	}
	parts := strings.Split(value, ",")
	name := parts[0]
	if !validJSONFieldName(name) {
		name = fallback
	}
	for _, option := range parts[1:] {
		if option != "" && option != "omitempty" {
			return "", false, false, fmt.Errorf("%s.%s: unsupported JSON tag option %q", path, name, option)
		}
	}
	return name, false, slices.Contains(parts[1:], "omitempty"), nil
}

func validJSONFieldName(name string) bool {
	if name == "" {
		return false
	}
	for _, character := range name {
		allowedPunctuation := strings.ContainsRune("!#$%&()*+-./:;<=>?@[]^_{|}~ ", character)
		if !allowedPunctuation && !unicode.IsLetter(character) && !unicode.IsDigit(character) {
			return false
		}
	}
	return true
}
