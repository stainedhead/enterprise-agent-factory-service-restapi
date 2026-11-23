package bedrock

import (
	"context"
	"fmt"
	"strings"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	awsbedrock "github.com/aws/aws-sdk-go-v2/service/bedrock"
	"github.com/aws/aws-sdk-go-v2/service/bedrock/types"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/housekeeping"
)

// modelAPI is the part of the Bedrock client the model lister uses.
type modelAPI interface {
	ListFoundationModels(ctx context.Context, in *awsbedrock.ListFoundationModelsInput, opts ...func(*awsbedrock.Options)) (*awsbedrock.ListFoundationModelsOutput, error)
}

// Models implements housekeeping.ModelLister: the text models Bedrock offers in the region.
type Models struct{ api modelAPI }

var _ housekeeping.ModelLister = (*Models)(nil)

// NewModels returns a Models on an SDK configuration.
func NewModels(cfg awssdk.Config) *Models { return &Models{api: awsbedrock.NewFromConfig(cfg)} }

// NewModelsWithAPI returns a Models on api (tests).
func NewModelsWithAPI(api modelAPI) *Models { return &Models{api: api} }

// ListModels implements housekeeping.ModelLister. A model is listed when it takes text in and gives text out
// and can be called on demand or through an inference profile; a model whose lifecycle is LEGACY is still
// listed (an admin decides), one that is not ACTIVE or LEGACY is not.
func (m *Models) ListModels(ctx context.Context) ([]housekeeping.CatalogModel, error) {
	out, err := m.api.ListFoundationModels(ctx, &awsbedrock.ListFoundationModelsInput{ByOutputModality: types.ModelModalityText})
	if err != nil {
		return nil, fmt.Errorf("bedrock: ListFoundationModels: %w", err)
	}
	var res []housekeeping.CatalogModel
	for _, s := range out.ModelSummaries {
		id := awssdk.ToString(s.ModelId)
		if id == "" || !callable(s) || !textIn(s) {
			continue
		}
		if s.ModelLifecycle != nil && s.ModelLifecycle.Status != types.FoundationModelLifecycleStatusActive && s.ModelLifecycle.Status != types.FoundationModelLifecycleStatusLegacy {
			continue
		}
		name := awssdk.ToString(s.ModelName)
		if name == "" {
			name = id
		}
		res = append(res, housekeeping.CatalogModel{ID: id, Provider: strings.ToLower(awssdk.ToString(s.ProviderName)), DisplayName: name})
	}
	return res, nil
}

func callable(s types.FoundationModelSummary) bool {
	for _, t := range s.InferenceTypesSupported {
		if t == types.InferenceTypeOnDemand {
			return true
		}
	}
	return false
}

func textIn(s types.FoundationModelSummary) bool {
	for _, mo := range s.InputModalities {
		if mo == types.ModelModalityText {
			return true
		}
	}
	return false
}
