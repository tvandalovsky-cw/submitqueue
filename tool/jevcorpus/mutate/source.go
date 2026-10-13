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
	"go/format"
	"go/parser"
	"go/token"
	"sort"
	"strings"
	"unicode"
)

type parsedFile struct {
	path string
	src  string
	fset *token.FileSet
	file *ast.File
}

func parseFile(p, src string) (parsedFile, bool) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, p, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return parsedFile{}, false
	}
	return parsedFile{path: p, src: src, fset: fset, file: f}, true
}

func (pf parsedFile) offset(pos token.Pos) int { return pf.fset.File(pos).Offset(pos) }

// packageFiles parses the files in dir that belong to its primary package —
// the one its first non-test file declares — so an external _test package,
// which cannot see unexported names, is left out.
func packageFiles(t Tree, dir string) []parsedFile {
	var parsed []parsedFile
	for _, p := range t.GoFiles(dir) {
		src, _ := t.File(p)
		if pf, ok := parseFile(p, src); ok {
			parsed = append(parsed, pf)
		}
	}
	name := ""
	for _, pf := range parsed {
		if !IsTestFile(pf.path) {
			name = pf.file.Name.Name
			break
		}
	}
	if name == "" {
		return nil
	}
	out := parsed[:0]
	for _, pf := range parsed {
		if pf.file.Name.Name == name {
			out = append(out, pf)
		}
	}
	return out
}

type textEdit struct {
	start, end int
	text       string
}

// splice applies non-overlapping edits to src.
func splice(src string, edits []textEdit) string {
	sorted := append([]textEdit(nil), edits...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].start > sorted[j].start })
	for _, e := range sorted {
		src = src[:e.start] + e.text + src[e.end:]
	}
	return src
}

// wholeLines widens [start, end) to the full lines it touches, with their
// trailing newline, so removing it leaves no stray indentation.
func wholeLines(src string, start, end int) (int, int) {
	for start > 0 && src[start-1] != '\n' {
		start--
	}
	if i := strings.IndexByte(src[end:], '\n'); i >= 0 {
		end += i + 1
	} else {
		end = len(src)
	}
	return start, end
}

// gofmt returns src formatted, or false when it no longer parses.
func gofmt(src string) (string, bool) {
	out, err := format.Source([]byte(src))
	if err != nil {
		return "", false
	}
	return string(out), true
}

func isExported(name string) bool {
	for _, r := range name {
		return unicode.IsUpper(r)
	}
	return false
}

func isFuncCandidate(fd *ast.FuncDecl) bool {
	return fd.Recv == nil && fd.Body != nil && fd.Name.Name != "init" && fd.Name.Name != "main" && fd.Name.Name != "_"
}

// selectorNames returns the identifiers that appear as the selected name of a
// selector expression (x.Name) or as a composite-literal key, where an
// identifier names a field or method rather than the declaration in scope.
func selectorNames(f *ast.File) map[*ast.Ident]bool {
	out := map[*ast.Ident]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.SelectorExpr:
			out[n.Sel] = true
		case *ast.KeyValueExpr:
			if id, ok := n.Key.(*ast.Ident); ok {
				out[id] = true
			}
		}
		return true
	})
	return out
}

// declarationCounts counts the identifiers declared under each name in f:
// functions, methods, types, values, parameters, results, fields, and local
// definitions.
func declarationCounts(f *ast.File, counts map[string]int) {
	addFields := func(fl *ast.FieldList) {
		if fl == nil {
			return
		}
		for _, field := range fl.List {
			for _, n := range field.Names {
				counts[n.Name]++
			}
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncDecl:
			counts[n.Name.Name]++
			addFields(n.Recv)
		case *ast.FuncType:
			addFields(n.TypeParams)
			addFields(n.Params)
			addFields(n.Results)
		case *ast.StructType:
			addFields(n.Fields)
		case *ast.InterfaceType:
			addFields(n.Methods)
		case *ast.TypeSpec:
			counts[n.Name.Name]++
		case *ast.ValueSpec:
			for _, id := range n.Names {
				counts[id.Name]++
			}
		case *ast.AssignStmt:
			if n.Tok == token.DEFINE {
				for _, l := range n.Lhs {
					if id, ok := l.(*ast.Ident); ok {
						counts[id.Name]++
					}
				}
			}
		case *ast.RangeStmt:
			if n.Tok == token.DEFINE {
				for _, e := range []ast.Expr{n.Key, n.Value} {
					if id, ok := e.(*ast.Ident); ok {
						counts[id.Name]++
					}
				}
			}
		case *ast.ImportSpec:
			counts[importName(n)]++
		case *ast.LabeledStmt:
			counts[n.Label.Name]++
		}
		return true
	})
}

// importName is the name an import binds in its file. For an unnamed import it
// is the path's last element, which matches the package name by convention.
func importName(imp *ast.ImportSpec) string {
	if imp.Name != nil {
		return imp.Name.Name
	}
	p := strings.Trim(imp.Path.Value, `"`)
	return p[strings.LastIndexByte(p, '/')+1:]
}

// allIdentNames is every identifier spelling in files, selectors included.
func allIdentNames(files []parsedFile) map[string]bool {
	out := map[string]bool{}
	for _, pf := range files {
		ast.Inspect(pf.file, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok {
				out[id.Name] = true
			}
			return true
		})
	}
	return out
}

// splitWords splits a Go identifier into lower-case words: "parseHTTPHeader"
// becomes "parse http header".
func splitWords(name string) []string {
	var words []string
	var cur []rune
	runes := []rune(name)
	for i, r := range runes {
		if r == '_' {
			if len(cur) > 0 {
				words = append(words, strings.ToLower(string(cur)))
				cur = nil
			}
			continue
		}
		boundary := i > 0 && unicode.IsUpper(r) &&
			(unicode.IsLower(runes[i-1]) || (i+1 < len(runes) && unicode.IsLower(runes[i+1]) && unicode.IsUpper(runes[i-1])))
		if boundary && len(cur) > 0 {
			words = append(words, strings.ToLower(string(cur)))
			cur = nil
		}
		cur = append(cur, r)
	}
	if len(cur) > 0 {
		words = append(words, strings.ToLower(string(cur)))
	}
	return words
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}
