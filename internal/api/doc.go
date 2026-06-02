// Package api implements the local control API for the chiling daemon.
//
// The control API is a plain HTTP server hosted inside the proxy daemon process,
// accessible over a Unix domain socket (primary) and/or localhost TCP (fallback).
// All routes are under /v1/ and return JSON responses.
//
// Routes:
//
//	GET /v1/status           Daemon status and configuration
//	GET /v1/events           Live event subscription (Server-Sent Events)
//	GET /v1/requests         Request list (with pagination: ?since=, ?limit=)
//	GET /v1/requests/:id     Request detail (?include=headers,body,rule-trace)
//
// Middleware:
//
// All requests pass through a recovery middleware (panics → 500 JSON error) and
// a logging middleware (method, path, status, duration).
package api
