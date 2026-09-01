// Package temporal implements the durable temporal value types defined by the
// openCypher M23 date/time proposal.  The values are immutable, comparable Go
// structs and deliberately do not use time.Local.
//
// Named-zone construction uses Go's IANA timezone loader.  The time/tzdata
// import makes the timezone database shipped with the pinned Go toolchain
// available on systems without a zoneinfo installation.  A resolved offset is
// retained in every DateTime value, so decoding, equality, indexing, accessors,
// and formatting do not change when the host timezone database changes.  New
// named-zone construction and calendar arithmetic use the rules available to
// the running binary and can therefore reflect a newer timezone database.
package temporal
