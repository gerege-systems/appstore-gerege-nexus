package registry

// What the credential helpers look like from the outside.
//
// They are unexported because nothing outside this package should be minting or
// hashing a platform token, and they are tested because both are the kind of
// small function whose bug is invisible until it is somebody else's catalogue:
// a parser that accepts a token on the wrong scheme, a digest that is not a
// digest. This file is the usual Go compromise — the test package sees them,
// the product does not.

var (
	BearerTokenForTest = bearerToken
	NewTokenForTest    = newToken
	TokenDigestForTest = tokenDigest
)
