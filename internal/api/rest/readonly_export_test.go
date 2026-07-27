package rest

// IsReadRequest exposes isReadRequest to the external rest_test package, where
// the spec-derived classification gate lives alongside the request-level
// enforcement tests that need rest_test's server fixtures. The classifier stays
// unexported in the production API: it encodes this package's own read/write
// policy and has no caller outside it.
var IsReadRequest = isReadRequest
