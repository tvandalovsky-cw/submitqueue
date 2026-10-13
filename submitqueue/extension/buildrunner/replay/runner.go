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

package replay

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/uber/submitqueue/platform/base/change"
	changegit "github.com/uber/submitqueue/platform/base/change/git"
	"github.com/uber/submitqueue/submitqueue/core/changeset"
	"github.com/uber/submitqueue/submitqueue/entity"
	"github.com/uber/submitqueue/submitqueue/extension/buildrunner"
)

// Params configures a replay runner.
type Params struct {
	// Config is the per-queue identity this runner serves.
	Config buildrunner.Config
	// Resolver resolves batches into their changes.
	Resolver changeset.Resolver
	// Index holds the recorded attempts.
	Index Index
	// TimeScale divides recorded durations, so a replay of hours of CI
	// finishes in minutes. Values below 1 are treated as 1.
	TimeScale float64
	// Now returns the current time. Nil means time.Now.
	Now func() time.Time
}

type runner struct {
	p Params
}

// New returns a replay build runner. A build fails if any change in its base
// or head is recorded as failing — a build containing a broken change is
// broken whatever it was meant to test — or if two of them are recorded as
// failing together, and takes the longest recorded duration among them.
// Changes with no record pass instantly.
func New(p Params) buildrunner.BuildRunner {
	if p.TimeScale < 1 {
		p.TimeScale = 1
	}
	if p.Now == nil {
		p.Now = time.Now
	}
	return &runner{p: p}
}

func (r *runner) Trigger(ctx context.Context, base []entity.Batch, head entity.Batch, _ entity.BuildMetadata) (entity.BuildID, error) {
	batches := append(append([]entity.Batch{}, base...), head)
	var found []Attempt
	var longest int64
	for _, b := range batches {
		changes, err := r.p.Resolver.ChangesForBatch(ctx, b)
		if err != nil {
			return entity.BuildID{}, fmt.Errorf("replay: resolve batch %s: %w", b.ID, err)
		}
		for _, a := range r.recorded(changes) {
			found = append(found, a)
			longest = max(longest, a.DurationMs)
		}
	}
	outcome := OutcomePass
	if anyFails(found) {
		outcome = OutcomeFail
	}
	due := r.p.Now().UnixMilli() + int64(float64(longest)/r.p.TimeScale)

	var suffix [4]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return entity.BuildID{}, fmt.Errorf("replay: generating build id: %w", err)
	}
	// The outcome and due time live in the ID, so Status needs no state and
	// any runner instance can answer for any build.
	return entity.BuildID{ID: fmt.Sprintf("replay-%s-%d-%s", outcome, due, hex.EncodeToString(suffix[:]))}, nil
}

func (r *runner) recorded(changes []change.Change) []Attempt {
	var found []Attempt
	for _, c := range changes {
		for _, uri := range c.URIs {
			id, err := changegit.ParseChangeID(uri)
			if err != nil {
				continue
			}
			if a, ok := r.p.Index[id.CommitSHA]; ok {
				found = append(found, a)
			}
		}
	}
	return found
}

func (r *runner) Status(_ context.Context, buildID entity.BuildID) (entity.BuildStatus, entity.BuildMetadata, error) {
	parts := strings.SplitN(buildID.ID, "-", 4)
	if len(parts) != 4 || parts[0] != "replay" {
		return entity.BuildStatusUnknown, nil, fmt.Errorf("replay: build %q was not minted by this runner", buildID.ID)
	}
	due, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return entity.BuildStatusUnknown, nil, fmt.Errorf("replay: build %q has no due time: %w", buildID.ID, err)
	}
	if r.p.Now().UnixMilli() < due {
		return entity.BuildStatusRunning, nil, nil
	}
	if Outcome(parts[1]) == OutcomeFail {
		return entity.BuildStatusFailed, nil, nil
	}
	return entity.BuildStatusSucceeded, nil, nil
}

// Cancel is a no-op: a replayed build has no CI behind it to stop.
func (r *runner) Cancel(context.Context, entity.BuildID) error {
	return nil
}
