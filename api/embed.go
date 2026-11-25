// Package api embeds the bundled OpenAPI document (api/openapi.yaml), the REST contract of afs-api.
package api

import _ "embed"

// ContractVersion is the API contract version this build implements. Every response carries it in
// X-Afs-Contract-Version (D47); it equals info.version of the OpenAPI document.
const ContractVersion = "1.5.0"

// OpenAPI is the bundled OpenAPI 3.1 document served at GET /v1/openapi.yaml.
//
//go:embed openapi.yaml
var OpenAPI []byte
