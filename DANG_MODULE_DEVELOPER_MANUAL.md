# Dang Module Developer Manual

This is a manual for writing Dang modules whose APIs are usable, debuggable, and
not full of ornamental plumbing.

## First Principles

- Hunt useless indirection aggressively. Check callers often with `rg`; do not
  trust yourself to remember the shape of the code.
- Prefer inlining until an abstraction proves it names a stable concept,
  removes real duplication, or exposes a useful debugging boundary.
- Model the public API as a domain object graph. Facts about one object belong
  on that object's type, not on a top-level function with an object path
  argument.
- Use cascading fields for derived values. Store inputs; compute derived values
  lazily from other fields.
- Think of object fields as a DAG. If you rendered that DAG to the user, every
  node should make sense as a question they might ask.
- Do not make a field just because it is convenient internally. If it would be
  embarrassing or confusing as public API, it probably should be local.
- When changing behavior or public API, update tests.

## Indirection Audit

Do this repeatedly while working, not once at the end.

1. List the fields and functions on each type.
2. For every private helper, run `rg` for its name.
3. If it has one caller, inspect the implementation and the call site together.
4. Inline it when it is small and does not name a real concept.
5. Re-read the resulting type. If the shape is clearer, keep the inline.

Remove an abstraction when all three are true:

- It is an intermediary abstraction.
- The implementation is small.
- It has one caller.

Keep an abstraction when it names a stable concept, prevents real duplication,
or marks a boundary users might reasonably debug.

Common bad indirections:

- A constant or hardcoded list with one consumer.
- A temporary pruning optimization promoted into a field.
- A helper input that exists only because the implementation was eager.
- A path indirection that always returns the same mount point.
- A vaguely named value that does not distinguish causal input from materialized
  result.

## Artifact Collection Pattern

Most modules collect artifacts: modules, packages, services, charts, images,
projects, test targets, generated outputs. Use one shape unless the domain
forces another.

```dang
type Tool {
  pub version: String! = "1.0"

  let artifactPaths(ws: Workspace!): [String!]! {
    ws.directory("/", include: ["**/artifact.toml"]).glob("**/artifact.toml")
      .map { path => path.trimSuffix("/artifact.toml") }
  }

  pub artifacts(ws: Workspace!): [Artifact!]! {
    artifactPaths(ws).map { path => artifact(path) }
  }

  pub artifact(path: String!): Artifact! {
    Artifact(
      path: path,
      version: version,
    )
  }
}

type Artifact {
  pub path: String!

  let version: String!

  pub source(ws: Workspace!): Directory! {
    ws.directory("/", include: include(ws))
  }

  pub result(ws: Workspace!): Directory! {
    base
      .withDirectory(".", source(ws))
      .withWorkdir(path)
      .directory(".")
  }
}
```

The pattern is:

- `artifacts(ws)` scans for paths.
- `artifacts(ws)` calls `artifact(path)` for each path.
- `artifact(path)` constructs the child object from stored inputs.
- Inputs are fields.
- Derived values are cascading lazy fields.
- Direct lookup does not need to rescan every artifact just to validate one
  requested path.

If compatibility or schema ergonomics require a context argument on direct
lookup, keep it boring. It should still construct the child object, not perform
a whole collection scan.

## Cascading Field Style

Stored fields should be inputs or identity. Derived fields should be functions
that compute from those inputs and from other derived fields.

```text
| Value Kind                  | Stored Field | Takes `ws` | Usually Public |
|-----------------------------|--------------|------------|----------------|
| User configuration          | Yes          | No         | On root        |
| Object identity             | Yes          | No         | Yes            |
| Private carried input       | Yes          | No         | No             |
| Workspace scan              | No           | Yes        | Maybe          |
| Named contribution layer    | No           | Yes        | Maybe          |
| Final causal input          | No           | Yes        | Often          |
| Materialized workspace data | No           | Yes        | Often          |
| Helper container            | No           | Maybe      | No             |
| Command/check result        | No           | Yes        | Yes            |
```

Do not store `ws: Workspace!` on child objects casually. Workspace-derived
values should usually take `ws` explicitly so cache invalidation remains visible
at the call site.

Do not make private carried inputs public just because lazy evaluation needs
them. Public surface is for users, not for explaining how the constructor works.

## Field DAG Test

Before adding or exposing a field, sketch the object as a DAG:

```text
path
version
includeFromConfig(ws)
include(ws) -> includeFromConfig(ws)
source(ws)  -> include(ws)
test(ws)    -> source(ws)
```

Then ask:

- Does every node answer a user-understandable question?
- Does the name describe the value, not the implementation accident?
- Is this value causal data, materialized data, or just a temporary local?
- If the node had a docstring and appeared in GraphQL, would it clarify the
  module?
- Is this really a child-object fact, or did it leak onto the root type?

If the node does not pass that test, keep it private, make it local, or delete
it.

## Public API Shape

Keep the root type for global configuration, discovery, direct lookup, and
whole-workspace operations.

The root object should usually offer two entry points for child objects:

```dang
pub artifacts(ws: Workspace!): [Artifact!]!

pub artifact(path: String!): Artifact!
```

Use `artifacts(ws)` for discovery. Use `artifact(path)` when a caller already
knows which object they want.

Object-specific facts then hang off the child object:

```dang
pub path: String!

pub include(ws: Workspace!): [String!]!

pub source(ws: Workspace!): Directory!
```

Do not mirror those as root-level parameterized functions. That creates a second
API path, makes the schema harder to reason about, and usually exposes internal
implementation shape instead of the user's domain model.

## Useful Introspection

Good public introspection answers one of these questions:

- What objects did the module discover?
- What did this object ask the workspace for?
- What did this object materialize from the workspace?
- Which named layer contributed to the final input?
- What command/container/result will the module run?

If users need to debug why a file or value was selected, expose the causal data.
If they only need to inspect the result, expose the materialized result.

## Native Helper Boundary

Dang is good at object shape, dependency wiring, workspace selection, container
composition, and exposing a clean GraphQL API. It is not where complex parsing
or domain-specific file semantics should live by default.

Use a native helper when the work needs:

- A real parser.
- Standard-library behavior from the target ecosystem.
- Careful path, syntax, or manifest handling.
- More than a small amount of string manipulation.
- Logic that is easier to test as normal code than as Dang expressions.

Keep the boundary boring:

- Dang chooses the files and calls the helper.
- The helper receives explicit paths or mounted input directories.
- The helper emits structured data, or a simple line-oriented format when the
  result is just a list.
- Dang turns that result into named cascading fields.

Do not create a helper just to hide one expression. Do not let helper internals
leak into public field names. The public API should name the module concept, not
the implementation technique.

The right boundary usually looks like this:

```text
Workspace scan -> small native parser -> named derived field -> final source
```

The wrong boundary usually looks like this:

```text
Workspace scan -> vague helper wrapper -> vague field -> another wrapper
```

## Workspace Semantics

Be precise about path roots.

`ws.directory("/")` and `ws.directory(path)` are different contracts. Include
patterns passed to a workspace-root directory are workspace-relative. Include
patterns passed to an object-root directory are object-relative. Moving the root
can silently change what gets included.

A common safe shape is:

```dang
pub source(ws: Workspace!): Directory! {
  ws.directory(
    "/",
    include: include(ws),
    exclude: exclude,
  )
}

base
  .withDirectory(".", source(ws))
  .withWorkdir(path)
```

That keeps the mounted directory workspace-shaped and uses `path` only to choose
where the command runs inside it.

## Include Layers

Keep these concepts separate:

- A base include layer is shared policy.
- A named include layer explains one source of additional inputs.
- The final include list is the causal input passed to `Workspace.directory`.
- The source directory is the materialized result.

Expose the final include list when users need to debug selection. Expose named
layers only when the layer name maps to something users already understand.
Inline single-use layers that are only there to make the implementation look
busy.

Do not normalize paths just to normalize paths. Keep a normalization helper only
when it prevents a real error. If the producer already returns valid
workspace-relative paths, pass them through.

## Comments

Comments should explain why the code has to be weird, not narrate what the next
line already says.

For temporary execution hacks, name the limitation plainly:

```dang
# FIXME: replace this when Dagger can directly run multiple containers in
# parallel. For now, merge one file from each test container into a directory
# and sync it to force every execution to run.
```

Avoid comments that depend on implied knowledge like "when checks can directly
wait" or "module executions" without saying which system is missing which
capability.

## Tests

Tests should cover public behavior and the debugging surface users rely on.

For public introspection, prefer fixture-backed checks:

- Direct lookup returns a handle with the requested identity.
- A named layer contains data discovered from a real fixture.
- The final include list contains the causal paths users expect.
- The materialized directory contains files that should be mounted.
- Batch operations still exercise every discovered object.

Use the module's declared test command from local repo instructions. If there is
no declared command, add one before growing the module.

## Working Style

Be concrete. Do not hand-wave.

- Show exact proposed schema snippets with final docstrings when asked.
- If the question is "what is this?", answer what it is, why it exists, and
  whether it is worth keeping.
- If a name is vague, fix the name or delete the concept.
- If a top-level function bypasses a child object, assume it is wrong until
  proven otherwise.
- If challenged, re-read the code, correct the model, and move.
- Keep explanations short unless asked for depth.
