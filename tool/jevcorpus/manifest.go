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

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/uber/submitqueue/submitqueue/extension/buildrunner/replay"
	"github.com/uber/submitqueue/tool/jevcorpus/mutate"
)

// Manifest is the corpus's answer key: what each change was built to do and
// what it did. It sits beside the replay dataset rather than in it, so nothing
// a scorer reads through the queue — branch, commit, dataset title — carries
// the label or the mutation that produced it.
type Manifest struct {
	// Repo is the seed repository the corpus was generated from.
	Repo string `json:"repo"`
	// BaseSHA is the seed commit every change is built on.
	BaseSHA string `json:"base_sha"`
	// Seed is the generator's random seed.
	Seed int64 `json:"seed"`
	// Verify is the shell command whose exit status is the label.
	Verify string `json:"verify"`
	// Changes are in dataset order, pair members included.
	Changes []ManifestChange `json:"changes"`
	// Pairs are the changes generated two at a time.
	Pairs []ManifestPair `json:"pairs,omitempty"`
}

// ManifestPair is two changes made against the same base, each verified to
// land alone, and verified again together.
type ManifestPair struct {
	// ID names the pair within the corpus.
	ID string `json:"id"`
	// Mutator is the pair mutator that made it.
	Mutator string `json:"mutator"`
	// Intent is what landing both was designed to do.
	Intent mutate.Intent `json:"intent"`
	// A and B are the two sides' branches.
	A string `json:"a"`
	B string `json:"b"`
	// ASHA and BSHA are the two sides' commits.
	ASHA string `json:"a_sha"`
	BSHA string `json:"b_sha"`
	// Lands is the observed label for both together.
	Lands bool `json:"lands"`
	// CompilesTogether is whether both together pass the compile check: true
	// when they land, and for a conflict that only shows when tests run.
	CompilesTogether bool `json:"compiles_together"`
	// NoiseFiles is how many unrelated files the two sides also edit.
	NoiseFiles int `json:"noise_files,omitempty"`
	// Function is the package-qualified function a behavior pair's sides
	// change and test; empty for other pairs.
	Function string `json:"function,omitempty"`
	// BehaviorMutator is the edit a behavior pair's A makes to Function.
	BehaviorMutator string `json:"behavior_mutator,omitempty"`
	// VerifyMs is how long verifying both together took, in milliseconds.
	VerifyMs int64 `json:"verify_ms"`
	// VerifyTail is the end of that run's output when it failed.
	VerifyTail string `json:"verify_tail,omitempty"`
}

// ManifestChange is one generated change.
type ManifestChange struct {
	// Branch is the sandbox branch holding the change.
	Branch string `json:"branch"`
	// SHA is the change's commit on the base.
	SHA string `json:"sha"`
	// Intent is what the change was designed to do.
	Intent mutate.Intent `json:"intent"`
	// Lands is the observed label: Verify passed.
	Lands bool `json:"lands"`
	// Compiles is whether the change passes the compile check alone.
	Compiles bool `json:"compiles"`
	// TargetTier is the size the planner aimed for.
	TargetTier Tier `json:"target_tier"`
	// Tier is the size measured from git's diff.
	Tier Tier `json:"tier"`
	// Mutators are the mutators applied, in order.
	Mutators []string `json:"mutators"`
	// Files is the number of files changed.
	Files int `json:"files"`
	// LinesChanged is lines added plus lines deleted.
	LinesChanged int `json:"lines_changed"`
	// DiffBytes is the size of the change's unified diff.
	DiffBytes int `json:"diff_bytes"`
	// Needle is the unsafe edit in a large change; empty Mutator when none.
	Needle ManifestNeedle `json:"needle,omitzero"`
	// VerifyMs is how long Verify took, in milliseconds.
	VerifyMs int64 `json:"verify_ms"`
	// VerifyTail is the end of Verify's output when it failed.
	VerifyTail string `json:"verify_tail,omitempty"`
	// Pair is the ID of the pair this change is a side of; empty for a single
	// change.
	Pair string `json:"pair,omitempty"`
}

// ManifestNeedle locates the unsafe edit in a large change.
type ManifestNeedle struct {
	// Mutator is the unsafe mutator applied.
	Mutator string `json:"mutator,omitempty"`
	// File is the file it edited.
	File string `json:"file,omitempty"`
	// Position is where File sits among the change's files.
	Position NeedlePosition `json:"position,omitempty"`
	// OffsetBytes is where File's section starts in the change's diff.
	OffsetBytes int `json:"offset_bytes,omitempty"`
	// BeyondDiffBudget is true when OffsetBytes is past the jev scorer's
	// default diff budget, so a single-change batch never shows the model the
	// edit.
	BeyondDiffBudget bool `json:"beyond_diff_budget,omitempty"`
}

// Category names the kind of change for slicing results: the mutator of a
// single-edit change, or "sweep" with its needle for a large one.
func (c ManifestChange) Category() string {
	if c.Pair != "" {
		return "pair member"
	}
	if c.Needle.Mutator != "" {
		return "sweep+" + c.Needle.Mutator
	}
	if c.TargetTier != TierS {
		return "sweep"
	}
	if len(c.Mutators) == 0 {
		return "unknown"
	}
	return c.Mutators[0]
}

func manifestPathFor(datasetPath string) string {
	return strings.TrimSuffix(datasetPath, filepath.Ext(datasetPath)) + ".manifest.json"
}

func loadManifest(path string) (Manifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, fmt.Errorf("read manifest: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return Manifest{}, fmt.Errorf("parse manifest %s: %w", path, err)
	}
	return m, nil
}

func writeJSON(path string, v any) error {
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(out, '\n'), 0o644)
}

func writeDataset(path string, ds replay.Dataset) error { return writeJSON(path, ds) }
