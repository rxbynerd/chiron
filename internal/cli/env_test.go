package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

var (
	// envNameShape matches the spelling of an environment variable name:
	// upper case with at least one underscore.
	envNameShape = regexp.MustCompile(`^[A-Z][A-Z0-9]*(_[A-Z0-9]+)+$`)
	// secretEnvRef matches a secret:// reference to the env backend, which
	// reads the variable it names.
	secretEnvRef = regexp.MustCompile(`^secret://([A-Za-z_][A-Za-z0-9_]*)$`)
)

// envNamesIn returns the environment variable names that files refer to:
// the argument of every os.Getenv and os.LookupEnv call that is a string
// literal or a constant declared in files, every string literal spelled
// like a variable name, and every secret:// reference to the env backend.
// Reads whose name is computed are found through the literals that supply
// it.
func envNamesIn(files []*ast.File) []string {
	consts := map[string]string{}
	for _, f := range files {
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, ident := range vs.Names {
					if i < len(vs.Values) {
						if s, ok := stringLiteral(vs.Values[i]); ok {
							consts[ident.Name] = s
						}
					}
				}
			}
		}
	}

	found := map[string]bool{}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.BasicLit:
				s, ok := stringLiteral(n)
				if !ok {
					return true
				}
				if envNameShape.MatchString(s) {
					found[s] = true
				}
				if m := secretEnvRef.FindStringSubmatch(s); m != nil {
					found[m[1]] = true
				}
			case *ast.CallExpr:
				if name, ok := envReadArgument(n, consts); ok {
					found[name] = true
				}
			}
			return true
		})
	}

	names := make([]string, 0, len(found))
	for name := range found {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// envReadArgument returns the variable name an os.Getenv or os.LookupEnv
// call reads when its argument is a literal or a known constant.
func envReadArgument(call *ast.CallExpr, consts map[string]string) (string, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || len(call.Args) != 1 {
		return "", false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != "os" || (sel.Sel.Name != "Getenv" && sel.Sel.Name != "LookupEnv") {
		return "", false
	}
	if s, ok := stringLiteral(call.Args[0]); ok {
		return s, true
	}
	var constName string
	switch arg := call.Args[0].(type) {
	case *ast.Ident:
		constName = arg.Name
	case *ast.SelectorExpr:
		constName = arg.Sel.Name
	}
	s, ok := consts[constName]
	return s, ok
}

func stringLiteral(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	return s, err == nil
}

func parseSource(t *testing.T, fset *token.FileSet, filename string, src any) *ast.File {
	t.Helper()
	must := require.New(t)
	f, err := parser.ParseFile(fset, filename, src, 0)
	must.NoError(err)
	return f
}

// TestAmbientEnv_CoversEveryProductionRead keeps the variables TestMain
// clears from going stale: every variable the production code of this
// package and of internal/config refers to must be one TestMain removes.
func TestAmbientEnv_CoversEveryProductionRead(t *testing.T) {
	must := require.New(t)

	fset := token.NewFileSet()
	var files []*ast.File
	for _, dir := range []string{".", "../config"} {
		paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
		must.NoError(err)
		for _, path := range paths {
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			files = append(files, parseSource(t, fset, path, nil))
		}
	}

	names := envNamesIn(files)
	must.Contains(names, "OTEL_EXPORTER_OTLP_ENDPOINT", "the scan found no environment reads")

	var uncovered []string
	for _, name := range names {
		if !clearedByTestMain(name) {
			uncovered = append(uncovered, name)
		}
	}
	must.Empty(uncovered, "production code refers to environment variables that TestMain does not clear; add them to ambientEnvNames (or a prefix to ambientEnvPrefixes) in main_test.go")
}

func TestEnvNamesIn_FindsEveryKindOfReference(t *testing.T) {
	must := require.New(t)

	const src = `package p

import "os"

const named = "NAMED_READ"

const (
	grouped = "GROUPED_READ"
	home    = "HOME"
)

var _ = []string{"LISTED_NAME", "lower_case", "single"}

func reads(dynamic string) {
	_ = os.Getenv("LITERAL")
	_, _ = os.LookupEnv(named)
	_ = os.Getenv(pkg.home)
	_ = os.Getenv(dynamic)
	_ = "secret://REFERENCED_KEY"
	_ = "secret://file/not-env"
}
`
	fset := token.NewFileSet()
	f := parseSource(t, fset, "p.go", src)

	must.Equal([]string{"GROUPED_READ", "HOME", "LISTED_NAME", "LITERAL", "NAMED_READ", "REFERENCED_KEY"}, envNamesIn([]*ast.File{f}))
}

func TestClearedByTestMain_CoversPrefixesAndNames(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"OTEL_EXPORTER_OTLP_ENDPOINT", true},
		{"CHIRON_NEW_VARIABLE", true},
		{"GEMINI_API_KEY", true},
		{"LANGFUSE_SECRET_KEY", true},
		{"PATH", false},
		{"HOME", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)

			must.Equal(tt.want, clearedByTestMain(tt.name))
		})
	}
}
