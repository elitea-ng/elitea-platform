package extract

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"

	"github.com/klippa-app/go-pdfium"
	pdfiumerrors "github.com/klippa-app/go-pdfium/errors"
	"github.com/klippa-app/go-pdfium/references"
	"github.com/klippa-app/go-pdfium/requests"
	"github.com/klippa-app/go-pdfium/webassembly"
	"github.com/tetratelabs/wazero"
)

// PDFEngineConcurrency is how many PDFs one process extracts at the same
// time. Each extraction holds its own WebAssembly linear memory (up to the
// memory limit, and briefly twice that while it grows), so this number is
// also a memory bound. It is one: elitea-main is one API process, and a
// second PDF waits for the first rather than doubling the peak.
const PDFEngineConcurrency = 1

// pdfEngine runs PDFium compiled to WebAssembly.
//
// ONE RUNTIME PER EXTRACTION, and that is the cancellation design. go-pdfium
// derives every instance's context from the pool's Config.Context, and with
// WithCloseOnContextDone wazero stops a running WebAssembly call when that
// context ends. So a deadline on the caller's context interrupts a parse that
// would otherwise loop for ever, without calling Instance.Kill — which races
// with an in-flight call inside go-pdfium. The compiled module is cached and
// shared, so a new runtime costs an instantiation, not a compilation.
//
// THE SANDBOX. The module gets an EMPTY file system (go-pdfium mounts the
// host root "/" by default), no stdout or stderr, and a memory ceiling. The
// whole configuration comes from poolConfig, and pdf_sandbox_test.go pins
// that it mounts nothing.
type pdfEngine struct {
	memoryLimitPages uint32
	cache            wazero.CompilationCache
	slots            chan struct{}
}

// pdfCompilationCache holds the compiled PDFium module for the whole process.
// The compilation depends only on the module, not on an engine's memory
// ceiling, so every Extractor shares it: a second Extractor (a test, or a
// composition that builds more than one) does not compile PDFium again.
var pdfCompilationCache = wazero.NewCompilationCache()

func newPDFEngine(memoryLimitPages uint32) *pdfEngine {
	return &pdfEngine{
		memoryLimitPages: memoryLimitPages,
		cache:            pdfCompilationCache,
		slots:            make(chan struct{}, PDFEngineConcurrency),
	}
}

// poolConfig is the ONE place the WebAssembly pool is configured. Every
// sandbox property is set here: an empty file system, no output streams, a
// memory ceiling, and a runtime that stops when ctx ends.
func (engine *pdfEngine) poolConfig(ctx context.Context) webassembly.Config {
	runtime := wazero.NewRuntimeConfig().
		WithCloseOnContextDone(true).
		WithCompilationCache(engine.cache)
	if engine.memoryLimitPages > 0 {
		runtime = runtime.WithMemoryLimitPages(engine.memoryLimitPages)
	}
	return webassembly.Config{
		Context:       ctx,
		MinIdle:       0,
		MaxIdle:       1,
		MaxTotal:      1,
		RuntimeConfig: runtime,
		// An empty FSConfig mounts no directory. Without it go-pdfium
		// mounts the host root, and a path-based open inside a hostile
		// document could reach elitea-main's secrets and certificates.
		FSConfig: wazero.NewFSConfig(),
		Stdout:   io.Discard,
		Stderr:   io.Discard,
	}
}

func (engine *pdfEngine) open(ctx context.Context) (pdfium.Pool, pdfium.Pdfium, error) {
	pool, err := webassembly.Init(engine.poolConfig(ctx))
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		return nil, nil, fmt.Errorf("%w: pdf engine: %w", ErrUnavailable, err)
	}
	instance, err := pool.GetInstanceWithContext(ctx)
	if err != nil {
		_ = pool.Close()
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		return nil, nil, fmt.Errorf("%w: pdf instance: %w", ErrUnavailable, err)
	}
	return pool, instance, nil
}

// warm opens and closes one instance, which compiles the module into the
// engine's cache. It takes a slot like an extraction, so it never adds to
// the memory bound.
func (engine *pdfEngine) warm(ctx context.Context) error {
	select {
	case engine.slots <- struct{}{}:
		defer func() { <-engine.slots }()
	case <-ctx.Done():
		return ctx.Err()
	}
	pool, instance, err := engine.open(ctx)
	if err != nil {
		return err
	}
	_ = instance.Close()
	return pool.Close()
}

// extract reads every page's text layer.
func (engine *pdfEngine) extract(
	ctx context.Context,
	data []byte,
	limits Limits,
	builder *textBuilder,
) error {
	select {
	case engine.slots <- struct{}{}:
		defer func() { <-engine.slots }()
	case <-ctx.Done():
		return ctx.Err()
	}
	pool, instance, err := engine.open(ctx)
	if err != nil {
		return err
	}
	defer func() {
		_ = instance.Close()
		_ = pool.Close()
	}()
	return readPDF(ctx, instance, data, limits, builder)
}

func readPDF(
	ctx context.Context,
	instance pdfium.Pdfium,
	data []byte,
	limits Limits,
	builder *textBuilder,
) error {
	opened, err := instance.OpenDocument(&requests.OpenDocument{File: &data})
	if err != nil {
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case errors.Is(err, pdfiumerrors.ErrPassword), errors.Is(err, pdfiumerrors.ErrSecurity):
			return refuse(ReasonEncrypted, err)
		}
		return refuse(ReasonMalformed, err)
	}
	defer func() {
		_, _ = instance.FPDF_CloseDocument(&requests.FPDF_CloseDocument{Document: opened.Document})
	}()
	count, err := instance.FPDF_GetPageCount(&requests.FPDF_GetPageCount{Document: opened.Document})
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return refuse(ReasonMalformed, err)
	}
	return readPages(ctx, count.PageCount, limits, builder, func(index int) (string, error) {
		return pageText(instance, opened.Document, index)
	})
}

// readPages adds each page in turn. The page limit is recorded only when it
// is what actually stopped the read: a text limit reached at page 600 of a
// 2,500-page document is a text limit, and the worker says so.
func readPages(
	ctx context.Context,
	pageCount int,
	limits Limits,
	builder *textBuilder,
	text func(index int) (string, error),
) error {
	builder.unitCount = pageCount
	pages := min(pageCount, limits.MaxPages)
	for index := range pages {
		if err := ctx.Err(); err != nil {
			return err
		}
		body, err := text(index)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// One broken page is recorded as a page without text rather
			// than refusing a document whose other pages read fine.
			body = ""
		}
		if !builder.add(UnitPage, strconv.Itoa(index+1), body) {
			return nil
		}
	}
	if pages < pageCount {
		builder.stop(PartialPageLimit)
	}
	return nil
}

func pageText(instance pdfium.Pdfium, document references.FPDF_DOCUMENT, index int) (string, error) {
	response, err := instance.GetPageText(&requests.GetPageText{
		Page: requests.Page{ByIndex: &requests.PageByIndex{Document: document, Index: index}},
	})
	if err != nil {
		return "", err
	}
	return response.Text, nil
}
