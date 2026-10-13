# jev

`jev` is a content `Scorer` that asks a System One decision model — TypeSafe Jev, reached through a LiteLLM proxy — for the probability that a batch builds and lands. It is meant to sit under `evidence` as the base price, replacing or averaging with `heuristic`.

## Request

Each score is one `POST` of a System One body to the configured path — LiteLLM's model-routed `/v1/systemone` by default, or the TypeSafe pass-through `/typesafe/v1/systemone` on proxies that expose only that (see the [LiteLLM decisions docs](https://docs.litellm.ai/docs/decisions)) — with a single noul question, `batch_lands`. The state is a JSON description of the batch built from `changeset.Resolver.DetailedForBatch`: the queue, change count, total files and lines changed, and per change its URI, author and changed files with line counts. Changes are sorted by URI so the same batch always produces the same request, and at most 200 files are described per batch; beyond that the state carries totals only and sets `files_truncated`. With a `changediff.Provider` configured, each change also carries its unified diff as `diff`, up to 64KB per batch; a change past the cap is cut at a line boundary and marked `diff_truncated`. Without one the model sees only file names and line counts, which carries little more signal than the `heuristic` scorer. A diff that cannot be read is a failure, so the batch falls back rather than being scored without its code.

With `jev.diff.files` and `jev.diff.references`, each change also carries `file_contents` (touched files in full) and `references` (`caller` and `definition` lines from elsewhere in the tree), from a separate 64KB budget spent only after every diff.

Sending diffs sends the code to the proxy and its model provider. It is opt-in for that reason.

The answer's `noul` is the price. A refusal, a missing answer, a value outside `[0, 1]`, a non-2xx status or a transport error is a failure.

## Caching and failure

A batch's requests are fixed at creation, so a price never goes stale. Successful prices are cached by queue and batch ID in a `Cache` that the wiring shares across every scorer built from one configuration; the speculator builds a fresh scorer per run, so a per-scorer cache would never be hit. Failures are not cached, so the next speculation run asks again.

On failure the scorer returns the configured fallback scorer's price and logs a warning. With no fallback it returns the error, and the `bestfirst` generator substitutes its own default. A missing endpoint or key is a failure on every call rather than a startup error, so a stack without credentials runs on the fallback.

Each call is bounded by the configured timeout (2s by default). The speculator scores every unresolved dependency on every run, so a slow model slows speculation; the cache makes that a one-time cost per batch.

## Configuration

In the orchestrator's `profiles.yaml`, under a scorer `base` or a composite component:

| Field | Default | Meaning |
|---|---|---|
| `jev.baseUrl` | — | Proxy root. When empty, read from `baseUrlEnv`. |
| `jev.baseUrlEnv` | `LITE_URL` | Environment variable holding the proxy root. |
| `jev.tokenEnv` | `LITE_KEY` | Environment variable holding the LiteLLM virtual key, sent as a bearer token. |
| `jev.path` | `/v1/systemone` | System One route on the proxy. Use `/typesafe/v1/systemone` for the TypeSafe pass-through. |
| `jev.model` | `jev` | The proxy's model name for the decision model. The pass-through may ignore it. |
| `jev.timeout` | `2s` | Bound on one call. |
| `jev.cacheSize` | `4096` | Batch prices kept in memory. |
| `jev.diff.git` | — | Repository to read diffs from, in the same shape as the git change provider's `git` block. Absent, no code is sent. Name the change provider's `repoPath` to share its copy. |
| `jev.diff.files` | `false` | Also send each touched file's full post-change text. |
| `jev.diff.references` | `false` | Also send untouched callers of changed symbols and definitions of called ones. |
| `fallback` | flat `heuristic` | Content scorer used when the model fails. Cannot itself be `jev`. |

The git demo enables it on `demo-queue`; see `service/submitqueue/demo/provider/git/profiles.yaml`.
