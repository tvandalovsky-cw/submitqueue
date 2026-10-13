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
	"regexp"
	"strconv"
	"strings"
)

// threadContext adds a leading ctx context.Context parameter to site's
// function and passes a context at every call: the caller's own when it has
// one, otherwise context.TODO(). It is the commonest real signature change,
// and the one a reviewer reads as routine.
func threadContext(files []parsedFile, site funcSite) (Patch, bool) {
	if hasContextParam(site.pf, site.decl.Type) || identNamesIn(site.decl)["ctx"] {
		return nil, false
	}
	out := Patch{}
	for _, pf := range files {
		ctxName, imported, ok := contextImportName(pf)
		if !ok {
			return nil, false
		}
		var edits []textEdit
		needsImport := false
		if pf.path == site.pf.path {
			params := site.decl.Type.Params
			text := "ctx " + ctxName + ".Context"
			if params.NumFields() > 0 {
				text += ", "
			}
			at := pf.offset(params.Opening) + 1
			edits = append(edits, textEdit{start: at, end: at, text: text})
			needsImport = true
		}
		scopes := contextScopes(pf, ctxName, imported)
		ast.Inspect(pf.file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			id, ok := call.Fun.(*ast.Ident)
			if !ok || id.Name != site.decl.Name.Name {
				return true
			}
			arg := ctxName + ".TODO()"
			if pf.path == site.pf.path && call.Pos() >= site.decl.Pos() && call.End() <= site.decl.End() {
				arg = "ctx"
			} else if name, ok := innermostContext(scopes, call.Pos()); ok {
				arg = name
			} else {
				needsImport = true
			}
			if len(call.Args) > 0 {
				arg += ", "
			}
			at := pf.offset(call.Lparen) + 1
			edits = append(edits, textEdit{start: at, end: at, text: arg})
			return true
		})
		if len(edits) == 0 {
			continue
		}
		if needsImport && !imported {
			edits = append(edits, addImportEdit(pf, `"context"`))
		}
		out[pf.path] = splice(pf.src, edits)
	}
	return out, true
}

// contextImportName returns the name pf binds the context package to, whether
// it imports it, and false if something else already holds the name context.
func contextImportName(pf parsedFile) (string, bool, bool) {
	for _, imp := range pf.file.Imports {
		if strings.Trim(imp.Path.Value, `"`) == "context" {
			if imp.Name != nil {
				return imp.Name.Name, true, imp.Name.Name != "_" && imp.Name.Name != "."
			}
			return "context", true, true
		}
	}
	for _, imp := range pf.file.Imports {
		if importName(imp) == "context" {
			return "", false, false
		}
	}
	return "context", false, true
}

func hasContextParam(pf parsedFile, ft *ast.FuncType) bool {
	for _, field := range ft.Params.List {
		if strings.HasSuffix(pf.src[pf.offset(field.Type.Pos()):pf.offset(field.Type.End())], ".Context") {
			return true
		}
	}
	return false
}

type contextScope struct {
	start, end token.Pos
	name       string
}

// contextScopes lists the functions and closures in pf that take a context,
// with the name of that parameter.
func contextScopes(pf parsedFile, ctxName string, imported bool) []contextScope {
	if !imported {
		return nil
	}
	var out []contextScope
	add := func(ft *ast.FuncType, start, end token.Pos) {
		for _, field := range ft.Params.List {
			if pf.src[pf.offset(field.Type.Pos()):pf.offset(field.Type.End())] != ctxName+".Context" {
				continue
			}
			for _, n := range field.Names {
				if n.Name != "_" {
					out = append(out, contextScope{start: start, end: end, name: n.Name})
					return
				}
			}
		}
	}
	ast.Inspect(pf.file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncDecl:
			add(n.Type, n.Pos(), n.End())
		case *ast.FuncLit:
			add(n.Type, n.Pos(), n.End())
		}
		return true
	})
	return out
}

func innermostContext(scopes []contextScope, at token.Pos) (string, bool) {
	best, found := contextScope{}, false
	for _, s := range scopes {
		if at >= s.start && at <= s.end && (!found || s.start >= best.start) {
			best, found = s, true
		}
	}
	return best.name, found
}

// addImportEdit adds an import of path to pf, into its first import block when
// it has one.
func addImportEdit(pf parsedFile, path string) textEdit {
	for _, d := range pf.file.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.IMPORT {
			continue
		}
		if gd.Lparen.IsValid() {
			at := pf.offset(gd.Lparen) + 1
			return textEdit{start: at, end: at, text: "\n\t" + path}
		}
		at := pf.offset(gd.Pos())
		return textEdit{start: at, end: at, text: "import " + path + "\n"}
	}
	at := pf.offset(pf.file.Name.End())
	return textEdit{start: at, end: at, text: "\n\nimport " + path}
}

var (
	// RemoveAPIVsNewCaller: A deletes an exported function nothing in the
	// repository calls — retiring a deprecated API — while B adds its first
	// caller.
	RemoveAPIVsNewCaller = PairMutator{Name: "remove-api-vs-new-caller", Intent: IntentUnsafe, apply: removeAPIVsCaller(true)}
	// RemoveAPIVsOtherCaller: A as in RemoveAPIVsNewCaller; B wraps a
	// different function.
	RemoveAPIVsOtherCaller = PairMutator{Name: "remove-api-vs-other-caller", Intent: IntentSafe, apply: removeAPIVsCaller(false)}
)

func removeAPIVsCaller(conflict bool) func(Tree, string, *rand.Rand) (Pair, bool) {
	return func(t Tree, dir string, r *rand.Rand) (Pair, bool) {
		files := packageFiles(t, dir)
		all := topLevelFuncs(files)
		var unused []funcSite
		for _, s := range all {
			if isExported(s.decl.Name.Name) && wrappable(s.decl) && !hasDirective(s.decl.Doc) && unreferencedInTree(t, s) {
				unused = append(unused, s)
			}
		}
		if len(unused) == 0 {
			return Pair{}, false
		}
		changed := unused[r.Intn(len(unused))]
		target, ok := pickWrapTarget(all, changed, conflict, r)
		if !ok {
			return Pair{}, false
		}
		b, ok := wrapperPatch(t, files, target, r)
		return Pair{A: deleteFuncDecl(changed), B: b}, ok
	}
}

// topLevelFuncs returns the package's top-level functions in non-test files.
func topLevelFuncs(files []parsedFile) []funcSite {
	var out []funcSite
	for _, pf := range files {
		if IsTestFile(pf.path) {
			continue
		}
		for _, d := range pf.file.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && isFuncCandidate(fd) {
				out = append(out, funcSite{pf: pf, decl: fd})
			}
		}
	}
	return out
}

var identifierPattern = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)

// unreferencedInTree reports whether site's name appears nowhere in the tree
// but in its own declaration and doc comment. Any other mention — a call, a
// selector in another package, a comment, a string — counts, so the answer
// errs towards "referenced".
func unreferencedInTree(t Tree, site funcSite) bool {
	name := site.decl.Name.Name
	start := site.decl.Pos()
	if site.decl.Doc != nil {
		start = site.decl.Doc.Pos()
	}
	own := 0
	for _, w := range identifierPattern.FindAllString(site.pf.src[site.pf.offset(start):site.pf.offset(site.decl.End())], -1) {
		if w == name {
			own++
		}
	}
	return t.wordCount(name) == own
}

// Behavior mutants and probe targets serve pairs whose conflict is in behavior
// rather than in the build. They are built where the module can be run, since
// a probe's expected outputs come from running the code.

// ProbeTarget is a function simple enough to call with literal arguments and
// compare by value: every parameter and result is a basic type or a slice of
// one, and a trailing error is allowed.
type ProbeTarget struct {
	// File is the file declaring the function.
	File string
	// Package is the package name the file declares.
	Package string
	// Func is the function name.
	Func string
	// Params are the parameter types, in order, one per parameter.
	Params []string
	// Names are the parameter names, in Params order; "arg<i>" for an unnamed
	// or blank one.
	Names []string
	// Results are the non-error result types, in order.
	Results []string
	// ReturnsError is true when the last result is an error.
	ReturnsError bool
}

var probeTypes = map[string]bool{
	"int": true, "int64": true, "uint": true, "string": true, "bool": true, "float64": true,
	"[]int": true, "[]string": true, "[]byte": true,
}

// ProbeTargets returns the probe targets declared in non-test files of dir
// that have a site for a behavior mutant.
func ProbeTargets(t Tree, dir string) []ProbeTarget {
	var out []ProbeTarget
	for _, s := range topLevelFuncs(packageFiles(t, dir)) {
		ft := s.decl.Type
		if ft.TypeParams != nil || ft.Results == nil || ft.Params.NumFields() == 0 {
			continue
		}
		pt := ProbeTarget{File: s.pf.path, Package: s.pf.file.Name.Name, Func: s.decl.Name.Name}
		ok := true
		typeOf := func(e ast.Expr) string { return s.pf.src[s.pf.offset(e.Pos()):s.pf.offset(e.End())] }
		for _, field := range ft.Params.List {
			typ := typeOf(field.Type)
			if !probeTypes[typ] {
				ok = false
				break
			}
			for i := 0; i < max(len(field.Names), 1); i++ {
				name := "arg" + strconv.Itoa(len(pt.Params))
				if i < len(field.Names) && field.Names[i].Name != "_" {
					name = field.Names[i].Name
				}
				pt.Params = append(pt.Params, typ)
				pt.Names = append(pt.Names, name)
			}
		}
		var results []string
		for _, field := range ft.Results.List {
			for i := 0; i < max(len(field.Names), 1); i++ {
				results = append(results, typeOf(field.Type))
			}
		}
		if n := len(results); n > 0 && results[n-1] == "error" {
			pt.ReturnsError = true
			results = results[:n-1]
		}
		for _, typ := range results {
			ok = ok && probeTypes[typ]
		}
		pt.Results = results
		if ok && len(pt.Results)+boolToInt(pt.ReturnsError) > 0 && len(behaviorSites(s.pf, s.decl)) > 0 {
			out = append(out, pt)
		}
	}
	return out
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// behaviorSite is one place a behavior mutant can edit.
type behaviorSite struct {
	mutator string
	edit    textEdit
}

// behaviorSites lists the flip-comparison and off-by-one edits inside fd.
func behaviorSites(pf parsedFile, fd *ast.FuncDecl) []behaviorSite {
	var out []behaviorSite
	for _, be := range conditionComparisons(pf.file) {
		if be.Pos() < fd.Pos() || be.End() > fd.End() {
			continue
		}
		at := pf.offset(be.OpPos)
		if !isNil(be.X) && !isNil(be.Y) {
			out = append(out, behaviorSite{mutator: FlipComparison.Name, edit: textEdit{start: at, end: at + len(be.Op.String()), text: flippedComparison[be.Op].String()}})
		}
		if relaxed, ok := relaxedBound[be.Op]; ok {
			out = append(out, behaviorSite{mutator: OffByOne.Name, edit: textEdit{start: at, end: at + len(be.Op.String()), text: relaxed.String()}})
		}
	}
	return out
}

// BehaviorMutants returns every single-site behavior mutant of fn in file, in
// a random order: each flips or relaxes one comparison in its conditions.
func BehaviorMutants(t Tree, file, fn string, r *rand.Rand) []NamedPatch {
	src, ok := t.File(file)
	if !ok {
		return nil
	}
	pf, ok := parseFile(file, src)
	if !ok {
		return nil
	}
	var out []NamedPatch
	for _, d := range pf.file.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Recv != nil || fd.Name.Name != fn {
			continue
		}
		for _, s := range behaviorSites(pf, fd) {
			if formatted, ok := gofmt(splice(pf.src, []textEdit{s.edit})); ok {
				out = append(out, NamedPatch{Mutator: s.mutator, Patch: Patch{file: formatted}})
			}
		}
	}
	r.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

// NamedPatch is a patch and the mutator that made it.
type NamedPatch struct {
	// Mutator names the mutator.
	Mutator string
	// Patch is the edit.
	Patch Patch
}
