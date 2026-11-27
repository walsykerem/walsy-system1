package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const fix4_13Endpoint = "https://evren-llmapi.ssyz.org.tr/v1/systemone"

// fix4_13BakedDefaults are the ten user-editable UI registry defaults, not SDK dependencies.
var fix4_13BakedDefaults = map[string]bool{
	"https://openrouter.ai/api/v1/systemone":                                                   true,
	"https://ollama.com/v1/systemone":                                                          true,
	"https://v1m.ir/v1/systemone":                                                              true,
	"https://evren-llmapi.ssyz.org.tr/v1/systemone":                                            true,
	"https://api.commandcode.ai/provider/v1/systemone":                                         true,
	"https://console.nace.ai/v1/systemone":                                                     true,
	"http://localhost:11434/v1/systemone":                                                      true,
	"http://127.0.0.1:8788/v1/systemone":                                                       true,
	"https://api.cloudflare.com/client/v4/accounts/YOUR_ACCOUNT_ID/ai/run/@cf/cloudflare/clef": true,
	"https://opencode.ai/zen/v1/systemone":                                                     true,
}

func fix4_13KnownProviderHost(host string) bool {
	switch strings.ToLower(host) {
	case "openrouter.ai", "ollama.com", "v1m.ir", "evren-llmapi.ssyz.org.tr",
		"api.commandcode.ai", "console.nace.ai", "localhost", "localhost:11434",
		"127.0.0.1", "127.0.0.1:8788", "api.cloudflare.com", "opencode.ai",
		"api.openai.com", "api.anthropic.com":
		return true
	}
	return false
}

func fix4_13ProviderAPIValue(v string) bool {
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	return fix4_13KnownProviderHost(u.Host) || strings.HasSuffix(u.Path, "/v1/systemone") || strings.Contains(u.Path, "/ai/run/")
}

// fix4_13Safe drops userinfo/query/fragment so no credential can reach test output.
func fix4_13Safe(v string) string {
	u, err := url.Parse(v)
	if err != nil {
		return "<unparseable-url>"
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	if len(u.Path) > 120 {
		u.Path = u.Path[:120] + "..."
	}
	return u.String()
}

func fix4_13Invariant(sources map[string]string, gomod string) []string {
	var bad []string
	paths := make([]string, 0, len(sources))
	for p := range sources {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	fset := token.NewFileSet()
	for _, p := range paths {
		f, err := parser.ParseFile(fset, p, sources[p], 0)
		if err != nil {
			bad = append(bad, p+": parse error: "+err.Error())
			continue
		}
		for _, imp := range f.Imports {
			ip, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				bad = append(bad, p+": bad import literal")
				continue
			}
			first := ip
			if i := strings.Index(ip, "/"); i > 0 {
				first = ip[:i]
			}
			if strings.Contains(first, ".") {
				bad = append(bad, p+": external dependency import "+ip)
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			v, err := strconv.Unquote(lit.Value)
			if err != nil || !fix4_13ProviderAPIValue(v) {
				return true
			}
			if v == fix4_13Endpoint {
				return true
			}
			if p == "internal/app/provider.go" && fix4_13BakedDefaults[v] {
				return true
			}
			bad = append(bad, p+": unexpected provider endpoint "+fix4_13Safe(v))
			return true
		})
	}
	for _, line := range strings.Split(gomod, "\n") {
		s := strings.TrimSpace(line)
		if s == "" || strings.HasPrefix(s, "//") || strings.HasPrefix(s, "go ") || s == "module walsy-system1" {
			continue
		}
		if strings.HasPrefix(s, "require") || strings.HasPrefix(s, "replace") || strings.HasPrefix(s, "exclude") {
			bad = append(bad, "go.mod: unexpected dependency directive "+s)
		}
	}
	sort.Strings(bad)
	return bad
}

func fix4_13Root(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func fix4_13Sources(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "testdata", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") || strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestFix4_13ProviderSDKInvariant(t *testing.T) {
	root := fix4_13Root(t)
	sources := fix4_13Sources(t, root)
	gomod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if bad := fix4_13Invariant(sources, string(gomod)); len(bad) > 0 {
		t.Fatalf("unexpected provider SDK surface:\n%s", strings.Join(bad, "\n"))
	}
	for _, p := range []string{"internal/app/gate.go", "scripts/releasegate/main.go", "scripts/finalgate/main.go"} {
		if !strings.Contains(sources[p], fix4_13Endpoint) {
			t.Errorf("%s: canonical System One endpoint missing", p)
		}
	}
	if !strings.Contains(sources["internal/app/gate.go"], "Questions map[string]soQuestion") {
		t.Error("internal/app/gate.go: questions object body missing")
	}
	if !strings.Contains(sources["internal/app/keytest.go"], "\"questions\": map[string]soQuestion") {
		t.Error("internal/app/keytest.go: SO probe questions object missing")
	}
	if soProbeModel != "clef" {
		t.Errorf("soProbeModel = %q, want clef", soProbeModel)
	}
	if !strings.Contains(string(gomod), "module walsy-system1\n") {
		t.Error("go.mod: standalone module assertion failed")
	}
	for _, name := range []string{"go.sum", filepath.Join("vendor", "modules.txt")} {
		if _, err := os.Stat(filepath.Join(root, name)); err == nil {
			t.Errorf("%s: unexpected dependency lockfile", name)
		}
	}
}

func TestFix4_13DetectorCatchesInjectedSDK(t *testing.T) {
	synthetic := map[string]string{
		"internal/app/provider.go": "package app\n",
		"internal/app/fake_sdk.go": "package app\nimport \"github.com/example/so-sdk\"\nvar endpoint = \"https://new.example/v1/systemone\"\n",
	}
	bad := fix4_13Invariant(synthetic, "module walsy-system1\ngo 1.26\n")
	joined := strings.Join(bad, "\n")
	if !strings.Contains(joined, "github.com/example/so-sdk") {
		t.Fatalf("external SDK import not detected: %q", joined)
	}
	if !strings.Contains(joined, "new.example/v1/systemone") {
		t.Fatalf("unexpected provider endpoint not detected: %q", joined)
	}
	if strings.Contains(joined, "x-api-key") || strings.Contains(joined, "sk-") {
		t.Fatal("sanitized output leaked secret-shaped text")
	}
	if len(bad) < 2 {
		t.Fatalf("invariant detected only %d violations, want import + endpoint", len(bad))
	}
}
