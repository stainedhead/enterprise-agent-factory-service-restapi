package domain

import "slices"

// Service settings added by the orchestration agent and model access change (change PRD §6.8). The
// generated registry (settings_gen.go) is built from the root PRD §6.13; these keys are in the change
// PRD, so they are listed here. The database holds the authoritative CHECKs (000022); these specs give
// the early, readable error and the defaults.
const (
	// SettingOrchestrationTools is "orchestration.tools": each tool once with enabled, default, templateId and imageRepository.
	SettingOrchestrationTools = "orchestration.tools"
	// SettingOrchestrationPublicBaseDomain is "orchestration.public_base_domain" (the 1.0.1 collab.base_domain).
	SettingOrchestrationPublicBaseDomain = "orchestration.public_base_domain"
	// SettingOrchestrationPrivateBaseDomain is "orchestration.private_base_domain" (unset disables private exposure).
	SettingOrchestrationPrivateBaseDomain = "orchestration.private_base_domain"
	// SettingOrchestrationPublicEndpoints is "orchestration.public_endpoints" (the 1.0.1 collab.public_endpoints).
	SettingOrchestrationPublicEndpoints = "orchestration.public_endpoints"
	// SettingOrchestrationPrivateEndpoints is "orchestration.private_endpoints".
	SettingOrchestrationPrivateEndpoints = "orchestration.private_endpoints"
	// SettingOrchestrationDefaultLimit is "orchestration.default_limit".
	SettingOrchestrationDefaultLimit = "orchestration.default_limit"
	// SettingOrchestrationDefaultWebUIExposure is "orchestration.default_web_ui_exposure".
	SettingOrchestrationDefaultWebUIExposure = "orchestration.default_web_ui_exposure"
	// SettingOrchestrationDefaultWebAPIExposure is "orchestration.default_web_api_exposure".
	SettingOrchestrationDefaultWebAPIExposure = "orchestration.default_web_api_exposure"
	// SettingOrchestrationDefaultResources is "orchestration.default_resources" ({cpu, memory}).
	SettingOrchestrationDefaultResources = "orchestration.default_resources"
	// SettingOrchestrationDefaultVolumeGiB is "orchestration.default_volume_gib".
	SettingOrchestrationDefaultVolumeGiB = "orchestration.default_volume_gib"
	// SettingModelsServiceAccess is "models.service_access" (all or selected); changed through PutServiceModelAccess.
	SettingModelsServiceAccess = "models.service_access"
	// SettingModelsDefaultTeamAccess is "models.default_team_access".
	SettingModelsDefaultTeamAccess = "models.default_team_access"
	// SettingModelsDefaultTeamModels is "models.default_team_models".
	SettingModelsDefaultTeamModels = "models.default_team_models"
	// SettingModelsDefaultModelID is "models.default_model_id".
	SettingModelsDefaultModelID = "models.default_model_id"
)

const dnsNamePattern = "^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\\.)+[a-z]{2,63}$"

// OrchestrationSettingSpecs lists the settings of change PRD §6.8 in PRD order.
var OrchestrationSettingSpecs = []SettingSpec{
	{Key: SettingOrchestrationTools, Type: "object", Default: `{"paperclip":{"enabled":true,"default":true,"templateId":null,"imageRepository":null,"webUi":true,"webApi":true,"registration":["manual","managed"],"inNamespacePort":3100},"buzz":{"enabled":false,"default":false,"templateId":null,"imageRepository":null,"webUi":false,"webApi":false,"registration":["managed"],"inNamespacePort":3000}}`, Allowed: "each tool once; exactly one default; the template must have orchestration and the tool capability; the profile members are fixed"},
	{Key: SettingOrchestrationPublicBaseDomain, Type: "string", Default: "null", Nullable: true, Pattern: dnsNamePattern, Allowed: "DNS name; unset disables public exposure"},
	{Key: SettingOrchestrationPrivateBaseDomain, Type: "string", Default: "null", Nullable: true, Pattern: dnsNamePattern, Allowed: "DNS name; unset disables private exposure"},
	{Key: SettingOrchestrationPublicEndpoints, Type: "boolean", Default: "true", Allowed: "boolean"},
	{Key: SettingOrchestrationPrivateEndpoints, Type: "boolean", Default: "true", Allowed: "boolean"},
	{Key: SettingOrchestrationDefaultLimit, Type: "string", Default: `"private"`, Pattern: "^(off|private|public)$", Allowed: "off, private, public"},
	{Key: SettingOrchestrationDefaultWebUIExposure, Type: "string", Default: `"private"`, Pattern: "^(off|private|public)$", Allowed: "off, private, public; not above default_limit"},
	{Key: SettingOrchestrationDefaultWebAPIExposure, Type: "string", Default: `"off"`, Pattern: "^(off|private|public)$", Allowed: "off, private, public; not above default_limit"},
	{Key: SettingOrchestrationDefaultResources, Type: "object", Default: `{"cpu":"1","memory":"4Gi"}`, Allowed: "resource preset {cpu, memory}"},
	{Key: SettingOrchestrationDefaultVolumeGiB, Type: "integer", Default: "20", Min: ptr(int64(10)), Max: ptr(int64(1000)), Allowed: "10 to 1000"},
	{Key: SettingModelsServiceAccess, Type: "string", Default: `"all"`, Pattern: "^(all|selected)$", Allowed: "all, selected"},
	{Key: SettingModelsDefaultTeamAccess, Type: "string", Default: `"all"`, Pattern: "^(all|selected)$", Allowed: "all, selected"},
	{Key: SettingModelsDefaultTeamModels, Type: "array", Default: "[]", Pattern: "^[A-Za-z0-9][A-Za-z0-9._:/-]{0,199}$", Items: "string", Allowed: "enabled model ids"},
	{Key: SettingModelsDefaultModelID, Type: "string", Default: "null", Nullable: true, Pattern: "^[A-Za-z0-9][A-Za-z0-9._:/-]{0,199}$", Allowed: "null, or an enabled model"},
}

// AllSettingSpecs returns the generated registry followed by the orchestration and model settings.
func AllSettingSpecs() []SettingSpec {
	return slices.Concat(SettingSpecs, OrchestrationSettingSpecs)
}

// RenamedSettings maps the 1.0.1 setting keys that 1.1.0 renamed to their new keys. PutSetting refuses the old key
// with reason setting_renamed until 2.0.0; the database keeps the old rows in step for readers.
var RenamedSettings = map[string]string{
	SettingCollabBaseDomain:      SettingOrchestrationPublicBaseDomain,
	SettingCollabPublicEndpoints: SettingOrchestrationPublicEndpoints,
}
