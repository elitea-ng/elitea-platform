package middleware

import (
	"net/http"

	"github.com/elitea-ng/elitea-platform/libs/go/faults"
	"github.com/elitea-ng/elitea-platform/libs/go/faults/httperr"
)

// SupportRef gives every request a support reference generated here (FU-299
// C1). It is put on the request's context, so every log line of the request
// carries it, and returned in X-Elitea-Support-Ref and in every error body.
// A client's X-Request-Id or X-Elitea-Support-Ref is never used: a client
// must not be able to choose, forge or inject the reference others search by.
func SupportRef(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ref := faults.NewSupportRef()
		w.Header().Set(httperr.SupportRefHeader, ref)
		next.ServeHTTP(w, r.WithContext(faults.WithSupportRef(r.Context(), ref)))
	})
}
