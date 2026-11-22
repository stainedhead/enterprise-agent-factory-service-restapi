package render

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
)

// Paths and audiences of the model-call and API delivery (gateway contract §3, API contract §2).
const (
	// GatewayDir is where the gateway CA bundle and the optional token are mounted.
	GatewayDir = "/var/run/secrets/afs/gateway"
	// GatewayCAPath is the CA bundle every SDK is told to trust.
	GatewayCAPath = GatewayDir + "/ca.crt"
	// GatewayTokenPath is the optional projected token (AFS_GATEWAY_TOKEN_FILE).
	GatewayTokenPath = GatewayDir + "/token"
	// GatewayTokenAudience is the audience of the gateway token.
	GatewayTokenAudience = "afs-gateway"
	// APIDir holds the afs-api token of agents in teams with an assigned work order (D29).
	APIDir = "/var/run/secrets/afs/api"
	// APITokenPath is the projected afs-api token.
	APITokenPath = APIDir + "/token"
	// APITokenAudience is the audience afs-api validates.
	APITokenAudience = "afs-api"
	// TokenLifetimeSeconds is the lifetime of both tokens; the kubelet rotates them.
	TokenLifetimeSeconds int64 = 3600
	// PlaceholderAPIKey is not a secret: it lets SDKs that insist on a key start (D57).
	PlaceholderAPIKey = "afs-gateway"

	volumeGateway = "afs-gateway"
	volumeAPI     = "afs-api"
)

// GatewayEnv returns the environment of the model-call path, the same for every runtime. There are no AWS
// keys in it: AWS SDKs find the pod's Pod Identity credentials, which carry no Bedrock permission, and
// the gateway ignores the signature (gateway contract §3).
func GatewayEnv(gatewayURL string) []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: "AFS_GATEWAY_URL", Value: gatewayURL},
		{Name: "AWS_ENDPOINT_URL_BEDROCK_RUNTIME", Value: gatewayURL},
		{Name: "ANTHROPIC_BASE_URL", Value: gatewayURL + "/anthropic"},
		{Name: "ANTHROPIC_API_KEY", Value: PlaceholderAPIKey},
		{Name: "AFS_GATEWAY_TOKEN_FILE", Value: GatewayTokenPath},
		{Name: "AWS_CA_BUNDLE", Value: GatewayCAPath},
		{Name: "SSL_CERT_FILE", Value: GatewayCAPath},
		{Name: "REQUESTS_CA_BUNDLE", Value: GatewayCAPath},
		{Name: "NODE_EXTRA_CA_CERTS", Value: GatewayCAPath},
	}
}

// gatewayVolume projects the gateway CA bundle (copied into the namespace by the team controller) and the
// optional gateway token into one directory.
func gatewayVolume() (corev1.Volume, corev1.VolumeMount) {
	return corev1.Volume{Name: volumeGateway, VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
			DefaultMode: ptr.To[int32](0o444),
			Sources: []corev1.VolumeProjection{
				{ConfigMap: &corev1.ConfigMapProjection{
					LocalObjectReference: corev1.LocalObjectReference{Name: GatewayCAConfigMap},
					Items:                []corev1.KeyToPath{{Key: GatewayCAKey, Path: "ca.crt"}},
				}},
				{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Audience: GatewayTokenAudience, ExpirationSeconds: ptr.To(TokenLifetimeSeconds), Path: "token"}},
			},
		}}},
		corev1.VolumeMount{Name: volumeGateway, MountPath: GatewayDir, ReadOnly: true}
}

// apiVolume projects the afs-api token. It exists only in teams with an assigned work order (D29).
func apiVolume() (corev1.Volume, corev1.VolumeMount) {
	return corev1.Volume{Name: volumeAPI, VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
			DefaultMode: ptr.To[int32](0o444),
			Sources: []corev1.VolumeProjection{
				{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Audience: APITokenAudience, ExpirationSeconds: ptr.To(TokenLifetimeSeconds), Path: "token"}},
			},
		}}},
		corev1.VolumeMount{Name: volumeAPI, MountPath: APIDir, ReadOnly: true}
}
