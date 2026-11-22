package domain

import (
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

// LogDestinationKind is where a team's logs are delivered (API contract §5).
type LogDestinationKind string

// Log destination kinds.
const (
	LogCloudWatch LogDestinationKind = "cloudwatch"
	LogS3         LogDestinationKind = "s3"
	LogOTLP       LogDestinationKind = "otlp"
	LogSplunkHEC  LogDestinationKind = "splunk_hec"
	LogDatadog    LogDestinationKind = "datadog"
)

// LogDestinationKinds lists every kind.
func LogDestinationKinds() []LogDestinationKind {
	return []LogDestinationKind{LogCloudWatch, LogS3, LogOTLP, LogSplunkHEC, LogDatadog}
}

// Valid reports whether k is a known kind.
func (k LogDestinationKind) Valid() bool {
	for _, v := range LogDestinationKinds() {
		if k == v {
			return true
		}
	}
	return false
}

// SecretPolicy says whether a kind takes a credential reference.
type SecretPolicy int

// Secret policies of a log destination kind.
const (
	// SecretForbidden: the kind authenticates with the team's IAM role (CloudWatch, S3).
	SecretForbidden SecretPolicy = iota
	// SecretOptional: the endpoint may need a credential (OTLP).
	SecretOptional
	// SecretRequired: the kind cannot deliver without one (Splunk HEC token, Datadog API key).
	SecretRequired
)

// logKindSpec is the non-secret configuration a kind accepts.
type logKindSpec struct {
	required []string
	optional []string
	secret   SecretPolicy
	check    func(key, val string) string // reason, "" when valid
}

var (
	regionRE      = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]$`)
	logGroupRE    = regexp.MustCompile(`^[A-Za-z0-9_./#-]{1,512}$`)
	bucketRE      = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	prefixRE      = regexp.MustCompile(`^[A-Za-z0-9!_.*'()/=-]{1,200}$`)
	plainRE       = regexp.MustCompile(`^[A-Za-z0-9_.:/ -]{1,200}$`)
	datadogSites  = map[string]bool{"datadoghq.com": true, "datadoghq.eu": true, "us3.datadoghq.com": true, "us5.datadoghq.com": true, "ap1.datadoghq.com": true, "ddog-gov.com": true}
	inlineCredRE  = regexp.MustCompile(`(?i)(token|secret|passw|apikey|credential|authoriz|bearer|privatekey|accesskey|signature|headers?$)`)
	logKindSpecs  = map[LogDestinationKind]logKindSpec{}
	maxConfigVals = 20
)

func init() {
	logKindSpecs[LogCloudWatch] = logKindSpec{required: []string{"logGroup", "region"}, optional: []string{"logStreamPrefix"}, secret: SecretForbidden,
		check: func(k, v string) string {
			switch k {
			case "logGroup":
				if !logGroupRE.MatchString(v) {
					return "must be a CloudWatch log group name"
				}
			case "region":
				if !regionRE.MatchString(v) {
					return "must be an AWS region such as us-east-1"
				}
			case "logStreamPrefix":
				if !plainRE.MatchString(v) {
					return "must be 1 to 200 characters of letters, digits and _.:/ -"
				}
			}
			return ""
		}}
	logKindSpecs[LogS3] = logKindSpec{required: []string{"bucket", "region"}, optional: []string{"prefix"}, secret: SecretForbidden,
		check: func(k, v string) string {
			switch k {
			case "bucket":
				if !bucketRE.MatchString(v) {
					return "must be an S3 bucket name"
				}
			case "region":
				if !regionRE.MatchString(v) {
					return "must be an AWS region such as us-east-1"
				}
			case "prefix":
				if !prefixRE.MatchString(v) || strings.HasPrefix(v, "/") {
					return "must be an object key prefix without a leading slash"
				}
			}
			return ""
		}}
	logKindSpecs[LogOTLP] = logKindSpec{required: []string{"endpoint"}, optional: []string{"protocol"}, secret: SecretOptional,
		check: func(k, v string) string {
			switch k {
			case "endpoint":
				return endpointReason(v)
			case "protocol":
				if v != "grpc" && v != "http" {
					return "must be grpc or http"
				}
			}
			return ""
		}}
	logKindSpecs[LogSplunkHEC] = logKindSpec{required: []string{"endpoint"}, optional: []string{"index", "source", "sourcetype"}, secret: SecretRequired,
		check: func(k, v string) string {
			if k == "endpoint" {
				return endpointReason(v)
			}
			if !plainRE.MatchString(v) {
				return "must be 1 to 200 characters of letters, digits and _.:/ -"
			}
			return ""
		}}
	logKindSpecs[LogDatadog] = logKindSpec{required: []string{"site"}, optional: []string{"service", "source"}, secret: SecretRequired,
		check: func(k, v string) string {
			if k == "site" {
				if !datadogSites[v] {
					return "must be a Datadog site such as datadoghq.com"
				}
				return ""
			}
			if !plainRE.MatchString(v) {
				return "must be 1 to 200 characters of letters, digits and _.:/ -"
			}
			return ""
		}}
}

// endpointReason checks an https endpoint with no credentials in it.
func endpointReason(v string) string {
	u, err := url.Parse(v)
	switch {
	case err != nil || u.Host == "" || len(v) > 500:
		return "must be an https URL"
	case u.Scheme != "https":
		return "must use https"
	case u.User != nil || u.RawQuery != "" || u.Fragment != "":
		return "must not carry credentials, a query or a fragment; reference a secret with secretId"
	}
	return ""
}

// InlineCredentialKey reports whether a JSON member name looks like a credential. A log destination
// never stores one inline: credentials are a secret reference (secretId), database contract §2.
func InlineCredentialKey(k string) bool {
	return inlineCredRE.MatchString(strings.NewReplacer("-", "", "_", "").Replace(k)) || strings.EqualFold(k, "value") || strings.EqualFold(k, "key")
}

// LogDestination is a team's log delivery target (API contract §5). Config holds non-secret settings
// only; SecretID references the credential. The table has no generation column, so Generation is the
// row version (every change is a spec change).
type LogDestination struct {
	ID         string
	TeamID     string
	Name       string
	Kind       LogDestinationKind
	Config     map[string]string
	SecretID   string
	Enabled    bool
	Version    Version
	Generation Generation
	CreatedAt  time.Time
	CreatedBy  string
	UpdatedAt  time.Time
	Status     Status
}

// NewLogDestinationParams is the input of CreateLogDestination.
type NewLogDestinationParams struct {
	Name     string
	Kind     LogDestinationKind
	Config   map[string]string
	SecretID string
	Enabled  bool
}

// Validate checks the name, the kind's configuration and the secret reference.
func (p NewLogDestinationParams) Validate() error {
	return validateLogDestination(p.Name, p.Kind, p.Config, p.SecretID)
}

// New builds the destination created by p.
func (p NewLogDestinationParams) New(id, teamID string, now time.Time) (LogDestination, error) {
	if err := p.Validate(); err != nil {
		return LogDestination{}, err
	}
	return LogDestination{ID: id, TeamID: teamID, Name: p.Name, Kind: p.Kind, Config: cloneConfig(p.Config), SecretID: p.SecretID,
		Enabled: p.Enabled, Version: 1, Generation: 1, CreatedAt: now.UTC(), UpdatedAt: now.UTC(), Status: PendingStatus(now)}, nil
}

// LogDestinationPatch is the input of UpdateLogDestination; nil means unchanged. The kind cannot
// change. Config replaces the whole configuration. SecretID may be cleared (ClearSecret).
type LogDestinationPatch struct {
	Name        *string
	Config      map[string]string
	SecretID    *string
	ClearSecret bool
	Enabled     *bool
}

// Apply returns d changed by the patch, validated as a whole; Generation is bumped by the caller's
// storage (it follows the version).
func (d LogDestination) Apply(p LogDestinationPatch, now time.Time) (LogDestination, error) {
	n := d
	n.Config = cloneConfig(d.Config)
	if p.Name != nil {
		n.Name = *p.Name
	}
	if p.Config != nil {
		n.Config = cloneConfig(p.Config)
	}
	switch {
	case p.ClearSecret:
		n.SecretID = ""
	case p.SecretID != nil:
		n.SecretID = *p.SecretID
	}
	if p.Enabled != nil {
		n.Enabled = *p.Enabled
	}
	if err := validateLogDestination(n.Name, n.Kind, n.Config, n.SecretID); err != nil {
		return LogDestination{}, err
	}
	n.UpdatedAt = now.UTC()
	return n, nil
}

func cloneConfig(c map[string]string) map[string]string {
	out := make(map[string]string, len(c))
	for k, v := range c {
		out[k] = v
	}
	return out
}

func validateLogDestination(name string, kind LogDestinationKind, cfg map[string]string, secretID string) error {
	var fe []FieldError
	bad := func(f, r string) { fe = append(fe, FieldError{Field: f, Reason: r}) }
	if n := len([]rune(name)); n < 1 || n > 100 {
		bad("name", "must be 1 to 100 characters")
	}
	if secretID != "" {
		if _, err := ParseID(PrefixSecret, secretID); err != nil {
			bad("secretId", "must be a secret id (sec_…)")
		}
	}
	spec, ok := logKindSpecs[kind]
	if !ok {
		bad("kind", "must be one of cloudwatch, s3, otlp, splunk_hec, datadog")
		return Validation(fe...)
	}
	if len(cfg) > maxConfigVals {
		bad("config", "has too many entries")
	}
	allowed := map[string]bool{}
	for _, k := range spec.required {
		allowed[k] = true
	}
	for _, k := range spec.optional {
		allowed[k] = true
	}
	keys := make([]string, 0, len(cfg))
	for k := range cfg {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		switch {
		case InlineCredentialKey(k):
			bad("config."+k, "credentials are never stored in a destination; put the value in a secret and pass secretId")
		case !allowed[k]:
			bad("config."+k, "is not a setting of a "+string(kind)+" destination")
		default:
			if r := spec.check(k, cfg[k]); r != "" {
				bad("config."+k, r)
			}
		}
	}
	for _, k := range spec.required {
		if _, ok := cfg[k]; !ok {
			bad("config."+k, "is required for a "+string(kind)+" destination")
		}
	}
	switch {
	case spec.secret == SecretForbidden && secretID != "":
		bad("secretId", string(kind)+" destinations use the team's IAM role and take no secret")
	case spec.secret == SecretRequired && secretID == "":
		bad("secretId", string(kind)+" destinations need a secret holding the credential")
	}
	if len(fe) > 0 {
		return Validation(fe...)
	}
	return nil
}
