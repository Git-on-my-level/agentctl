//go:build !unix

package unixsocket

// PathLimit uses the portable BSD filesystem address limit on other platforms.
const PathLimit = 103
