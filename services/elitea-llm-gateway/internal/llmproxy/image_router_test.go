package llmproxy

import (
	"sync/atomic"

	"github.com/maximhq/bifrost/core/schemas"
)

// imageRouter records whether an image method reached the provider.
type imageRouter struct {
	fakeRouter
	called atomic.Bool
}

func (r *imageRouter) imageCalled() bool { return r.called.Load() }

func (r *imageRouter) ImageGenerationRequest(ctx *schemas.BifrostContext, req *schemas.BifrostImageGenerationRequest) (*schemas.BifrostImageGenerationResponse, *schemas.BifrostError) {
	r.called.Store(true)
	return r.fakeRouter.ImageGenerationRequest(ctx, req)
}

func (r *imageRouter) ImageEditRequest(ctx *schemas.BifrostContext, req *schemas.BifrostImageEditRequest) (*schemas.BifrostImageGenerationResponse, *schemas.BifrostError) {
	r.called.Store(true)
	return r.fakeRouter.ImageEditRequest(ctx, req)
}

func (r *imageRouter) ImageVariationRequest(ctx *schemas.BifrostContext, req *schemas.BifrostImageVariationRequest) (*schemas.BifrostImageGenerationResponse, *schemas.BifrostError) {
	r.called.Store(true)
	return r.fakeRouter.ImageVariationRequest(ctx, req)
}
