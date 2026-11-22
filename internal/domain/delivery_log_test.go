package domain

import (
	"strings"
	"testing"
	"time"
)

const logSecretID = "sec_01JD2Q3R4S5T6V7W8X9Y0Z0001"

func logFields(t *testing.T, err error) []string {
	t.Helper()
	e, ok := err.(*Error) //nolint:errorlint // the domain returns *Error directly
	if !ok || e == nil {
		t.Fatalf("want a ValidationError, got %v", err)
	}
	var out []string
	for _, f := range e.Fields {
		out = append(out, f.Field)
	}
	return out
}

func TestLogDestinationValidate(t *testing.T) {
	cfg := func(kv ...string) map[string]string {
		m := map[string]string{}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	tests := []struct {
		name   string
		in     NewLogDestinationParams
		fields []string // empty: valid
	}{
		{"cloudwatch", NewLogDestinationParams{Name: "cw", Kind: LogCloudWatch, Config: cfg("logGroup", "/afs/payments", "region", "us-east-1")}, nil},
		{"cloudwatch missing region", NewLogDestinationParams{Name: "cw", Kind: LogCloudWatch, Config: cfg("logGroup", "/afs/payments")}, []string{"config.region"}},
		{"cloudwatch with secret", NewLogDestinationParams{Name: "cw", Kind: LogCloudWatch, Config: cfg("logGroup", "g", "region", "us-east-1"), SecretID: logSecretID}, []string{"secretId"}},
		{"s3", NewLogDestinationParams{Name: "s", Kind: LogS3, Config: cfg("bucket", "team-logs", "region", "eu-west-1", "prefix", "afs/")}, nil},
		{"s3 leading slash", NewLogDestinationParams{Name: "s", Kind: LogS3, Config: cfg("bucket", "team-logs", "region", "eu-west-1", "prefix", "/afs")}, []string{"config.prefix"}},
		{"otlp no secret", NewLogDestinationParams{Name: "o", Kind: LogOTLP, Config: cfg("endpoint", "https://otel.example.com:4318")}, nil},
		{"otlp with secret", NewLogDestinationParams{Name: "o", Kind: LogOTLP, Config: cfg("endpoint", "https://otel.example.com:4318", "protocol", "http"), SecretID: logSecretID}, nil},
		{"otlp http endpoint", NewLogDestinationParams{Name: "o", Kind: LogOTLP, Config: cfg("endpoint", "http://otel.example.com")}, []string{"config.endpoint"}},
		{"otlp endpoint userinfo", NewLogDestinationParams{Name: "o", Kind: LogOTLP, Config: cfg("endpoint", "https://u:p@otel.example.com")}, []string{"config.endpoint"}},
		{"otlp endpoint query token", NewLogDestinationParams{Name: "o", Kind: LogOTLP, Config: cfg("endpoint", "https://otel.example.com/?token=abc")}, []string{"config.endpoint"}},
		{"splunk", NewLogDestinationParams{Name: "sp", Kind: LogSplunkHEC, Config: cfg("endpoint", "https://splunk.example.com:8088", "index", "afs"), SecretID: logSecretID}, nil},
		{"splunk without secret", NewLogDestinationParams{Name: "sp", Kind: LogSplunkHEC, Config: cfg("endpoint", "https://splunk.example.com:8088")}, []string{"secretId"}},
		{"datadog", NewLogDestinationParams{Name: "dd", Kind: LogDatadog, Config: cfg("site", "datadoghq.com"), SecretID: logSecretID}, nil},
		{"datadog bad site", NewLogDestinationParams{Name: "dd", Kind: LogDatadog, Config: cfg("site", "evil.example.com"), SecretID: logSecretID}, []string{"config.site"}},
		{"inline token", NewLogDestinationParams{Name: "sp", Kind: LogSplunkHEC, Config: cfg("endpoint", "https://splunk.example.com", "hecToken", "abc"), SecretID: logSecretID}, []string{"config.hecToken"}},
		{"inline api key", NewLogDestinationParams{Name: "dd", Kind: LogDatadog, Config: cfg("site", "datadoghq.com", "api_key", "abc"), SecretID: logSecretID}, []string{"config.api_key"}},
		{"inline header", NewLogDestinationParams{Name: "o", Kind: LogOTLP, Config: cfg("endpoint", "https://otel.example.com", "headers", "x")}, []string{"config.headers"}},
		{"unknown key", NewLogDestinationParams{Name: "o", Kind: LogOTLP, Config: cfg("endpoint", "https://otel.example.com", "extra", "x")}, []string{"config.extra"}},
		{"bad secret id", NewLogDestinationParams{Name: "o", Kind: LogOTLP, Config: cfg("endpoint", "https://otel.example.com"), SecretID: "agt_01JD2Q3R4S5T6V7W8X9Y0Z0001"}, []string{"secretId"}},
		{"unknown kind", NewLogDestinationParams{Name: "o", Kind: "syslog", Config: cfg()}, []string{"kind"}},
		{"empty name", NewLogDestinationParams{Name: "", Kind: LogOTLP, Config: cfg("endpoint", "https://otel.example.com")}, []string{"name"}},
		{"long name", NewLogDestinationParams{Name: strings.Repeat("n", 101), Kind: LogOTLP, Config: cfg("endpoint", "https://otel.example.com")}, []string{"name"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.in.Validate()
			if len(tc.fields) == 0 {
				if err != nil {
					t.Fatalf("want valid, got %v", err)
				}
				return
			}
			got := strings.Join(logFields(t, err), ",")
			if got != strings.Join(tc.fields, ",") {
				t.Fatalf("fields = %s, want %v", got, tc.fields)
			}
		})
	}
}

func TestLogDestinationNewAndApply(t *testing.T) {
	now := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	d, err := NewLogDestinationParams{Name: "cw", Kind: LogCloudWatch, Enabled: true,
		Config: map[string]string{"logGroup": "g", "region": "us-east-1"}}.New("log_X", "team_X", now)
	if err != nil || d.Version != 1 || d.Generation != 1 || d.Status.Phase != PhasePending || !d.Enabled {
		t.Fatalf("New = %+v, %v", d, err)
	}
	off := false
	n, err := d.Apply(LogDestinationPatch{Enabled: &off, Config: map[string]string{"logGroup": "h", "region": "us-west-2"}}, now.Add(time.Hour))
	if err != nil || n.Enabled || n.Config["logGroup"] != "h" || !n.UpdatedAt.After(now) {
		t.Fatalf("Apply = %+v, %v", n, err)
	}
	if d.Config["logGroup"] != "g" {
		t.Fatal("Apply must not change the receiver's config")
	}
	// A patch is validated as a whole: a secret on CloudWatch is refused.
	s := logSecretID
	if _, err := d.Apply(LogDestinationPatch{SecretID: &s}, now); err == nil {
		t.Fatal("secret on a cloudwatch destination accepted")
	}
	// Clearing the secret of a kind that requires one is refused.
	dd, _ := NewLogDestinationParams{Name: "dd", Kind: LogDatadog, Config: map[string]string{"site": "datadoghq.com"}, SecretID: logSecretID}.New("log_Y", "team_X", now)
	if _, err := dd.Apply(LogDestinationPatch{ClearSecret: true}, now); err == nil {
		t.Fatal("clearing the datadog secret accepted")
	}
}

func TestInlineCredentialKey(t *testing.T) {
	for _, k := range []string{"token", "hecToken", "password", "api_key", "apiKey", "secret", "authorization", "Bearer", "headers", "value", "key", "private-key", "accessKeyId"} {
		if !InlineCredentialKey(k) {
			t.Errorf("%q not recognised as a credential", k)
		}
	}
	for _, k := range []string{"endpoint", "logGroup", "region", "bucket", "site", "index"} {
		if InlineCredentialKey(k) {
			t.Errorf("%q wrongly flagged", k)
		}
	}
}
