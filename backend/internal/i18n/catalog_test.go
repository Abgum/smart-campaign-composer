package i18n

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Every Turkish message the API can answer with has an English entry. The
// test reads the source of the packages whose messages reach API answers
// and checks each Turkish-looking string literal against the catalog: a
// message built by concatenation passes when its pieces are in an entry.
// Text that never reaches an answer (the model's prompts, the stored notes
// of a chat) lives in other packages and is not read.
func TestCatalog_CoversAPIMessages(t *testing.T) {
	sources := []struct {
		glob string
		only string // a single function to read, or "" for the whole file
	}{
		{"../httpapi/*.go", ""},
		{"../campaign/*.go", ""},
		{"../content/*.go", ""},
		{"../language/*.go", ""},
		{"../abtest/*.go", ""},
		{"../segment/*.go", ""},
		{"../geo/geo.go", ""},
		{"../analytics/*.go", ""},
		{"../tracking/*.go", ""},
		{"../chat/agentloop/agentloop.go", ""},
		{"../chat/turns/execute.go", "reason"},
		{"../llm/mock/demo.go", "Scenarios"},
	}
	// Turkish text in these files that is not an API message: version notes
	// stored with a campaign (the UI translates them) and the stand-in name a
	// rendered message uses.
	notMessages := map[string]bool{"Manuel oluşturuldu": true, "Manuel düzenlendi": true, "Değerli Müşterimiz": true}
	looksTurkish := regexp.MustCompile(`[çğıöşüÇĞİÖŞÜ]|\b(ister|olmalı|gerekli|olamaz|için|değil|yok|bulunamadı|kullanılamaz)\b`)
	// known: a piece of an entry, or a message printed from one
	known := func(s string) bool {
		for i, p := range catalog {
			if strings.Contains(p.TR, s) || (p.Src != "" && strings.Contains(p.Src, s)) {
				return true
			}
			if _, ok := entries[i].tr.match(s); ok {
				return true
			}
		}
		return false
	}
	fset := token.NewFileSet()
	checked := 0
	for _, src := range sources {
		files, _ := filepath.Glob(src.glob)
		if len(files) == 0 {
			t.Fatalf("no files for %s", src.glob)
		}
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			file, err := parser.ParseFile(fset, f, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			var roots []ast.Node
			for _, d := range file.Decls {
				switch d := d.(type) {
				case *ast.FuncDecl:
					if src.only == "" || d.Name.Name == src.only {
						roots = append(roots, d)
					}
				case *ast.GenDecl:
					if src.only == "" {
						roots = append(roots, d)
						continue
					}
					for _, sp := range d.Specs {
						if vs, ok := sp.(*ast.ValueSpec); ok && len(vs.Names) > 0 && vs.Names[0].Name == src.only {
							roots = append(roots, vs)
						}
					}
				}
			}
			for _, root := range roots {
				ast.Inspect(root, func(n ast.Node) bool {
					lit, ok := n.(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						return true
					}
					s, err := strconv.Unquote(lit.Value)
					if err != nil || !looksTurkish.MatchString(s) || notMessages[s] {
						return true
					}
					checked++
					if !known(strings.TrimSpace(s)) {
						t.Errorf("%s: no catalog entry for %q", fset.Position(lit.Pos()), s)
					}
					return true
				})
			}
		}
	}
	if checked < 80 {
		t.Errorf("read %d messages; the sources moved?", checked)
	}
}
