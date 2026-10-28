package main

import (
	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/coordinator"
	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/providers/cloudflare"
	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/providers/nvidianim"
	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/providers/opencodego"
	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/providers/openrouterfree"
	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/providers/synthetic"
	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/providers/zai"
)

var runtimeState = coordinator.New(zai.NewModule(), opencodego.NewModule(), synthetic.NewModule(), cloudflare.NewModule(), nvidianim.NewModule(), openrouterfree.NewModule())
