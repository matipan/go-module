# Successor Notes: Dang Module Development

This is not generic advice. It is what this repo has already taught us the hard
way. Follow it unless the code proves it wrong.

## Non-Negotiables

- Read `go.dang` before proposing API. Do not invent shapes from memory.
- When changing the module, update tests and run:

  ```sh
  dagger check -m .dagger/modules/e2e
  ```

- Put facts about one Go module on `GoModule`.
- Keep top-level `Go` for global configuration, module discovery, module lookup,
  and whole-workspace operations.
- Do not add top-level bypass lookups like `goFiles(modulePath:)`. If a fact is
  truly public module data, attach it to `GoModule`; otherwise keep it local.
- Do not expose implementation trivia. A public field should help a downstream
  user debug real behavior.
- A private helper must earn its keep. If it is small, single-use, and does not
  name a real concept, inline it.

## Public API Shape

Good public surface:

- `Go.modules(ws)` lists discovered modules.
- `Go.module(ws, path)` returns one discovered module. This is useful because the
  Dagger CLI does not provide a good built-in way to filter arrays of objects.
- `GoModule.path` is the module root path relative to the workspace.
- `GoModule.includeFromGoDirectives` is the include list discovered from that
  module's Go source directives, including `go:embed` and `workspace:include`.
- `GoModule.includeFromGoModReplace` is the include list discovered from local
  `go.mod` replace directives.
- `GoModule.source` is the final filtered workspace directory mounted for Go
  commands.

Bad public surface:

- `modulePaths`: callers can already query `modules().path`.
- `workdir`: it was useless indirection. Commands mount the workspace-shaped
  source at `/ws` and then use the module `path` as the workdir.
- `sourceIncludes`: if it only means "the patterns passed to `source`", either
  expose the final `include` list clearly or let users inspect `source`.
- `goFilesWithSourceIncludes`: this was only a tree-pruning optimization. It was
  removed. Simpler code beats this optimization here.
- `goFiles`: this is currently just the local input to
  `includeFromGoDirectives`. It is not public API.
- `includeBase`: this was private, single-use, and mostly hardcoded. It was
  inlined into final include construction.
- `exclude`: currently just shared vendor exclusion policy. It is not useful
  module introspection.

## Workspace Semantics

Do not accidentally change path roots.

`includeExtraFiles` is provided by the user as workspace-relative include
patterns. It must be applied to `ws.directory("/")`, not a module-root directory.

Current source construction is workspace-root based:

```dang
let include = [
  "**/*.go",
  "**/*.c",
  "**/*.h",
  "**/*.s",
  "**/*.S",
  "**/*.syso",
  "**/go.mod",
  "**/go.sum",
  "**/go.work",
  "**/go.work.sum",
] + includeExtraFiles + includeFromGoDirectives + includeFromGoModReplace

let source = ws.directory(
  "/",
  include: include,
  exclude: exclude,
)
```

Then commands use:

```dang
base
  .withDirectory(".", source)
  .withWorkdir(path)
```

That means `source` is workspace-shaped, and `path` selects the module working
directory inside it. Do not rewrite this into `ws.directory(path)` unless you are
intentionally changing the contract.

## Include Layers

The final per-module include list is different for each `GoModule`.

Shared part:

- built-in Go source patterns
- `includeExtraFiles`

Module-specific part:

- `includeFromGoDirectives`
- `includeFromGoModReplace`

The useful distinction is:

- `include` is what we asked the workspace for.
- `source` is the materialized directory result.

If users need to debug why a file was included, `include` is the causal data. If
they only need to know what files are present, `source` is enough.

## Lazy Fields And `Workspace`

Be careful with `ws: Workspace!`. It has cache invalidation consequences. Do not
casually stash workspace-derived values or store `ws` on `GoModule` without
thinking through what becomes pinned.

If moving toward lazy/cascading `GoModule` fields, split the world like this:

```text
| Candidate                 | Workspace                            | Derived/Input |
|---------------------------|--------------------------------------|---------------|
| `path`                    | No                                   | Input         |
| `version`                 | No                                   | Input         |
| `includeFromGoDirectives` | Direct                               | Derived       |
| `includeFromGoModReplace` | Direct                               | Derived       |
| `include`                 | Indirect (`includeFromGoDirectives`, | Derived       |
|                           | `includeFromGoModReplace`)           |               |
| `source`                  | Direct                               | Derived       |
| `testdata`                | Direct                               | Derived       |
| `base`                    | No                                   | Derived       |
| `testExec`                | Indirect (`source`, `testdata`)      | Derived       |
| `test`                    | Indirect (`testExec`)                | Derived       |
| `generate`                | Indirect (`source`)                  | Derived       |
```

Supporting inputs for a lazy design may include things that are not currently
stored on `GoModule`, such as `includeExtraFiles` or `exclude`. Do not call them
public `GoModule` fields just because lazy evaluation would need them.

## Indirection Rules

Remove an abstraction when all three are true:

- It is an intermediary abstraction.
- The implementation is small.
- It has one caller.

Examples already cleaned up or rejected:

- `workdir`
- `sourceIncludes`
- `goFilesWithSourceIncludes`
- `includeBase`
- private cleanup helpers whose only job was to wrap one expression

Keep an abstraction when it names a stable concept, prevents real duplication,
or marks a boundary users might reasonably debug.

## Tests

Tests belong in `.dagger/modules/e2e/main.dang`.

For public introspection fields, test real fixture behavior:

- `Go.module(ws, path)` returns the requested module.
- `includeFromGoDirectives` contains paths discovered from `go:embed` and
  `workspace:include`.
- `includeFromGoModReplace` contains local replacement paths.
- `source` contains files that should be mounted.

`testAll` currently forces multiple module test containers by merging a file
from each module's `testExec` into a directory and syncing that directory. The
comment should stay plain:

```dang
# FIXME: replace this when Dagger can directly run multiple containers in
# parallel. For now, merge one file from each Go module's test container
# into a directory and sync it to force every testExec to run.
```

## Communication Style For This Maintainer

Be concrete. Do not hand-wave.

- Show exact proposed schema snippets with final docstrings when asked.
- If the question is "what is this?", answer what it is, why it exists, and
  whether it is worth keeping.
- If a name is vague, fix the name or delete the concept.
- If a top-level function bypasses `GoModule`, assume it is wrong until proven
  otherwise.
- If challenged, do not defend bad shape. Re-read the code, correct the model,
  and move.
- Keep explanations short unless asked for depth.
