package main

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws/athena"
)

// settings are read from the environment (architecture §7). Everything the Helm chart supplies from
// Terraform outputs (AFS_CUR_*, AFS_ATHENA_*, AFS_REPORTS_*) is checked by the job that needs it, so a
// job does not fail for a setting only another job uses.
type settings struct {
	DatabaseURL string // AFS_DATABASE_URL: the afs_billing login
	DBIAMAuth   bool   // AFS_DB_IAM_AUTH

	CURDatabase     string // AFS_CUR_DATABASE
	CURTable        string // AFS_CUR_TABLE
	CURTagPrefix    string // AFS_CUR_TAG_PREFIX, default user_
	AthenaWorkgroup string // AFS_ATHENA_WORKGROUP
	AthenaOutput    string // AFS_ATHENA_OUTPUT

	// PricingRegion is the Price List API region (AFS_PRICING_REGION): the API is served from a few
	// regions only. Default us-east-1.
	PricingRegion string
	// PriceRegion is the region whose Bedrock prices are proposed (AFS_AWS_REGION, AWS_REGION as fallback,
	// default us-east-1).
	PriceRegion string
	// Lookback is how many days ending yesterday a CUR job covers (AFS_BILLING_LOOKBACK_DAYS, default 3).
	Lookback int
}

const defaultLookback = 3

func settingsFrom(env func(string) string) (settings, error) {
	s := settings{
		DatabaseURL: env("AFS_DATABASE_URL"), CURDatabase: env("AFS_CUR_DATABASE"), CURTable: env("AFS_CUR_TABLE"),
		CURTagPrefix: env("AFS_CUR_TAG_PREFIX"), AthenaWorkgroup: env("AFS_ATHENA_WORKGROUP"), AthenaOutput: env("AFS_ATHENA_OUTPUT"),
		PricingRegion: env("AFS_PRICING_REGION"), Lookback: defaultLookback,
	}
	if s.PriceRegion = env("AFS_AWS_REGION"); s.PriceRegion == "" {
		s.PriceRegion = env("AWS_REGION")
	}
	if s.PriceRegion == "" {
		s.PriceRegion = "us-east-1"
	}
	if s.PricingRegion == "" {
		s.PricingRegion = "us-east-1"
	}
	if v := env("AFS_DB_IAM_AUTH"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return s, fmt.Errorf("AFS_DB_IAM_AUTH: %q is not a boolean", v)
		}
		s.DBIAMAuth = b
	}
	if v := env("AFS_BILLING_LOOKBACK_DAYS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 31 {
			return s, fmt.Errorf("AFS_BILLING_LOOKBACK_DAYS: %q is not a number from 1 to 31", v)
		}
		s.Lookback = n
	}
	if s.DatabaseURL == "" {
		return s, errors.New("AFS_DATABASE_URL is not set")
	}
	return s, nil
}

// athenaConfig is the CUR source of the cur-ingest job.
func (s settings) athenaConfig() (athena.Config, error) {
	for _, c := range []struct{ name, v string }{{"AFS_CUR_DATABASE", s.CURDatabase}, {"AFS_CUR_TABLE", s.CURTable}, {"AFS_ATHENA_WORKGROUP", s.AthenaWorkgroup}} {
		if c.v == "" {
			return athena.Config{}, fmt.Errorf("%s is not set", c.name)
		}
	}
	return athena.Config{Database: s.CURDatabase, Table: s.CURTable, Workgroup: s.AthenaWorkgroup,
		OutputLocation: s.AthenaOutput, TagPrefix: s.CURTagPrefix}, nil
}
