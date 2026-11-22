package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reportschedules"
)

// SchedulesAPI serves the report schedule operations (M4-E2): ListReportSchedules,
// CreateReportSchedule, UpdateReportSchedule and DeleteReportSchedule.
type SchedulesAPI struct {
	Svc   *reportschedules.Service
	Pages PageTokens
}

// schedulesArea names the handler area of the schedule operations.
const schedulesArea = "reportschedules"

// BindSchedules binds the schedule handlers as their own area.
func BindSchedules(a *SchedulesAPI) {
	for i := range Areas {
		if Areas[i].Name == schedulesArea {
			Areas[i].Register = a.Register
			return
		}
	}
	Areas = append(Areas, Area{Name: schedulesArea, Register: a.Register})
}

// Register binds the schedule operations.
func (a *SchedulesAPI) Register(g *Registry) error {
	return w2Bind(g, map[string]http.HandlerFunc{
		"ListReportSchedules": a.list, "CreateReportSchedule": a.create, "UpdateReportSchedule": a.update, "DeleteReportSchedule": a.del,
	})
}

func (a *SchedulesAPI) list(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	page, err := w2Page(a.Pages, r, c)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	rows, next, err := a.Svc.List(r.Context(), c, r.PathValue("teamId"), page)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	out := w2ListBody[scheduleBody]{Items: make([]scheduleBody, 0, len(rows)), NextToken: w2Next(a.Pages, r, c, next)}
	for _, s := range rows {
		out.Items = append(out.Items, toScheduleBody(s))
	}
	WriteJSON(w, http.StatusOK, out)
}

func (a *SchedulesAPI) create(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	teamID := r.PathValue("teamId")
	raw, err := w2Body(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var req createScheduleRequest
	if err := Decode(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	idem, err := w2Idem(r, "CreateReportSchedule", teamID, raw, func(v any) (int, json.RawMessage, error) {
		return w2Render(http.StatusCreated, toScheduleBody(v.(domain.ReportSchedule)))
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	res, err := a.Svc.Create(r.Context(), c, teamID, req.params(), idem)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if res.Replay != nil {
		w2Replay(w, res.Replay)
		return
	}
	w2Etag(w, res.Schedule.Version)
	w.Header().Set("Location", "/v1/teams/"+teamID+"/report-schedules/"+res.Schedule.ID)
	WriteJSON(w, http.StatusCreated, toScheduleBody(res.Schedule))
}

func (a *SchedulesAPI) update(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	expect, err := w2IfMatch(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var req updateScheduleRequest
	if err := Decode(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	s, err := a.Svc.Update(r.Context(), c, r.PathValue("teamId"), r.PathValue("scheduleId"), req.patch(), expect)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	w2Etag(w, s.Version)
	WriteJSON(w, http.StatusOK, toScheduleBody(s))
}

func (a *SchedulesAPI) del(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	expect, err := w2IfMatch(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if err := a.Svc.Delete(r.Context(), c, r.PathValue("teamId"), r.PathValue("scheduleId"), expect); err != nil {
		WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
