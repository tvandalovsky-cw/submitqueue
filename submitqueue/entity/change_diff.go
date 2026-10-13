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

package entity

// ChangeDiff is the code content of one change: the unified diff from where the
// change diverged from the change before it in its stack (or from the target,
// for the first) to the change's head, plus optional surrounding context read
// at the change's head.
type ChangeDiff struct {
	// URI is the change URI this diff describes.
	URI string `json:"uri"`
	// Patch is the unified diff text. Binary files appear as git's one-line
	// "Binary files ... differ" marker rather than content.
	Patch string `json:"patch"`
	// Files is the post-change content of each text file the change modifies,
	// in path order. Added files are omitted: they are already whole in Patch.
	// Empty unless the provider was asked for files.
	Files []FileContent `json:"files,omitempty"`
	// References are lines elsewhere in the repository, at the change's head,
	// related to symbols the change touches. Empty unless the provider was
	// asked for references.
	References []CodeReference `json:"references,omitempty"`
}

// FileContent is the full text of one file at a commit.
type FileContent struct {
	// Path is the file path relative to the repository root.
	Path string `json:"path"`
	// Content is the file's text.
	Content string `json:"content"`
}

// CodeReferenceKind says how a reference relates to the change.
type CodeReferenceKind string

const (
	// CodeReferenceKindUnknown is the zero-value sentinel.
	CodeReferenceKindUnknown CodeReferenceKind = ""
	// CodeReferenceKindCaller is a use of a symbol whose definition the change
	// modifies or removes, in a file the change does not touch.
	CodeReferenceKindCaller CodeReferenceKind = "caller"
	// CodeReferenceKindDefinition is the definition of a symbol the change calls
	// but does not itself define.
	CodeReferenceKindDefinition CodeReferenceKind = "definition"
)

// CodeReference is one line of code related to a symbol a change touches.
type CodeReference struct {
	// Kind says how the line relates to the change.
	Kind CodeReferenceKind `json:"kind"`
	// Symbol is the identifier the line was found for.
	Symbol string `json:"symbol"`
	// Path is the file path relative to the repository root.
	Path string `json:"path"`
	// Line is the 1-based line number within Path.
	Line int `json:"line"`
	// Text is the line's content, without its trailing newline.
	Text string `json:"text"`
	// DeclarationBefore is, for a caller, the symbol's declaration line as the
	// change removed it. Empty for a definition.
	DeclarationBefore string `json:"declaration_before,omitempty"`
	// DeclarationAfter is, for a caller, the symbol's declaration line as the
	// change rewrote it, or empty when the change removes the symbol.
	DeclarationAfter string `json:"declaration_after,omitempty"`
}
