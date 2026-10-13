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

	"github.com/uber/submitqueue/platform/base/change"
	"github.com/uber/submitqueue/submitqueue/extension/speculation/scorer/oracle"
)

type truth struct {
	r *runner
}

// Truth returns an oracle.Truth backed by the recorded outcomes in idx, so the
// oracle agrees with the replay runner on every head. A batch is known only
// if at least one of its changes is recorded.
func Truth(idx Index) oracle.Truth {
	return truth{r: &runner{p: Params{Index: idx}}}
}

func (t truth) Outcome(_ context.Context, changes []change.Change) (bool, bool, error) {
	found := t.r.recorded(changes)
	for _, a := range found {
		if a.Outcome == OutcomeFail {
			return true, true, nil
		}
	}
	return false, len(found) > 0, nil
}
