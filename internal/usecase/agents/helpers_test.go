package agents_test

import (
	"errors"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

func asErr(err error, target **domain.Error) bool { return errors.As(err, target) }

func hasField(e *domain.Error, field string) bool {
	for _, f := range e.Fields {
		if f.Field == field {
			return true
		}
	}
	return false
}
