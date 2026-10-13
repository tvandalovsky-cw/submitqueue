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

// Command replaybench builds replay benchmarks from an open-source
// repository's GitHub merge-queue history. See
// doc/rfc/submitqueue/replay-benchmark.md.
//
//	replaybench fetch       -repo owner/name -since 2026-01-01 -until 2026-02-01 -out ds.json
//	replaybench materialize -dataset ds.json -mirror /tmp/m.git -sandbox /tmp/s/sandbox.git
//	replaybench split       -dataset ds.json -size 12
//	replaybench uris        -dataset ds.json
package main

import (
	"fmt"
	"os"
)

const usage = `usage: replaybench <fetch|split|materialize|uris> [flags]`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "fetch":
		err = runFetch(os.Args[2:])
	case "split":
		err = runSplit(os.Args[2:])
	case "materialize":
		err = runMaterialize(os.Args[2:])
	case "uris":
		err = runURIs(os.Args[2:])
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
