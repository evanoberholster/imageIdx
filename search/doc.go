// Package search plans and executes conjunctive queries across bitmap, geo,
// and timestamp indexes.
//
// Geographic clauses accept S2 cell IDs through WithinCells; they do not
// accept latitude/longitude bounds. Reused plans, result buffers, and
// workspaces allow zero-allocation steady-state execution.
//
// Each index keeps its existing publication and consistency model. A combined
// query observes one independently loaded snapshot per compiled stage; it is
// not a database-wide transaction snapshot. Index methods manage read-side
// lifetime registration internally.
package search
