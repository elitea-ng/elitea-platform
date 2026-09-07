package shadow

import (
	"bytes"
	"crypto/rand"
	"io"
	"math/big"
	"net/http"
	"time"
)

type responseCapture struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (r *responseCapture) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *responseCapture) Write(b []byte) (int, error) {
	r.body.Write(b)
	return r.ResponseWriter.Write(b)
}

type MiddlewareConfig struct {
	Comparator *Comparator
	Metrics    *Metrics
}

func MiddlewareWithMetrics(cfg MiddlewareConfig) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !cfg.Comparator.cfg.Enabled {
				next.ServeHTTP(w, r)
				return
			}

			if !shouldSample(cfg.Comparator.Weight()) {
				next.ServeHTTP(w, r)
				return
			}

			capture := &responseCapture{
				ResponseWriter: w,
				status:         http.StatusOK,
			}

			start := time.Now()
			next.ServeHTTP(capture, r)
			newLatency := time.Since(start)

			bodySnapshot := make([]byte, capture.body.Len())
			copy(bodySnapshot, capture.body.Bytes())

			go func() {
				result := cfg.Comparator.Compare(
					r.Context(),
					r.Method,
					r.URL.Path,
					capture.status,
					bodySnapshot,
					r.Header,
				)
				result.NewLatency = newLatency
				if cfg.Metrics != nil {
					cfg.Metrics.Record(result)
				}
			}()
		})
	}
}

func Middleware(comparator *Comparator) func(http.Handler) http.Handler {
	return MiddlewareWithMetrics(MiddlewareConfig{Comparator: comparator})
}

func shouldSample(weight float64) bool {
	if weight >= 1.0 {
		return true
	}
	if weight <= 0.0 {
		return false
	}
	n, err := rand.Int(rand.Reader, big.NewInt(1000))
	if err != nil {
		return false
	}
	return float64(n.Int64()) < weight*1000
}

func DrainBody(r *http.Request) []byte {
	if r.Body == nil {
		return nil
	}
	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(body))
	return body
}
