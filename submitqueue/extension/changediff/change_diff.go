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

// Package changediff defines the contract for reading the code content of a
// change — its diff — as opposed to the metadata a changeprovider reports.
//
// The two are separate because their costs differ by orders of magnitude: change
// metadata is small, persisted, and read by every stage, while a diff can be
// megabytes and is wanted only by consumers that judge the code itself (for
// example a model-backed scorer). Keeping diffs out of entity.ChangeDetails keeps
// them out of the change store and out of every consumer that never reads them.
package changediff

//go:generate mockgen -source=change_diff.go -destination=mock/change_diff_mock.go -package=mock

import (
	"context"

	"github.com/uber/submitqueue/platform/base/change"
	"github.com/uber/submitqueue/submitqueue/entity"
)

// Provider reads the diffs of a change.
type Provider interface {
	// Diff returns one entity.ChangeDiff per URI of c, in URI order. For a stack
	// each diff covers only its own change, so the diffs concatenate to the
	// stack's whole effect. Diffs are returned in full; callers bound their size.
	Diff(ctx context.Context, c change.Change) ([]entity.ChangeDiff, error)
}

// Config carries the per-queue identity handed to a Factory.
type Config struct {
	// QueueName identifies the queue this Provider serves.
	QueueName string
}

// Factory builds the Provider for a queue.
type Factory interface {
	// For returns the Provider for the given queue.
	For(cfg Config) (Provider, error)
}
