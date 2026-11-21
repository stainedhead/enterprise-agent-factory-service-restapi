package postgres

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// setSessionSQL applies database contract §3 with set_config(..., true), which is SET LOCAL with
// bind parameters.
const setSessionSQL = `SELECT set_config('afs.actor', $1, true),
       set_config('afs.service_admin', $2, true),
       set_config('afs.team_ids', $3, true),
       set_config('afs.security_operator', $4, true)`

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// sessionArgs validates s and returns the set_config arguments.
func sessionArgs(s port.Session) ([]any, error) {
	if s.Actor == "" {
		return nil, fmt.Errorf("postgres: session without an actor")
	}
	for _, id := range s.TeamIDs {
		if id == "" || strings.ContainsAny(id, ", ") {
			return nil, fmt.Errorf("postgres: invalid team id %q in session", id)
		}
	}
	return []any{s.Actor, onOff(s.ServiceAdmin), strings.Join(s.TeamIDs, ","), onOff(s.SecurityOperator)}, nil
}

func applySession(ctx context.Context, tx pgx.Tx, s port.Session) error {
	args, err := sessionArgs(s)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, setSessionSQL, args...)
	return err
}
