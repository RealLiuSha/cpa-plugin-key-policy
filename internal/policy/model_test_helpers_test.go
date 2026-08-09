package policy

func freeTestModel(name, provider, targetModel string) ModelDefinition {
	return ModelDefinition{
		Name: name, Targets: []ModelTarget{{Provider: provider, TargetModel: targetModel}},
		Dispatch: "round-robin", BillingMode: "tokens", Free: true,
	}
}

func tokenTestModel(name, provider, targetModel string, inputPrice, outputPrice float64) ModelDefinition {
	return ModelDefinition{
		Name: name, Targets: []ModelTarget{{Provider: provider, TargetModel: targetModel}},
		Dispatch: "round-robin", BillingMode: "tokens",
		InputPricePerMillion: inputPrice, OutputPricePerMillion: outputPrice,
	}
}

func perCallTestModel(name, provider, targetModel string, price float64) ModelDefinition {
	return ModelDefinition{
		Name: name, Targets: []ModelTarget{{Provider: provider, TargetModel: targetModel}},
		Dispatch: "round-robin", BillingMode: "per_call", PerCallUSD: price,
	}
}

func modelRefs(names ...string) []KeyModelRef {
	refs := make([]KeyModelRef, 0, len(names))
	for _, name := range names {
		refs = append(refs, KeyModelRef{Name: name})
	}
	return refs
}
