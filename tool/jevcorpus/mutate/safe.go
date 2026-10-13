// Copyright (c) 2026 Uber Technologies, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package mutate

import (
	"go/ast"
	"go/token"
	"math/rand"
	"path"
	"regexp"
	"strings"
)

// RenameFunc renames an unexported function and every use of it.
var RenameFunc = Mutator{Name: "rename-func", Intent: IntentSafe, Scope: ScopePackage, apply: renameFunc}

// RenameLocals renames local variables in some of a file's functions.
var RenameLocals = Mutator{Name: "rename-locals", Intent: IntentSafe, Scope: ScopeFile, apply: renameLocals}

// DocumentFuncs adds doc comments to undocumented functions.
var DocumentFuncs = Mutator{Name: "document-funcs", Intent: IntentSafe, Scope: ScopeFile, apply: documentFuncs}

// DeleteDeadFunc deletes an unexported function nothing references.
var DeleteDeadFunc = Mutator{Name: "delete-dead-func", Intent: IntentSafe, Scope: ScopePackage, apply: deleteDeadFunc}

// AddHelper adds a small, tested, unused helper in a new file.
var AddHelper = Mutator{Name: "add-helper", Intent: IntentSafe, Scope: ScopePackage, apply: addHelper}

// hasDirective reports whether a doc comment carries a compiler or cgo
// directive, which can reference a function by name from outside Go.
func hasDirective(doc *ast.CommentGroup) bool {
	if doc == nil {
		return false
	}
	for _, c := range doc.List {
		if strings.HasPrefix(c.Text, "//go:") || strings.HasPrefix(c.Text, "//export") {
			return true
		}
	}
	return false
}

var renamedFuncSuffixes = []string{"Impl", "Internal", "Helper", "Once"}

func renameFunc(t Tree, dir string, r *rand.Rand) (Patch, bool) {
	files := packageFiles(t, dir)
	counts := map[string]int{}
	for _, pf := range files {
		declarationCounts(pf.file, counts)
	}
	names := allIdentNames(files)
	var sites []funcSite
	for _, s := range unexportedFuncs(files) {
		if counts[s.decl.Name.Name] == 1 && !hasDirective(s.decl.Doc) {
			sites = append(sites, s)
		}
	}
	if len(sites) == 0 {
		return nil, false
	}
	s := sites[r.Intn(len(sites))]
	old := s.decl.Name.Name
	renamed := ""
	for _, i := range r.Perm(len(renamedFuncSuffixes)) {
		if cand := old + renamedFuncSuffixes[i]; !names[cand] {
			renamed = cand
			break
		}
	}
	if renamed == "" {
		return nil, false
	}

	out := Patch{}
	word := regexp.MustCompile(`\b` + regexp.QuoteMeta(old) + `\b`)
	for _, pf := range files {
		sel := selectorNames(pf.file)
		var edits []textEdit
		ast.Inspect(pf.file, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && id.Name == old && !sel[id] {
				at := pf.offset(id.Pos())
				edits = append(edits, textEdit{start: at, end: at + len(old), text: renamed})
			}
			return true
		})
		if pf.path == s.pf.path && s.decl.Doc != nil {
			start, end := pf.offset(s.decl.Doc.Pos()), pf.offset(s.decl.Doc.End())
			edits = append(edits, textEdit{start: start, end: end, text: word.ReplaceAllString(pf.src[start:end], renamed)})
		}
		if len(edits) > 0 {
			out[pf.path] = splice(pf.src, edits)
		}
	}
	return out, true
}

// shortLocalNames maps terse local names to the longer spellings a rename
// sweep would choose.
var shortLocalNames = map[string]string{
	"i": "idx", "j": "jdx", "n": "count", "s": "str", "b": "buf", "k": "key",
	"v": "val", "c": "cur", "m": "mapping", "r": "reader", "w": "writer",
	"p": "ptr", "t": "tmp", "e": "elem", "f": "fn", "x": "item", "res": "result",
	"req": "request", "resp": "response", "cfg": "config", "msg": "message",
	"buf": "buffer", "idx": "index", "tmp": "scratch", "val": "value",
}

func renamedLocal(name string) string {
	if long, ok := shortLocalNames[name]; ok {
		return long
	}
	return name + "Value"
}

func renameLocals(t Tree, target string, r *rand.Rand) (Patch, bool) {
	pf, ok := parseTarget(t, target)
	if !ok {
		return nil, false
	}
	topLevel := map[string]int{}
	for _, f := range packageFiles(t, path.Dir(target)) {
		for _, d := range f.file.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				if d.Recv == nil {
					topLevel[d.Name.Name]++
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch spec := spec.(type) {
					case *ast.TypeSpec:
						topLevel[spec.Name.Name]++
					case *ast.ValueSpec:
						for _, id := range spec.Names {
							topLevel[id.Name]++
						}
					}
				}
			}
		}
	}
	for _, imp := range pf.file.Imports {
		topLevel[importName(imp)]++
	}
	fileNames := allIdentNames([]parsedFile{pf})
	sel := selectorNames(pf.file)

	var edits []textEdit
	var eligible [][]textEdit
	for _, d := range pf.file.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		if fe := localRenames(pf, fd, topLevel, fileNames, sel); len(fe) > 0 {
			eligible = append(eligible, fe)
		}
	}
	if len(eligible) == 0 {
		return nil, false
	}
	for i, fe := range eligible {
		if i == 0 || r.Intn(2) == 0 {
			edits = append(edits, fe...)
		}
	}
	return Patch{pf.path: splice(pf.src, edits)}, true
}

// localRenames returns the edits renaming up to three of fd's locals, skipping
// any whose new or old spelling could collide with something else in scope.
func localRenames(pf parsedFile, fd *ast.FuncDecl, topLevel map[string]int, fileNames map[string]bool, sel map[*ast.Ident]bool) []textEdit {
	unsafeNames := map[string]bool{"_": true, "err": true, "ok": true}
	for _, fl := range []*ast.FieldList{fd.Recv, fd.Type.Params, fd.Type.Results, fd.Type.TypeParams} {
		if fl == nil {
			continue
		}
		for _, field := range fl.List {
			for _, n := range field.Names {
				unsafeNames[n.Name] = true
			}
		}
	}
	var defined []string
	seen := map[string]bool{}
	define := func(e ast.Expr) {
		if id, ok := e.(*ast.Ident); ok && !seen[id.Name] {
			seen[id.Name] = true
			defined = append(defined, id.Name)
		}
	}
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt:
			if n.Tok == token.DEFINE {
				for _, l := range n.Lhs {
					define(l)
				}
			}
		case *ast.RangeStmt:
			if n.Tok == token.DEFINE {
				define(n.Key)
				define(n.Value)
			}
		case *ast.Field:
			for _, id := range n.Names {
				unsafeNames[id.Name] = true
			}
		case *ast.KeyValueExpr:
			if id, ok := n.Key.(*ast.Ident); ok {
				unsafeNames[id.Name] = true
			}
		case *ast.LabeledStmt:
			unsafeNames[n.Label.Name] = true
		}
		return true
	})

	var edits []textEdit
	renamed := 0
	for _, name := range defined {
		if renamed == 3 {
			break
		}
		to := renamedLocal(name)
		if unsafeNames[name] || topLevel[name] > 0 || fileNames[to] || topLevel[to] > 0 {
			continue
		}
		fileNames[to] = true
		renamed++
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && id.Name == name && !sel[id] {
				at := pf.offset(id.Pos())
				edits = append(edits, textEdit{start: at, end: at + len(name), text: to})
			}
			return true
		})
	}
	return edits
}

var docVerbs = map[string]string{
	"get": "returns the", "new": "returns a new", "make": "makes the", "build": "builds the",
	"parse": "parses the", "load": "loads the", "read": "reads the", "write": "writes the",
	"handle": "handles the", "create": "creates the", "update": "updates the",
	"delete": "deletes the", "add": "adds the", "remove": "removes the", "find": "finds the",
	"list": "lists the", "run": "runs the", "start": "starts the", "stop": "stops the",
	"close": "closes the", "open": "opens the", "validate": "validates the",
	"check": "checks the", "apply": "applies the", "format": "formats the",
	"convert": "converts the", "set": "sets the", "to": "converts to", "with": "returns a copy with the",
	"is": "reports whether", "has": "reports whether it has", "can": "reports whether it can",
}

func docSentence(name string) string {
	words := splitWords(name)
	if len(words) == 0 {
		return "// " + name + " is part of this package's implementation."
	}
	if verb, ok := docVerbs[words[0]]; ok && len(words) > 1 {
		return "// " + name + " " + verb + " " + strings.Join(words[1:], " ") + "."
	}
	return "// " + name + " implements the " + strings.Join(words, " ") + " step."
}

func documentFuncs(t Tree, target string, r *rand.Rand) (Patch, bool) {
	pf, ok := parseTarget(t, target)
	if !ok {
		return nil, false
	}
	var undocumented []*ast.FuncDecl
	for _, d := range pf.file.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Doc == nil {
			undocumented = append(undocumented, fd)
		}
	}
	if len(undocumented) == 0 {
		return nil, false
	}
	var edits []textEdit
	for i, fd := range undocumented {
		if i > 0 && r.Intn(10) >= 7 {
			continue
		}
		at, _ := wholeLines(pf.src, pf.offset(fd.Pos()), pf.offset(fd.Pos()))
		edits = append(edits, textEdit{start: at, end: at, text: docSentence(fd.Name.Name) + "\n"})
	}
	return Patch{pf.path: splice(pf.src, edits)}, true
}

func deleteDeadFunc(t Tree, dir string, r *rand.Rand) (Patch, bool) {
	files := packageFiles(t, dir)
	var sites []funcSite
	for _, s := range unexportedFuncs(files) {
		if !hasDirective(s.decl.Doc) && funcReferences(files, s) == 0 {
			sites = append(sites, s)
		}
	}
	if len(sites) == 0 {
		return nil, false
	}
	return deleteFuncDecl(sites[r.Intn(len(sites))]), true
}

type helperTemplate struct {
	name, stem, src, test string
}

var helperTemplates = []helperTemplate{
	{
		name: "clampInt", stem: "clamp",
		src: `package {{pkg}}

// {{name}} limits v to the closed range [lo, hi].
func {{name}}(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
`,
		test: `package {{pkg}}

import "testing"

func {{test}}(t *testing.T) {
	cases := []struct{ v, lo, hi, want int }{
		{v: -1, lo: 0, hi: 10, want: 0},
		{v: 5, lo: 0, hi: 10, want: 5},
		{v: 11, lo: 0, hi: 10, want: 10},
	}
	for _, c := range cases {
		if got := {{name}}(c.v, c.lo, c.hi); got != c.want {
			t.Errorf("got %d, want %d", got, c.want)
		}
	}
}
`,
	},
	{
		name: "uniqueStrings", stem: "unique",
		src: `package {{pkg}}

// {{name}} returns values without duplicates, keeping first occurrences in
// order.
func {{name}}(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, v := range values {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}
`,
		test: `package {{pkg}}

import "testing"

func {{test}}(t *testing.T) {
	got := {{name}}([]string{"b", "a", "b", "c", "a"})
	want := []string{"b", "a", "c"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}
`,
	},
	{
		name: "chunkStrings", stem: "chunk",
		src: `package {{pkg}}

// {{name}} splits values into consecutive slices of at most size elements.
// A non-positive size returns nil.
func {{name}}(values []string, size int) [][]string {
	if size <= 0 {
		return nil
	}
	var out [][]string
	for start := 0; start < len(values); start += size {
		end := start + size
		if end > len(values) {
			end = len(values)
		}
		out = append(out, values[start:end])
	}
	return out
}
`,
		test: `package {{pkg}}

import "testing"

func {{test}}(t *testing.T) {
	got := {{name}}([]string{"a", "b", "c", "d", "e"}, 2)
	if len(got) != 3 || len(got[0]) != 2 || len(got[2]) != 1 || got[2][0] != "e" {
		t.Fatalf("unexpected chunks %v", got)
	}
	if {{name}}([]string{"a"}, 0) != nil {
		t.Fatal("non-positive size must return nil")
	}
}
`,
	},
	{
		name: "reversedStrings", stem: "reverse",
		src: `package {{pkg}}

// {{name}} returns a reversed copy of values, leaving values unchanged.
func {{name}}(values []string) []string {
	out := make([]string, len(values))
	for i, v := range values {
		out[len(values)-1-i] = v
	}
	return out
}
`,
		test: `package {{pkg}}

import "testing"

func {{test}}(t *testing.T) {
	in := []string{"a", "b", "c"}
	got := {{name}}(in)
	if got[0] != "c" || got[2] != "a" || in[0] != "a" {
		t.Fatalf("got %v from %v", got, in)
	}
}
`,
	},
}

func addHelper(t Tree, dir string, r *rand.Rand) (Patch, bool) {
	files := packageFiles(t, dir)
	if len(files) == 0 {
		return nil, false
	}
	pkg := files[0].file.Name.Name
	names := allIdentNames(files)
	for _, i := range r.Perm(len(helperTemplates)) {
		h := helperTemplates[i]
		test := "Test" + upperFirst(h.name)
		srcPath := path.Join(dir, h.stem+"_helper.go")
		testPath := path.Join(dir, h.stem+"_helper_test.go")
		_, srcExists := t.File(srcPath)
		_, testExists := t.File(testPath)
		if names[h.name] || names[test] || srcExists || testExists {
			continue
		}
		fill := strings.NewReplacer("{{pkg}}", pkg, "{{name}}", h.name, "{{test}}", test)
		return Patch{srcPath: fill.Replace(h.src), testPath: fill.Replace(h.test)}, true
	}
	return nil, false
}
