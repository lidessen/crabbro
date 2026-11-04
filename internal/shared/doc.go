// Package shared is the daemon-facing view of the control API.
//
// Wire types and the socket defaults live in pkg/api. This package aliases
// them so existing daemon code keeps one definition, and it holds the
// daemon-only limits (timeouts, body caps, ring size).
package shared
