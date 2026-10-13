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
	"strconv"
)

// FlipComparison inverts one comparison in an if or for condition. Nil checks
// are left to DropErrorCheck so this stays a logic bug rather than a crash.
var FlipComparison = Mutator{Name: "flip-comparison", Intent: IntentUnsafe, Scope: ScopeFile, apply: flipComparison}

// OffByOne relaxes or tightens a bound against len(), or moves an integer
// literal in a comparison by one.
var OffByOne = Mutator{Name: "off-by-one", Intent: IntentUnsafe, Scope: ScopeFile, apply: offByOne}

// DropErrorCheck removes an `if err != nil { ... return ... }` block.
var DropErrorCheck = Mutator{Name: "drop-error-check", Intent: IntentUnsafe, Scope: ScopeFile, apply: dropErrorCheck}

// DeleteCalledFunc deletes an unexported function that the package still
// references.
var DeleteCalledFunc = Mutator{Name: "delete-called-func", Intent: IntentUnsafe, Scope: ScopePackage, apply: deleteCalledFunc}

// AddParam adds a parameter to a function without updating its callers.
var AddParam = Mutator{Name: "add-param", Intent: IntentUnsafe, Scope: ScopePackage, apply: addParam}

var flippedComparison = map[token.Token]token.Token{
	token.EQL: token.NEQ, token.NEQ: token.EQL,
	token.LSS: token.GEQ, token.GEQ: token.LSS,
	token.GTR: token.LEQ, token.LEQ: token.GTR,
}

var relaxedBound = map[token.Token]token.Token{
	token.LSS: token.LEQ, token.LEQ: token.LSS,
	token.GTR: token.GEQ, token.GEQ: token.GTR,
}

func isNil(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == "nil"
}

func isLenCall(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	id, ok := call.Fun.(*ast.Ident)
	return ok && id.Name == "len"
}

// conditionComparisons returns the comparisons inside if and for conditions.
func conditionComparisons(f *ast.File) []*ast.BinaryExpr {
	var out []*ast.BinaryExpr
	visit := func(cond ast.Expr) {
		if cond == nil {
			return
		}
		ast.Inspect(cond, func(n ast.Node) bool {
			if _, ok := n.(*ast.FuncLit); ok {
				return false
			}
			if be, ok := n.(*ast.BinaryExpr); ok {
				if _, cmp := flippedComparison[be.Op]; cmp {
					out = append(out, be)
				}
			}
			return true
		})
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.IfStmt:
			visit(n.Cond)
		case *ast.ForStmt:
			visit(n.Cond)
		}
		return true
	})
	return out
}

func replaceOperator(pf parsedFile, be *ast.BinaryExpr, op token.Token) Patch {
	start := pf.offset(be.OpPos)
	return Patch{pf.path: splice(pf.src, []textEdit{{start: start, end: start + len(be.Op.String()), text: op.String()}})}
}

func flipComparison(t Tree, target string, r *rand.Rand) (Patch, bool) {
	pf, ok := parseTarget(t, target)
	if !ok {
		return nil, false
	}
	var sites []*ast.BinaryExpr
	for _, be := range conditionComparisons(pf.file) {
		if !isNil(be.X) && !isNil(be.Y) {
			sites = append(sites, be)
		}
	}
	if len(sites) == 0 {
		return nil, false
	}
	be := sites[r.Intn(len(sites))]
	return replaceOperator(pf, be, flippedComparison[be.Op]), true
}

func offByOne(t Tree, target string, r *rand.Rand) (Patch, bool) {
	pf, ok := parseTarget(t, target)
	if !ok {
		return nil, false
	}
	type site struct {
		be  *ast.BinaryExpr
		lit *ast.BasicLit
	}
	var sites []site
	for _, be := range conditionComparisons(pf.file) {
		if _, bound := relaxedBound[be.Op]; !bound {
			continue
		}
		if isLenCall(be.X) || isLenCall(be.Y) {
			sites = append(sites, site{be: be})
			continue
		}
		for _, side := range []ast.Expr{be.X, be.Y} {
			if lit, ok := side.(*ast.BasicLit); ok && lit.Kind == token.INT {
				if _, err := strconv.Atoi(lit.Value); err == nil {
					sites = append(sites, site{be: be, lit: lit})
				}
			}
		}
	}
	if len(sites) == 0 {
		return nil, false
	}
	s := sites[r.Intn(len(sites))]
	if s.lit == nil {
		return replaceOperator(pf, s.be, relaxedBound[s.be.Op]), true
	}
	v, _ := strconv.Atoi(s.lit.Value)
	start := pf.offset(s.lit.Pos())
	return Patch{pf.path: splice(pf.src, []textEdit{{start: start, end: start + len(s.lit.Value), text: strconv.Itoa(v + 1)}})}, true
}

func dropErrorCheck(t Tree, target string, r *rand.Rand) (Patch, bool) {
	pf, ok := parseTarget(t, target)
	if !ok {
		return nil, false
	}
	var sites []*ast.IfStmt
	ast.Inspect(pf.file, func(n ast.Node) bool {
		is, ok := n.(*ast.IfStmt)
		if !ok || is.Init != nil || is.Else != nil {
			return true
		}
		be, ok := is.Cond.(*ast.BinaryExpr)
		if !ok || be.Op != token.NEQ || !isNil(be.Y) {
			return true
		}
		if id, ok := be.X.(*ast.Ident); !ok || id.Name != "err" {
			return true
		}
		for _, stmt := range is.Body.List {
			if _, ok := stmt.(*ast.ReturnStmt); ok {
				sites = append(sites, is)
				break
			}
		}
		return true
	})
	if len(sites) == 0 {
		return nil, false
	}
	is := sites[r.Intn(len(sites))]
	start, end := wholeLines(pf.src, pf.offset(is.Pos()), pf.offset(is.End()))
	return Patch{pf.path: splice(pf.src, []textEdit{{start: start, end: end}})}, true
}

type funcSite struct {
	pf   parsedFile
	decl *ast.FuncDecl
}

// funcReferences counts identifiers spelled name across files, outside the
// declaration itself and outside selectors and literal keys.
func funcReferences(files []parsedFile, site funcSite) int {
	name := site.decl.Name.Name
	refs := 0
	for _, pf := range files {
		sel := selectorNames(pf.file)
		ast.Inspect(pf.file, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if !ok || id.Name != name || sel[id] {
				return true
			}
			if pf.path == site.pf.path && id.Pos() >= site.decl.Pos() && id.End() <= site.decl.End() {
				return true
			}
			refs++
			return true
		})
	}
	return refs
}

// unexportedFuncs returns the package's unexported top-level functions in
// non-test files.
func unexportedFuncs(files []parsedFile) []funcSite {
	var out []funcSite
	for _, pf := range files {
		if IsTestFile(pf.path) {
			continue
		}
		for _, d := range pf.file.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if ok && isFuncCandidate(fd) && !isExported(fd.Name.Name) {
				out = append(out, funcSite{pf: pf, decl: fd})
			}
		}
	}
	return out
}

func deleteFuncDecl(site funcSite) Patch {
	startPos := site.decl.Pos()
	if site.decl.Doc != nil {
		startPos = site.decl.Doc.Pos()
	}
	start, end := wholeLines(site.pf.src, site.pf.offset(startPos), site.pf.offset(site.decl.End()))
	return Patch{site.pf.path: splice(site.pf.src, []textEdit{{start: start, end: end}})}
}

func deleteCalledFunc(t Tree, dir string, r *rand.Rand) (Patch, bool) {
	files := packageFiles(t, dir)
	var sites []funcSite
	for _, s := range unexportedFuncs(files) {
		if funcReferences(files, s) > 0 {
			sites = append(sites, s)
		}
	}
	if len(sites) == 0 {
		return nil, false
	}
	return deleteFuncDecl(sites[r.Intn(len(sites))]), true
}

var extraParams = []struct{ name, typ string }{
	{"limit", "int"}, {"verbose", "bool"}, {"retries", "int"}, {"flags", "uint32"},
}

func addParam(t Tree, dir string, r *rand.Rand) (Patch, bool) {
	files := packageFiles(t, dir)
	var sites []funcSite
	for _, pf := range files {
		if IsTestFile(pf.path) {
			continue
		}
		for _, d := range pf.file.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || !isFuncCandidate(fd) || isVariadic(fd) || fd.Type.TypeParams != nil {
				continue
			}
			if s := (funcSite{pf: pf, decl: fd}); calledIn(files, s) {
				sites = append(sites, s)
			}
		}
	}
	if len(sites) == 0 {
		return nil, false
	}
	s := sites[r.Intn(len(sites))]
	used := map[string]bool{}
	ast.Inspect(s.decl, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok {
			used[id.Name] = true
		}
		return true
	})
	for _, off := range r.Perm(len(extraParams)) {
		p := extraParams[off]
		if used[p.name] {
			continue
		}
		params := s.decl.Type.Params
		text := p.name + " " + p.typ
		if params.NumFields() > 0 {
			text = ", " + text
			if trailingComma(s.pf.src, s.pf.offset(params.Closing)) {
				text = p.name + " " + p.typ + ","
			}
		}
		at := s.pf.offset(params.Closing)
		return Patch{s.pf.path: splice(s.pf.src, []textEdit{{start: at, end: at, text: text}})}, true
	}
	return nil, false
}

func isVariadic(fd *ast.FuncDecl) bool {
	list := fd.Type.Params.List
	if len(list) == 0 {
		return false
	}
	_, ok := list[len(list)-1].Type.(*ast.Ellipsis)
	return ok
}

// calledIn reports whether site's function is called by name somewhere
// outside its own body.
func calledIn(files []parsedFile, site funcSite) bool {
	fd := site.decl
	for _, pf := range files {
		sameFile := pf.path == site.pf.path
		found := false
		ast.Inspect(pf.file, func(n ast.Node) bool {
			if found {
				return false
			}
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if id, ok := call.Fun.(*ast.Ident); ok && id.Name == fd.Name.Name && !(sameFile && call.Pos() >= fd.Pos() && call.End() <= fd.End()) {
				found = true
			}
			return true
		})
		if found {
			return true
		}
	}
	return false
}

func trailingComma(src string, closing int) bool {
	for i := closing - 1; i >= 0; i-- {
		switch src[i] {
		case ' ', '\t', '\n', '\r':
			continue
		case ',':
			return true
		default:
			return false
		}
	}
	return false
}

// parseTarget parses a non-test file target.
func parseTarget(t Tree, target string) (parsedFile, bool) {
	if IsTestFile(target) {
		return parsedFile{}, false
	}
	src, ok := t.File(target)
	if !ok {
		return parsedFile{}, false
	}
	return parseFile(target, src)
}
