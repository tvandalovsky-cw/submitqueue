# changediff

Reads the code content of a change — its unified diff — as opposed to the metadata a `changeprovider` reports (author, files, line counts).

## Why it is separate from changeprovider

Change metadata is small, persisted in the change store, and read by every stage. A diff can be megabytes and is wanted only by consumers that judge the code itself, such as the `jev` scorer. Keeping diffs out of `entity.ChangeDetails` keeps them out of the change store and out of every consumer that never reads them. Nothing persists a diff; a consumer that wants one reads it on demand and caches whatever it derives.

## Contract

`Provider.Diff` takes a `change.Change` and returns one `entity.ChangeDiff` per URI, in URI order. For a stack, each diff covers only its own change, so the diffs concatenate to the stack's whole effect. Diffs are returned in full; the caller decides how much of them it can afford to use.

## Implementations

- `git` — reads from a local copy of a git remote through `platform/git/repo`. It uses the same baseline as the git change provider: the first change from where it diverged from the target, each later change from where it diverged from the change before it, so a diff and its numstat agree. It refreshes the target before diffing, because a change rebased onto a newer target would otherwise be diffed against a stale merge base and appear to contain the target's new commits. Sharing a repository copy with the git change provider shares its lock.

## Context beyond the diff

The git implementation can also return, per change, the full post-change text of every text file it adds or modifies (`IncludeFiles`), and references found by searching the tree at the change's head (`IncludeReferences`): untouched *callers* of symbols the diff modifies or removes, and *definitions* of functions the diff calls but does not define. Symbols are found by a line-oriented heuristic over the diff — Go declarations first, Python and JavaScript as a best effort — and searches follow Go scoping: a bare call resolves only in the caller's package directory, and a use from another package must be qualified. Searches are capped per symbol and per change. See [doc/rfc/submitqueue/code-aware-scoring.md](../../../doc/rfc/submitqueue/code-aware-scoring.md).

## Wiring

The orchestrator builds a `git` provider for a `jev` scorer whose `jev.diff.git` block names a repository. A block naming the same `repoPath` as the queue's git change provider shares that copy; one path naming two different remotes is rejected at startup.
