package httpapi

import (
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

// scheduleBody is the wire shape of ReportSchedule (API contract §5, camelCase). lastStatus is
// "succeeded" or "failed"; the database stores "sent" for a success.
type scheduleBody struct {
	ID        string               `json:"id"`
	Version   int64                `json:"version"`
	CreatedAt string               `json:"createdAt"`
	CreatedBy string               `json:"createdBy"`
	UpdatedAt string               `json:"updatedAt"`
	Name      string               `json:"name"`
	Report    string               `json:"report"`
	Query     domain.ScheduleQuery `json:"query"`
	Cadence   string               `json:"cadence"`
	ChannelID string               `json:"channelId"`
	Format    string               `json:"format"`
	Enabled   bool                 `json:"enabled"`
	NextRunAt *string              `json:"nextRunAt"`
	LastRunAt *string              `json:"lastRunAt"`
	// LastStatus and LastError are null before the first run and after a success (error).
	LastStatus *string `json:"lastStatus"`
	LastError  *string `json:"lastError"`
}

func toScheduleBody(s domain.ReportSchedule) scheduleBody {
	b := scheduleBody{ID: s.ID, Version: int64(s.Version), CreatedAt: w2Time(s.CreatedAt), CreatedBy: s.CreatedBy, UpdatedAt: w2Time(s.UpdatedAt),
		Name: s.Name, Report: string(s.Report), Query: s.Query, Cadence: string(s.Cadence), ChannelID: s.ChannelID, Format: s.Format, Enabled: s.Enabled,
		LastRunAt: w2TimePtr(s.LastRunAt), LastError: optStr(s.LastError)}
	next := s.NextRunAt
	b.NextRunAt = w2TimePtr(&next)
	switch s.LastStatus {
	case domain.ScheduleSent:
		v := "succeeded"
		b.LastStatus = &v
	case domain.ScheduleFailed:
		v := "failed"
		b.LastStatus = &v
	}
	return b
}

// createScheduleRequest is the body of CreateReportSchedule.
type createScheduleRequest struct {
	Name      string               `json:"name"`
	Report    string               `json:"report"`
	Query     domain.ScheduleQuery `json:"query"`
	Cadence   string               `json:"cadence"`
	ChannelID string               `json:"channelId"`
	Format    string               `json:"format"`
	Enabled   *bool                `json:"enabled"`
}

func (r createScheduleRequest) params() domain.NewScheduleParams {
	return domain.NewScheduleParams{Name: r.Name, Report: domain.ReportKind(r.Report), Query: r.Query, Cadence: domain.Cadence(r.Cadence),
		ChannelID: r.ChannelID, Format: r.Format, Enabled: r.Enabled}
}

// updateScheduleRequest is the body of UpdateReportSchedule: absent members stay as they are.
type updateScheduleRequest struct {
	Name      *string               `json:"name"`
	Query     *domain.ScheduleQuery `json:"query"`
	Cadence   *string               `json:"cadence"`
	ChannelID *string               `json:"channelId"`
	Enabled   *bool                 `json:"enabled"`
}

func (r updateScheduleRequest) patch() domain.SchedulePatch {
	p := domain.SchedulePatch{Name: r.Name, Query: r.Query, ChannelID: r.ChannelID, Enabled: r.Enabled}
	if r.Cadence != nil {
		c := domain.Cadence(*r.Cadence)
		p.Cadence = &c
	}
	return p
}
