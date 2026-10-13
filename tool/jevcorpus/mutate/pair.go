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
	"sort"
	"strconv"
	"strings"
)

// Pair is two changes made against the same base. Their file sets are
// disjoint, so a textual merge of the two is always clean: whatever goes wrong
// when both land goes wrong in the code, not in the merge.
type Pair struct {
	// A is the change that lands first.
	A Patch
	// B is the change that lands second.
	B Patch
}

// PairMutator makes a Pair in one package. Each side is designed to build and
// pass alone; Intent says whether the two together should too.
type PairMutator struct {
	// Name identifies the pair mutator in a corpus manifest; stable across
	// releases.
	Name string
	// Intent is what landing both is designed to do to the build.
	Intent Intent
	apply  func(t Tree, dir string, r *rand.Rand) (Pair, bool)
}

// Apply makes a pair in the package at dir. It returns false when the package
// has no site for this mutator. Every returned file is gofmt-clean, and the
// two sides never touch the same file.
func (m PairMutator) Apply(t Tree, dir string, r *rand.Rand) (Pair, bool) {
	p, ok := m.apply(t, dir, r)
	if !ok || len(p.A) == 0 || len(p.B) == 0 {
		return Pair{}, false
	}
	a, okA := formatPatch(t, p.A)
	b, okB := formatPatch(t, p.B)
	if !okA || !okB {
		return Pair{}, false
	}
	for file := range a {
		if _, shared := b[file]; shared {
			return Pair{}, false
		}
	}
	return Pair{A: a, B: b}, true
}

func formatPatch(t Tree, p Patch) (Patch, bool) {
	out := make(Patch, len(p))
	for file, src := range p {
		formatted, ok := gofmt(src)
		if !ok {
			return nil, false
		}
		if old, exists := t.File(file); exists && old == formatted {
			continue
		}
		out[file] = formatted
	}
	return out, len(out) > 0
}

// The conflicting pairs reproduce the commonest code failure found in real
// merge-queue history: one change alters an API and updates every use it can
// see, while another, written against the old API, adds a use it cannot.
var (
	// SignatureVsNewCaller: A threads a context.Context through a function
	// and every call; B adds a new call with the old arguments.
	SignatureVsNewCaller = PairMutator{Name: "signature-vs-new-caller", Intent: IntentUnsafe, apply: signatureVsCaller(true)}
	// RenameVsNewCaller: A renames a function and every use; B adds a new use
	// of the old name.
	RenameVsNewCaller = PairMutator{Name: "rename-vs-new-caller", Intent: IntentUnsafe, apply: renameVsCaller(true)}
	// DeleteVsNewCaller: A deletes a function nothing calls; B adds its first
	// caller.
	DeleteVsNewCaller = PairMutator{Name: "delete-vs-new-caller", Intent: IntentUnsafe, apply: deleteVsCaller(true)}
	// DuplicateHelper: A and B each add the same helper, under the same name,
	// in different files.
	DuplicateHelper = PairMutator{Name: "duplicate-helper", Intent: IntentUnsafe, apply: helperPair(true)}
)

// The compatible pairs have the same shapes, so the shape alone cannot predict
// the outcome: each pairs the same A with a B that touches a different
// function, or a different helper.
var (
	// SignatureVsOtherCaller: A as in SignatureVsNewCaller; B wraps a
	// different function.
	SignatureVsOtherCaller = PairMutator{Name: "signature-vs-other-caller", Intent: IntentSafe, apply: signatureVsCaller(false)}
	// RenameVsOtherCaller: A as in RenameVsNewCaller; B wraps a different
	// function.
	RenameVsOtherCaller = PairMutator{Name: "rename-vs-other-caller", Intent: IntentSafe, apply: renameVsCaller(false)}
	// DeleteVsOtherCaller: A as in DeleteVsNewCaller; B wraps a different
	// function.
	DeleteVsOtherCaller = PairMutator{Name: "delete-vs-other-caller", Intent: IntentSafe, apply: deleteVsCaller(false)}
	// DistinctHelpers: A and B each add a different helper.
	DistinctHelpers = PairMutator{Name: "distinct-helpers", Intent: IntentSafe, apply: helperPair(false)}
)

// Pairs is every pair mutator, conflicting first.
var Pairs = []PairMutator{
	SignatureVsNewCaller, RenameVsNewCaller, DeleteVsNewCaller, RemoveAPIVsNewCaller, DuplicateHelper,
	SignatureVsOtherCaller, RenameVsOtherCaller, DeleteVsOtherCaller, RemoveAPIVsOtherCaller, DistinctHelpers,
}

// PairsByIntent returns the pair mutators with intent i, in Pairs order.
func PairsByIntent(i Intent) []PairMutator {
	var out []PairMutator
	for _, m := range Pairs {
		if m.Intent == i {
			out = append(out, m)
		}
	}
	return out
}

// wrappable reports whether a wrapper can forward to fd: no type parameters,
// since the wrapper would have to restate their constraints.
func wrappable(fd *ast.FuncDecl) bool { return fd.Type.TypeParams == nil }

// pickWrapTarget returns the function B wraps: the one A changes when the pair
// is meant to conflict, otherwise a different wrappable function in the
// package.
func pickWrapTarget(sites []funcSite, changed funcSite, conflict bool, r *rand.Rand) (funcSite, bool) {
	if conflict {
		return changed, wrappable(changed.decl)
	}
	var others []funcSite
	for _, s := range sites {
		if s.decl.Name.Name != changed.decl.Name.Name && wrappable(s.decl) {
			others = append(others, s)
		}
	}
	if len(others) == 0 {
		return funcSite{}, false
	}
	return others[r.Intn(len(others))], true
}

func signatureVsCaller(conflict bool) func(Tree, string, *rand.Rand) (Pair, bool) {
	return func(t Tree, dir string, r *rand.Rand) (Pair, bool) {
		files := packageFiles(t, dir)
		sites := unexportedFuncs(files)
		var changeable []funcSite
		for _, s := range sites {
			if wrappable(s.decl) && !isVariadic(s.decl) && calledIn(files, s) && onlyCalled(files, s) {
				changeable = append(changeable, s)
			}
		}
		if len(changeable) == 0 {
			return Pair{}, false
		}
		changed := changeable[r.Intn(len(changeable))]
		a, ok := threadContext(files, changed)
		if !ok {
			return Pair{}, false
		}
		target, ok := pickWrapTarget(sites, changed, conflict, r)
		if !ok {
			return Pair{}, false
		}
		b, ok := wrapperPatch(t, files, target, r)
		return Pair{A: a, B: b}, ok
	}
}

func renameVsCaller(conflict bool) func(Tree, string, *rand.Rand) (Pair, bool) {
	return func(t Tree, dir string, r *rand.Rand) (Pair, bool) {
		files := packageFiles(t, dir)
		sites := unexportedFuncs(files)
		counts := map[string]int{}
		for _, pf := range files {
			declarationCounts(pf.file, counts)
		}
		var renamable []funcSite
		for _, s := range sites {
			if counts[s.decl.Name.Name] == 1 && !hasDirective(s.decl.Doc) && wrappable(s.decl) {
				renamable = append(renamable, s)
			}
		}
		if len(renamable) == 0 {
			return Pair{}, false
		}
		changed := renamable[r.Intn(len(renamable))]
		a, ok := renameFuncEverywhere(files, changed, r)
		if !ok {
			return Pair{}, false
		}
		target, ok := pickWrapTarget(sites, changed, conflict, r)
		if !ok {
			return Pair{}, false
		}
		b, ok := wrapperPatch(t, files, target, r)
		return Pair{A: a, B: b}, ok
	}
}

func deleteVsCaller(conflict bool) func(Tree, string, *rand.Rand) (Pair, bool) {
	return func(t Tree, dir string, r *rand.Rand) (Pair, bool) {
		files := packageFiles(t, dir)
		sites := unexportedFuncs(files)
		var dead []funcSite
		for _, s := range sites {
			if !hasDirective(s.decl.Doc) && funcReferences(files, s) == 0 && wrappable(s.decl) {
				dead = append(dead, s)
			}
		}
		if len(dead) == 0 {
			return Pair{}, false
		}
		changed := dead[r.Intn(len(dead))]
		target, ok := pickWrapTarget(sites, changed, conflict, r)
		if !ok {
			return Pair{}, false
		}
		b, ok := wrapperPatch(t, files, target, r)
		return Pair{A: deleteFuncDecl(changed), B: b}, ok
	}
}

func helperPair(conflict bool) func(Tree, string, *rand.Rand) (Pair, bool) {
	return func(t Tree, dir string, r *rand.Rand) (Pair, bool) {
		files := packageFiles(t, dir)
		if len(files) == 0 {
			return Pair{}, false
		}
		pkg := files[0].file.Name.Name
		names := allIdentNames(files)
		order := r.Perm(len(helperTemplates))
		var free []helperTemplate
		for _, i := range order {
			h := helperTemplates[i]
			if !names[h.name] && !names["Test"+upperFirst(h.name)] {
				free = append(free, h)
			}
		}
		if len(free) < 2 {
			return Pair{}, false
		}
		first, second := free[0], free[1]
		if conflict {
			second = first
		}
		a, okA := helperFiles(t, dir, pkg, first, "_helper")
		b, okB := helperFiles(t, dir, pkg, second, "_util")
		return Pair{A: a, B: b}, okA && okB
	}
}

// helperFiles writes h, and a test whose name carries the file suffix so two
// copies of one helper collide on the helper rather than on their tests.
func helperFiles(t Tree, dir, pkg string, h helperTemplate, suffix string) (Patch, bool) {
	srcPath := path.Join(dir, h.stem+suffix+".go")
	testPath := path.Join(dir, h.stem+suffix+"_test.go")
	_, srcExists := t.File(srcPath)
	_, testExists := t.File(testPath)
	if srcExists || testExists {
		return nil, false
	}
	test := "Test" + upperFirst(h.name) + upperFirst(strings.TrimPrefix(suffix, "_"))
	fill := strings.NewReplacer("{{pkg}}", pkg, "{{name}}", h.name, "{{test}}", test)
	return Patch{srcPath: fill.Replace(h.src), testPath: fill.Replace(h.test)}, true
}

// onlyCalled reports whether every use of site's function is a direct call, so
// updating the calls updates every use. A function passed as a value would keep
// its old type at that use and break A on its own.
func onlyCalled(files []parsedFile, site funcSite) bool {
	name := site.decl.Name.Name
	refs, calls := 0, 0
	for _, pf := range files {
		sel := selectorNames(pf.file)
		sameFile := pf.path == site.pf.path
		inDecl := func(n ast.Node) bool {
			return sameFile && n.Pos() >= site.decl.Pos() && n.End() <= site.decl.End()
		}
		ast.Inspect(pf.file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.Ident:
				if n.Name == name && !sel[n] && !inDecl(n) {
					refs++
				}
			case *ast.CallExpr:
				if id, ok := n.Fun.(*ast.Ident); ok && id.Name == name && !inDecl(n) {
					calls++
				}
			}
			return true
		})
	}
	return refs == calls
}

// appendToList inserts item before the closing parenthesis at closing,
// respecting a trailing comma left by a multi-line list.
func appendToList(pf parsedFile, closing token.Pos, nonEmpty bool, item string) textEdit {
	at := pf.offset(closing)
	text := item
	if nonEmpty {
		text = ", " + item
		if trailingComma(pf.src, at) {
			text = item + ","
		}
	}
	return textEdit{start: at, end: at, text: text}
}

// renameFuncEverywhere renames site's function, every use in the package, and
// its doc comment.
func renameFuncEverywhere(files []parsedFile, site funcSite, r *rand.Rand) (Patch, bool) {
	old := site.decl.Name.Name
	names := allIdentNames(files)
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
	word := regexp.MustCompile(`\b` + regexp.QuoteMeta(old) + `\b`)
	out := Patch{}
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
		if pf.path == site.pf.path && site.decl.Doc != nil {
			start, end := pf.offset(site.decl.Doc.Pos()), pf.offset(site.decl.Doc.End())
			edits = append(edits, textEdit{start: start, end: end, text: word.ReplaceAllString(pf.src[start:end], renamed)})
		}
		if len(edits) > 0 {
			out[pf.path] = splice(pf.src, edits)
		}
	}
	return out, true
}

var wrapperPrefixes = []string{"Run", "Try", "Call", "Do"}

// wrapperPatch adds a new file holding an exported function that forwards to
// target with target's own parameters — the smallest new use of a function,
// and one that compiles exactly as long as target keeps its name and
// signature. Exported, it reads as new API rather than as dead code.
func wrapperPatch(t Tree, files []parsedFile, target funcSite, r *rand.Rand) (Patch, bool) {
	names := allIdentNames(files)
	fn := target.decl.Name.Name
	wrapper := ""
	for _, i := range r.Perm(len(wrapperPrefixes)) {
		if cand := wrapperPrefixes[i] + upperFirst(fn); !names[cand] {
			wrapper = cand
			break
		}
	}
	if wrapper == "" {
		return nil, false
	}
	file := path.Join(path.Dir(target.pf.path), strings.Join(splitWords(fn), "_")+"_call.go")
	if _, exists := t.File(file); exists {
		return nil, false
	}

	pf, ft := target.pf, target.decl.Type
	var params, args []string
	n := 0
	for _, field := range ft.Params.List {
		typ := pf.src[pf.offset(field.Type.Pos()):pf.offset(field.Type.End())]
		variadic := strings.HasPrefix(typ, "...")
		fieldNames := field.Names
		count := len(fieldNames)
		if count == 0 {
			count = 1
		}
		for i := 0; i < count; i++ {
			arg := "arg" + strconv.Itoa(n)
			if i < len(fieldNames) && fieldNames[i].Name != "_" {
				arg = fieldNames[i].Name
			}
			n++
			params = append(params, arg+" "+typ)
			if variadic {
				args = append(args, arg+"...")
			} else {
				args = append(args, arg)
			}
		}
	}
	results := ""
	if ft.Results != nil {
		results = " " + pf.src[pf.offset(ft.Results.Pos()):pf.offset(ft.Results.End())]
	}
	call := fn + "(" + strings.Join(args, ", ") + ")"
	body := "\t" + call + "\n"
	if ft.Results != nil && ft.Results.NumFields() > 0 {
		body = "\treturn " + call + "\n"
	}
	signature := "(" + strings.Join(params, ", ") + ")" + results

	var imports []string
	for _, imp := range pf.file.Imports {
		if regexp.MustCompile(`\b` + regexp.QuoteMeta(importName(imp)) + `\.`).MatchString(signature) {
			spec := imp.Path.Value
			if imp.Name != nil {
				spec = imp.Name.Name + " " + spec
			}
			imports = append(imports, spec)
		}
	}
	sort.Strings(imports)
	var src strings.Builder
	src.WriteString("package " + pf.file.Name.Name + "\n\n")
	if len(imports) > 0 {
		src.WriteString("import (\n\t" + strings.Join(imports, "\n\t") + "\n)\n\n")
	}
	src.WriteString("// " + wrapper + " calls " + fn + ".\n")
	src.WriteString("func " + wrapper + signature + " {\n" + body + "}\n")
	return Patch{file: src.String()}, true
}

func identNamesIn(n ast.Node) map[string]bool {
	used := map[string]bool{}
	ast.Inspect(n, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok {
			used[id.Name] = true
		}
		return true
	})
	return used
}
