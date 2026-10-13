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

// Package git provides a changediff.Provider that reads diffs out of a local
// copy of a git remote.
//
// Each change is diffed from where it diverged from the change before it in its
// stack, and the first from the target — the same baseline the git change
// provider measures line counts against, so a diff and its numstat agree.
//
// Optionally it also reads context at the change's head: the full post-change
// text of touched files, and references found by searching the tree for the
// symbols the diff touches (see context.go). See
// doc/rfc/submitqueue/code-aware-scoring.md for the tiers this implements.
package git

import (
	"context"
	"fmt"
	"sync"

	"github.com/uber-go/tally"

	"github.com/uber/submitqueue/platform/base/change"
	changegit "github.com/uber/submitqueue/platform/base/change/git"
	coremetrics "github.com/uber/submitqueue/platform/metrics"
	"github.com/uber/submitqueue/submitqueue/entity"
	"github.com/uber/submitqueue/submitqueue/extension/changediff"
)

const opName = "git_changediff"

// Repository is the git plumbing the differ needs. platform/git/repo.Repo
// satisfies it; sharing one with the git change provider shares its lock.
type Repository interface {
	sync.Locker
	FetchTarget(ctx context.Context) error
	EnsureCommit(ctx context.Context, sha, ref string) error
	MergeBase(ctx context.Context, a, b string) (string, error)
	RunRaw(ctx context.Context, args ...string) (string, error)
	Remote() string
	Target() string
}

// Params carries what a differ needs.
type Params struct {
	Config       changediff.Config
	Repo         Repository
	MetricsScope tally.Scope
	// IncludeFiles adds each touched file's post-change content.
	IncludeFiles bool
	// IncludeReferences adds untouched callers of symbols the change modifies
	// or removes, and definitions of symbols it calls but does not define.
	IncludeReferences bool
}

type differ struct {
	repo              Repository
	metricsScope      tally.Scope
	includeFiles      bool
	includeReferences bool
}

// New returns a changediff.Provider reading from repo.
func New(params Params) changediff.Provider {
	scope := params.MetricsScope
	if scope == nil {
		scope = tally.NoopScope
	}
	return &differ{
		repo:              params.Repo,
		metricsScope:      scope.SubScope(opName),
		includeFiles:      params.IncludeFiles,
		includeReferences: params.IncludeReferences,
	}
}

func (d *differ) Diff(ctx context.Context, c change.Change) (_ []entity.ChangeDiff, retErr error) {
	op := coremetrics.Begin(d.metricsScope, "diff", coremetrics.LongLatencyBuckets)
	defer func() { op.Complete(retErr) }()

	d.repo.Lock()
	defer d.repo.Unlock()

	// Without a fresh target, a change rebased onto a newer target would diff
	// against a stale merge base and include the target's own new commits.
	if err := d.repo.FetchTarget(ctx); err != nil {
		return nil, fmt.Errorf("failed to update target branch %s: %w", d.repo.Target(), err)
	}

	diffs := make([]entity.ChangeDiff, 0, len(c.URIs))
	previous := ""
	for _, uri := range c.URIs {
		id, err := changegit.ParseChangeID(uri)
		if err != nil {
			return nil, fmt.Errorf("failed to parse change URI: %w", err)
		}
		if err := d.repo.EnsureCommit(ctx, id.CommitSHA, id.Ref); err != nil {
			return nil, err
		}
		against := d.repo.Remote() + "/" + d.repo.Target()
		if previous != "" {
			against = previous
		}
		base, err := d.repo.MergeBase(ctx, against, id.CommitSHA)
		if err != nil {
			return nil, err
		}
		patch, err := d.repo.RunRaw(ctx, "diff", "-M", "--no-color", "--no-ext-diff", base, id.CommitSHA)
		if err != nil {
			return nil, fmt.Errorf("failed to diff change %s: %w", uri, err)
		}
		files, refs, err := d.changeContext(ctx, base, id.CommitSHA, patch)
		if err != nil {
			return nil, fmt.Errorf("failed to read context for change %s: %w", uri, err)
		}
		diffs = append(diffs, entity.ChangeDiff{URI: uri, Patch: patch, Files: files, References: refs})
		previous = id.CommitSHA
	}
	return diffs, nil
}
