package inventory

// One unexported decision this package's tests reach directly.
//
// sourcesError is the map from an expansion refusal to the status and the text
// a caller receives, and it is the only part of the invoke path a test cannot
// drive through the route: reaching its 503 arm needs a settings resolver that
// fails in a way no request can cause. Exposing it here rather than widening
// the package's own API keeps the mapping internal to production callers while
// making every arm of it assertable.

// StatusForSourceError is sourcesError, for tests in this package's external
// test package.
var StatusForSourceError = sourcesError
