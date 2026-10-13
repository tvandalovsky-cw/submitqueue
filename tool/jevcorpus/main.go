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

// Command jevcorpus builds a synthetic, build-verified corpus of safe and unsafe
// Go changes and grades outcome scorers against it. See
// doc/rfc/submitqueue/synthetic-corpus.md.
//
//	jevcorpus generate -repo /path/to/seed -count 80 -out corpus.json -sandbox /tmp/c/sandbox.git
//	jevcorpus eval     -dataset corpus.json -sandbox /tmp/c/sandbox.git -scorers size,jev
//
// The dataset is in the replay format, so `replaybench uris` lists its change
// URIs and the replay build runner and oracle scorer read it unchanged.
package main

import (
	"fmt"
	"os"
)

const usage = `usage: jevcorpus <generate|eval> [flags]`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "generate":
		err = runGenerate(os.Args[2:])
	case "eval":
		err = runEval(os.Args[2:])
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
