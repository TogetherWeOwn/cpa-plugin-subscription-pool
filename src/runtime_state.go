package main

import (
	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/coordinator"
	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/providers/opencodego"
	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/providers/synthetic"
	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/providers/zai"
)

var runtimeState = coordinator.New(zai.NewModule(), opencodego.NewModule(), synthetic.NewModule())
