package teams_test

import (
	"crypto/sha256"
	"errors"
	"strings"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

func sum(b []byte) []byte { s := sha256.Sum256(b); return s[:] }

func contains(s, sub string) bool { return strings.Contains(s, sub) }

func asDomain(err error, target **domain.Error) bool { return errors.As(err, target) }
