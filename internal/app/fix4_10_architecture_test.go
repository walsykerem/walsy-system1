package app

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

const (
	fix4ArchHarnessFile = "fix4_10_architecture_test.go"
	fix4ArchMinDupBody  = 40
)

var (
	fix4ArchSimNameRE = regexp.MustCompile("^fix4_[a-z0-9_]+_sim_test\\.go$")
	fix4ArchSpaceRE   = regexp.MustCompile("\\s+")
	fix4ArchCommentRE = regexp.MustCompile("(?s)//[^\\n]*|/\\*.*?\\*/")
)

type fix4ArchSource struct {
	path string
	file *ast.File
	fset *token.FileSet
}

type fix4ArchFunc struct {
	name string
	file string
	line int
	body string
}

func fix4ArchRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above working directory")
		}
		dir = parent
	}
}

func fix4ArchAppDir(t *testing.T) string {
	t.Helper()
	root := fix4ArchRepoRoot(t)
	app := filepath.Join(root, "internal", "app")
	if st, err := os.Stat(app); err == nil && st.IsDir() {
		return app
	}
	return root
}

func fix4ArchGoFiles(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", ".fast", "dist", "node_modules", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".go") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	return files
}

func fix4ArchParse(t *testing.T, files []string) []fix4ArchSource {
	t.Helper()
	var out []fix4ArchSource
	for _, path := range files {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", filepath.Base(path), err)
		}
		out = append(out, fix4ArchSource{path: path, file: f, fset: fset})
	}
	return out
}

func fix4ArchIsFix4(path string) bool {
	return strings.HasPrefix(filepath.Base(path), "fix4_")
}

func fix4ArchIsTest(path string) bool {
	return strings.HasSuffix(path, "_test.go")
}

func fix4ArchNormalizeBody(body string) string {
	body = fix4ArchCommentRE.ReplaceAllString(body, "")
	return fix4ArchSpaceRE.ReplaceAllString(body, "")
}

func fix4ArchFuncs(t *testing.T, sources []fix4ArchSource) []fix4ArchFunc {
	t.Helper()
	var out []fix4ArchFunc
	for _, s := range sources {
		for _, decl := range s.file.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			var buf bytes.Buffer
			if err := printer.Fprint(&buf, s.fset, fd.Body); err != nil {
				t.Fatalf("print %s: %v", filepath.Base(s.path), err)
			}
			out = append(out, fix4ArchFunc{
				name: fd.Name.Name,
				file: s.path,
				line: s.fset.Position(fd.Pos()).Line,
				body: fix4ArchNormalizeBody(buf.String()),
			})
		}
	}
	return out
}

func fix4ArchFuncRef(f fix4ArchFunc) string {
	return fmt.Sprintf("%s:%d:%s", filepath.Base(f.file), f.line, f.name)
}

func fix4ArchIdentCallName(n ast.Node) (string, bool) {
	call, ok := n.(*ast.CallExpr)
	if !ok {
		return "", false
	}
	if id, ok := call.Fun.(*ast.Ident); ok {
		return id.Name, true
	}
	if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
		return sel.Sel.Name, true
	}
	return "", false
}

func fix4ArchPkgCall(n ast.Node) (string, string, *ast.CallExpr, bool) {
	call, ok := n.(*ast.CallExpr)
	if !ok {
		return "", "", nil, false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", "", nil, false
	}
	id, ok := sel.X.(*ast.Ident)
	if !ok {
		return "", "", nil, false
	}
	return id.Name, sel.Sel.Name, call, true
}

func fix4ArchLoopbackHost(host string) bool {
	host = strings.Trim(host, "[]")
	return host == "localhost" || host == "127.0.0.1" || host == "::1" || strings.HasSuffix(host, ".localhost")
}

// shortcut: only literal network targets are checked; a dynamic endpoint builder needs taint analysis.
func fix4ArchExternalLiteral(s string) bool {
	if s == "" || strings.HasPrefix(s, "/") || strings.HasPrefix(s, "unix") {
		return false
	}
	if strings.Contains(s, "://") {
		u, err := url.Parse(s)
		if err != nil {
			return false
		}
		host := u.Hostname()
		return host != "" && !fix4ArchLoopbackHost(host)
	}
	host := s
	if i := strings.LastIndex(host, ":"); i >= 0 {
		host = host[:i]
	}
	if strings.ContainsAny(host, " /") {
		return false
	}
	return host != "" && !fix4ArchLoopbackHost(host)
}

func fix4ArchCheckLiteralTarget(t *testing.T, path string, fset *token.FileSet, n ast.Node, arg ast.Expr, callName string) {
	t.Helper()
	lit, ok := arg.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return
	}
	target := strings.Trim(lit.Value, "\"")
	if fix4ArchExternalLiteral(target) {
		t.Errorf("%s:%d %s targets non-loopback literal %s", filepath.Base(path), fset.Position(n.Pos()).Line, callName, target)
	}
}

func TestFix4_10_ArchitectureSingleThresholdSource(t *testing.T) {
	app := fix4ArchAppDir(t)
	sources := fix4ArchParse(t, fix4ArchGoFiles(t, app))
	decl := map[string][]string{}
	for _, s := range sources {
		for _, d := range s.file.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			loc := fmt.Sprintf("%s:%d", filepath.Base(s.path), s.fset.Position(fd.Pos()).Line)
			decl[fd.Name.Name] = append(decl[fd.Name.Name], loc)
		}
	}
	for _, name := range []string{"defaultThreshold", "confBucket", "bucketConf", "passesKeyConfRule", "routerAudit", "recordDecisionReason", "finalAudit"} {
		if locs := decl[name]; len(locs) != 1 {
			t.Errorf("%s declared %d times, want exactly 1 (%v)", name, len(locs), locs)
		}
	}
	for _, path := range fix4ArchGoFiles(t, app) {
		if !fix4ArchIsFix4(path) || fix4ArchIsTest(path) {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			if strings.Contains(line, "0.8") {
				t.Errorf("%s:%d raw confidence threshold literal in new fix4 production file; use defaultThreshold()/confBucket(): %s", filepath.Base(path), i+1, strings.TrimSpace(line))
			}
		}
	}
}

func TestFix4_10_ArchitectureNoDuplicateHelpers(t *testing.T) {
	app := fix4ArchAppDir(t)
	sources := fix4ArchParse(t, fix4ArchGoFiles(t, app))
	funcs := fix4ArchFuncs(t, sources)
	byName := map[string][]string{}
	for _, f := range funcs {
		if !fix4ArchIsFix4(f.file) {
			continue
		}
		byName[f.name] = append(byName[f.name], fix4ArchFuncRef(f))
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if locs := byName[name]; len(locs) > 1 {
			t.Errorf("duplicate fix4 helper %s at %v", name, locs)
		}
	}
	byBody := map[string][]fix4ArchFunc{}
	for _, f := range funcs {
		if len(f.body) < fix4ArchMinDupBody {
			continue
		}
		byBody[f.body] = append(byBody[f.body], f)
	}
	bodies := make([]string, 0, len(byBody))
	for body := range byBody {
		bodies = append(bodies, body)
	}
	sort.Strings(bodies)
	for _, body := range bodies {
		group := byBody[body]
		if len(group) < 2 {
			continue
		}
		var fix4Refs []string
		var refs []string
		for _, f := range group {
			refs = append(refs, fix4ArchFuncRef(f))
			if fix4ArchIsFix4(f.file) {
				fix4Refs = append(fix4Refs, fix4ArchFuncRef(f))
			}
		}
		if len(fix4Refs) > 0 {
			t.Errorf("duplicate implementation shared by %v (fix4: %v)", refs, fix4Refs)
		}
	}
}

func TestFix4_10_ArchitectureDecisionAuditCoupled(t *testing.T) {
	app := fix4ArchAppDir(t)
	sources := fix4ArchParse(t, fix4ArchGoFiles(t, app))
	for _, s := range sources {
		base := filepath.Base(s.path)
		if base != "route.go" {
			continue
		}
		decision, audit := false, false
		ast.Inspect(s.file, func(n ast.Node) bool {
			if kv, ok := n.(*ast.KeyValueExpr); ok {
				if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "Decision" {
					decision = true
				}
			}
			if name, ok := fix4ArchIdentCallName(n); ok && (name == "routerAudit" || name == "recordDecisionReason") {
				audit = true
			}
			return true
		})
		if !decision || !audit {
			t.Errorf("route.go lost decision/audit coupling: decision=%v audit=%v", decision, audit)
		}
	}
	for _, s := range sources {
		if !fix4ArchIsFix4(s.path) || fix4ArchIsTest(s.path) {
			continue
		}
		for _, d := range s.file.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			decision, audit := false, false
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if kv, ok := n.(*ast.KeyValueExpr); ok {
					if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "Decision" {
						decision = true
					}
				}
				if name, ok := fix4ArchIdentCallName(n); ok {
					switch name {
					case "routerAudit", "recordDecisionReason", "finalAudit", "emitMetric":
						audit = true
					}
				}
				return true
			})
			if decision && !audit {
				t.Errorf("%s:%d writes Decision without an audit record", filepath.Base(s.path), s.fset.Position(fd.Pos()).Line)
			}
		}
	}
}

func TestFix4_10_ArchitectureResponsibilityBoundaries(t *testing.T) {
	app := fix4ArchAppDir(t)
	for _, s := range fix4ArchParse(t, fix4ArchGoFiles(t, app)) {
		base := filepath.Base(s.path)
		isCache := strings.HasPrefix(base, "cache") && strings.HasSuffix(base, ".go") && !fix4ArchIsTest(base)
		isRouterPolicy := base == "router_policy.go"
		if isCache || isRouterPolicy {
			for _, imp := range s.file.Imports {
				path := strings.Trim(imp.Path.Value, "\"")
				if path == "net" || path == "net/http" || path == "net/http/httptest" {
					t.Errorf("%s imports %s; cache/policy must stay off the network layer", base, path)
				}
			}
		}
		if isRouterPolicy {
			ast.Inspect(s.file, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok {
					switch id.Name {
					case "decisionCache", "shardedCache", "newDecisionCache", "newShardedCache":
						t.Errorf("%s owns cache implementation %s", base, id.Name)
					}
				}
				return true
			})
		}
		if isCache {
			ast.Inspect(s.file, func(n ast.Node) bool {
				pkg, fn, _, ok := fix4ArchPkgCall(n)
				if ok && (pkg == "net" || pkg == "http") && (fn == "Listen" || fn == "ListenAndServe" || fn == "Dial") {
					t.Errorf("%s owns listener/dial %s.%s", base, pkg, fn)
				}
				return true
			})
		}
		if fix4ArchIsFix4(s.path) && !fix4ArchIsTest(s.path) {
			ast.Inspect(s.file, func(n ast.Node) bool {
				pkg, fn, _, ok := fix4ArchPkgCall(n)
				if ok && (pkg == "net" || pkg == "http") && (fn == "Listen" || fn == "ListenAndServe") && !strings.Contains(base, "transport") {
					t.Errorf("%s listens outside a transport file: %s.%s", base, pkg, fn)
				}
				return true
			})
		}
	}
}

func TestFix4_10_ArchitectureSimHarnessContract(t *testing.T) {
	app := fix4ArchAppDir(t)
	for _, path := range fix4ArchGoFiles(t, app) {
		base := filepath.Base(path)
		if !fix4ArchIsFix4(path) || !fix4ArchIsTest(path) || base == fix4ArchHarnessFile {
			continue
		}
		if !fix4ArchSimNameRE.MatchString(base) {
			t.Errorf("%s violates fix4_*_sim_test.go naming contract", base)
		}
		s := fix4ArchParse(t, []string{path})[0]
		hasTest := false
		for _, d := range s.file.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok {
				continue
			}
			if fd.Name.Name == "TestMain" {
				t.Errorf("%s declares TestMain; global harness owns it", base)
			}
			if strings.HasPrefix(fd.Name.Name, "TestFix4") && fd.Recv == nil && fd.Body != nil {
				hasTest = true
			}
		}
		if !hasTest {
			t.Errorf("%s has no TestFix4* simulation entry point", base)
		}
	}
}

func TestFix4_10_ArchitectureTestsUseNoExternalNetwork(t *testing.T) {
	root := fix4ArchRepoRoot(t)
	for _, path := range fix4ArchGoFiles(t, root) {
		if !fix4ArchIsTest(path) {
			continue
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", filepath.Base(path), err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			pkg, fn, call, ok := fix4ArchPkgCall(n)
			if !ok {
				return true
			}
			switch {
			case pkg == "http" && (fn == "Get" || fn == "Post" || fn == "PostForm" || fn == "Head"):
				if len(call.Args) > 0 {
					fix4ArchCheckLiteralTarget(t, path, fset, n, call.Args[0], pkg+"."+fn)
				}
			case pkg == "net" && (fn == "Dial" || fn == "DialTimeout"):
				if len(call.Args) < 2 {
					return true
				}
				if lit, ok := call.Args[0].(*ast.BasicLit); ok && strings.Trim(lit.Value, "\"") == "unix" {
					return true
				}
				fix4ArchCheckLiteralTarget(t, path, fset, n, call.Args[1], pkg+"."+fn)
			case pkg == "exec" && fn == "Command" && len(call.Args) > 0:
				prog := ""
				if lit, ok := call.Args[0].(*ast.BasicLit); ok {
					prog = strings.Trim(lit.Value, "\"")
				}
				if prog != "curl" && prog != "wget" {
					return true
				}
				for _, arg := range call.Args[1:] {
					if lit, ok := arg.(*ast.BasicLit); ok {
						target := strings.Trim(lit.Value, "\"")
						if strings.Contains(target, "://") && fix4ArchExternalLiteral(target) {
							t.Errorf("%s:%d exec %s targets non-loopback literal %s", filepath.Base(path), fset.Position(n.Pos()).Line, prog, target)
						}
					}
				}
			}
			return true
		})
	}
}

func TestFix4_10_ArchitectureCGOZeroStdlibOnly(t *testing.T) {
	root := fix4ArchRepoRoot(t)
	files := fix4ArchGoFiles(t, root)
	for _, s := range fix4ArchParse(t, files) {
		for _, imp := range s.file.Imports {
			if strings.Trim(imp.Path.Value, "\"") == "C" {
				t.Errorf("%s imports C; contract requires CGO_ENABLED=0", filepath.Base(s.path))
			}
		}
		for _, group := range s.file.Comments {
			for _, comment := range group.List {
				if strings.Contains(comment.Text, "go:build cgo") {
					t.Errorf("%s has a cgo build tag", filepath.Base(s.path))
				}
			}
		}
		if !fix4ArchIsTest(s.path) {
			data, err := os.ReadFile(s.path)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), "CGO_ENABLED=1") {
				t.Errorf("%s hardcodes CGO_ENABLED=1", filepath.Base(s.path))
			}
		}
	}
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	mod := string(data)
	if strings.Contains(mod, "require (") || strings.Contains(mod, "require ") {
		t.Errorf("go.mod must stay stdlib-only, got: %s", strings.TrimSpace(mod))
	}
}
