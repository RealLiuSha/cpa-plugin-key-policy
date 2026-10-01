package policy

import "net/http"

// testAdmission is what a client sees for one request: CPA authenticates it
// and, when authentication deferred RPM and quota, the plugin's request
// interceptor admits it.
type testAdmission struct {
	AuthDecision
	Denial *Denial
}

func admitRequest(store *Store, method, path string, headers http.Header, body []byte) testAdmission {
	decision := store.Authenticate(method, path, headers, nil, body)
	result := testAdmission{AuthDecision: decision, Denial: decision.Denial}
	if decision.Allowed && decision.Deferred {
		if denial := store.AdmitIntercepted(headers, decision.Requested, path); denial != nil {
			result.Allowed, result.Reason, result.Denial = false, denial.Reason, denial
		}
	}
	return result
}

func freeTestModel(name, provider, targetModel string) ModelDefinition {
	return ModelDefinition{Name: name, Provider: provider, TargetModel: targetModel, BillingMode: "tokens"}
}

func tokenTestModel(name, provider, targetModel string, inputPrice, outputPrice float64) ModelDefinition {
	return ModelDefinition{
		Name: name, Provider: provider, TargetModel: targetModel, BillingMode: "tokens",
		InputPricePerMillion: inputPrice, OutputPricePerMillion: outputPrice,
	}
}

func perCallTestModel(name, provider, targetModel string, price float64) ModelDefinition {
	return ModelDefinition{Name: name, Provider: provider, TargetModel: targetModel, BillingMode: "per_call", PerCallUSD: price}
}

func modelRefs(names ...string) []KeyModelRef {
	refs := make([]KeyModelRef, 0, len(names))
	for _, name := range names {
		refs = append(refs, KeyModelRef{Name: name})
	}
	return refs
}
