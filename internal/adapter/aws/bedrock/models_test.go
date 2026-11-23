package bedrock

import (
	"context"
	"errors"
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	awsbedrock "github.com/aws/aws-sdk-go-v2/service/bedrock"
	"github.com/aws/aws-sdk-go-v2/service/bedrock/types"
)

type fakeModelAPI struct {
	out *awsbedrock.ListFoundationModelsOutput
	err error
}

func (f fakeModelAPI) ListFoundationModels(context.Context, *awsbedrock.ListFoundationModelsInput, ...func(*awsbedrock.Options)) (*awsbedrock.ListFoundationModelsOutput, error) {
	return f.out, f.err
}

func fmSummary(id, name, provider string, status types.FoundationModelLifecycleStatus, in ...types.ModelModality) types.FoundationModelSummary {
	return types.FoundationModelSummary{
		ModelId: awssdk.String(id), ModelName: awssdk.String(name), ProviderName: awssdk.String(provider),
		InputModalities:         in,
		InferenceTypesSupported: []types.InferenceType{types.InferenceTypeOnDemand},
		ModelLifecycle:          &types.FoundationModelLifecycle{Status: status},
	}
}

func TestListModels(t *testing.T) {
	prov := fmSummary("p.model", "P", "Acme", types.FoundationModelLifecycleStatusActive, types.ModelModalityText)
	prov.InferenceTypesSupported = []types.InferenceType{types.InferenceTypeProvisioned}
	out := &awsbedrock.ListFoundationModelsOutput{ModelSummaries: []types.FoundationModelSummary{
		fmSummary("a.model", "", "Acme", types.FoundationModelLifecycleStatusActive, types.ModelModalityText),
		fmSummary("b.model", "B", "Beta", types.FoundationModelLifecycleStatusLegacy, types.ModelModalityText),
		fmSummary("image.only", "I", "Acme", types.FoundationModelLifecycleStatusActive, types.ModelModalityImage),
		prov,
		{ModelId: awssdk.String("")},
	}}
	got, err := NewModelsWithAPI(fakeModelAPI{out: out}).ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "a.model" || got[0].DisplayName != "a.model" || got[0].Provider != "acme" || got[1].ID != "b.model" {
		t.Fatalf("got %+v", got)
	}
}

func TestListModelsError(t *testing.T) {
	if _, err := NewModelsWithAPI(fakeModelAPI{err: errors.New("boom")}).ListModels(context.Background()); err == nil {
		t.Fatal("want error")
	}
}
