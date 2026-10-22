# Combined search

`search.New(db)` binds a unified `index.Store`; `Engine.Compile(entity, ...)`
adds the entity's liveness clause, rejects foreign/mismatched namespaces, and
builds a reusable `Plan`. Prefer it to bare `search.Compile` when querying a
unified store. For examples see `cmd/imageidx/examples/bitmaponly` and
`search/README.md`.

| Index family | Search integration |
|---|---|
| `bitmapidx` | `AllValues`, `NamespaceText`, signed `NamespaceRange`, unsigned `NamespaceBSIRange` (`Engine.BSIRange`) |
| `timeidx` | `During`; use `ExactTimestamp` to verify lossy candidates |
| `geoidx` | `InCells` |
| `radixhash` | exact-content duplicate/presence checks through `index.Matcher`; not a Search clause |
| `simhash` | import duplicate detection through `index.Matcher`, not a plan clause |
| `vectoridx` | ANN outside the plan; `FilterRanked` filters candidate results |

Plan stages are ordered by compile-time cardinality hints; call `Refresh`
after substantial data changes. `AppendIDs` produces sorted unique IDs,
`FilterIDs` preserves candidate order and duplicates, `Count` uses bitmap
cardinality where possible, and `AppendPage`/`Visit`/ranking stream verified
batches. `Workspace` is exclusive to one execution; `Executor` pools it for
concurrent callers. Stages read current index views independently, not a
database-wide snapshot. `WithACL` applies image grants only; authenticate
group memberships before querying.
