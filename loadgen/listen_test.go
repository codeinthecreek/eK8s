package loadgen

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// listenCalls returns the position of every call in the non-test .go
// files of dir to a function or method whose name starts with Listen
// (net.Listen*, (*net.ListenConfig).Listen*, http.ListenAndServe* and
// (*http.Server).ListenAndServe*), whatever its package is imported as.
// Listener is a type, not a listen, so it is not counted.
func listenCalls(t *testing.T, fset *token.FileSet, dir string) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	var sites []string
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			var name string
			switch fun := call.Fun.(type) {
			case *ast.SelectorExpr:
				name = fun.Sel.Name
			case *ast.Ident: // a dot import
				name = fun.Name
			}
			if strings.HasPrefix(name, "Listen") && name != "Listener" {
				sites = append(sites, fset.Position(call.Pos()).String()+" "+name)
			}
			return true
		})
	}
	return sites
}

// TestSingleListener checks that loadgen's build opens exactly one
// listener: the control API's on 9090 (cmd/loadgen's listen). A second
// would be a network service the NetworkPolicy and the kind checks do
// not cover (DESIGN.md sec 10).
func TestSingleListener(t *testing.T) {
	fset := token.NewFileSet()
	var sites []string
	for _, dir := range []string{".", filepath.Join("cmd", "loadgen")} {
		sites = append(sites, listenCalls(t, fset, dir)...)
	}
	if len(sites) != 1 || !strings.HasPrefix(sites[0], filepath.Join("cmd", "loadgen", "main.go")+":") {
		t.Errorf("listen call sites %q, want exactly one, in cmd/loadgen/main.go", sites)
	}
}
