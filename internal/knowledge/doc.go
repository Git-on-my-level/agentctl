// Package knowledge compiles independently authored Git repositories into an
// immutable, content-addressed bundle. SyncSource is the sole write/fetch
// operation; Parse, Ingest, Compile, Verify, LoadBundle, and context consumers are
// read-only and never refresh Git state implicitly.
package knowledge
