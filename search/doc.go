// Package search plans and executes conjunctive queries across bitmap, geo,
// and timestamp indexes.
//
// Each index keeps its existing publication and consistency model. A combined
// query observes one independently loaded snapshot per scheduled stage; it is
// not a database-wide transaction snapshot.
package search
