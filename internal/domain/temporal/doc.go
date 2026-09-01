// Package temporal implements the durable temporal value types defined by the
// openCypher M23 date/time proposal.  The values are immutable, comparable Go
// structs and deliberately do not use time.Local.
//
// Default named-zone construction uses the checksum-pinned IANA release and
// profile identified by PinnedTZDBVersion and PinnedTZDBProfile. It never
// consults ZONEINFO or host files. A resolved offset is retained in every
// DateTime value, so decoding, equality, indexing, accessors, and formatting
// also remain stable. Applications may inject a different ZoneDatabase
// explicitly where required.
package temporal
