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

// pdfEngineConcurrency is how many PDFs one process extracts at the same
// time. Each extraction holds its own WebAssembly linear memory (up to the
// memory limit), so this number is also a memory bound.
const pdfEngineConcurrency = 2

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
// host root "/" by default), no stdout or stderr, and a memory ceiling.
type pdfEngine struct {
	memoryLimitPages uint32
	cache            wazero.CompilationCache
	slots            chan struct{}
}

func newPDFEngine(memoryLimitPages uint32) *pdfEngine {
	return &pdfEngine{
		memoryLimitPages: memoryLimitPages,
		cache:            wazero.NewCompilationCache(),
		slots:            make(chan struct{}, pdfEngineConcurrency),
	}
}

func (engine *pdfEngine) open(ctx context.Context) (pdfium.Pool, pdfium.Pdfium, error) {
	config := wazero.NewRuntimeConfig().
		WithCloseOnContextDone(true).
		WithCompilationCache(engine.cache)
	if engine.memoryLimitPages > 0 {
		config = config.WithMemoryLimitPages(engine.memoryLimitPages)
	}
	pool, err := webassembly.Init(webassembly.Config{
		Context:       ctx,
		MinIdle:       0,
		MaxIdle:       1,
		MaxTotal:      1,
		RuntimeConfig: config,
		FSConfig:      wazero.NewFSConfig(),
		Stdout:        io.Discard,
		Stderr:        io.Discard,
	})
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
	builder.unitCount = count.PageCount
	pages := count.PageCount
	if pages > limits.MaxPages {
		pages = limits.MaxPages
		builder.stop(PartialPageLimit)
	}
	for index := range pages {
		if err := ctx.Err(); err != nil {
			return err
		}
		text, err := pageText(instance, opened.Document, index)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// One broken page is recorded as a page without text rather
			// than refusing a document whose other pages read fine.
			text = ""
		}
		if !builder.add(UnitPage, strconv.Itoa(index+1), text) {
			return nil
		}
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
