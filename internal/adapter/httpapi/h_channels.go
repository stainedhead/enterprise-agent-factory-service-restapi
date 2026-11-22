package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/notify"
)

// ChannelsAPI serves the notification channel operations: the team's six (M3-G1, M3-G4) and the
// service-level four (M3-G2), RC-15.
type ChannelsAPI struct {
	Svc   *notify.Service
	Pages PageTokens
}

// BindChannels binds the channel handlers as the delivery area.
func BindChannels(a *ChannelsAPI) Registrar { return a.Register }

// Register binds the channel operations.
func (a *ChannelsAPI) Register(g *Registry) error {
	return w2Bind(g, map[string]http.HandlerFunc{
		"ListNotificationChannels": a.list(false), "CreateNotificationChannel": a.create(false), "GetNotificationChannel": a.get,
		"UpdateNotificationChannel": a.update(false), "DeleteNotificationChannel": a.del(false), "TestNotificationChannel": a.test,
		"ListServiceNotificationChannels": a.list(true), "CreateServiceNotificationChannel": a.create(true),
		"UpdateServiceNotificationChannel": a.update(true), "DeleteServiceNotificationChannel": a.del(true),
	})
}

// teamOf is the path's team, "" on the service routes (they have no teamId).
func teamOf(r *http.Request) string { return r.PathValue("teamId") }

func (a *ChannelsAPI) list(service bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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
		var rows []domain.NotificationChannel
		var next string
		if service {
			rows, next, err = a.Svc.ListService(r.Context(), c, page)
		} else {
			rows, next, err = a.Svc.ListTeam(r.Context(), c, teamOf(r), page)
		}
		if err != nil {
			WriteError(w, r, err)
			return
		}
		out := w2ListBody[channelBody]{Items: make([]channelBody, 0, len(rows)), NextToken: w2Next(a.Pages, r, c, next)}
		for _, ch := range rows {
			out.Items = append(out.Items, toChannelBody(ch))
		}
		WriteJSON(w, http.StatusOK, out)
	}
}

func (a *ChannelsAPI) get(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	ch, err := a.Svc.GetTeam(r.Context(), c, teamOf(r), r.PathValue("id"))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	w2Etag(w, ch.Version)
	WriteJSON(w, http.StatusOK, toChannelBody(ch))
}

func (a *ChannelsAPI) create(service bool) http.HandlerFunc {
	op := "CreateNotificationChannel"
	base := "/v1/teams/"
	if service {
		op = "CreateServiceNotificationChannel"
	}
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := w2Caller(r)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		teamID := teamOf(r)
		raw, err := w2Body(r)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		if err := refuseInlineChannelCredential(raw); err != nil {
			WriteError(w, r, err)
			return
		}
		var req createChannelRequest
		if err := Decode(r, &req); err != nil {
			WriteError(w, r, err)
			return
		}
		idem, err := w2Idem(r, op, teamID, raw, func(v any) (int, json.RawMessage, error) {
			return w2Render(http.StatusCreated, toChannelBody(v.(domain.NotificationChannel)))
		})
		if err != nil {
			WriteError(w, r, err)
			return
		}
		var res notify.Result
		if service {
			res, err = a.Svc.CreateService(r.Context(), c, req.params(), idem)
		} else {
			res, err = a.Svc.CreateTeam(r.Context(), c, teamID, req.params(), idem)
		}
		if err != nil {
			WriteError(w, r, err)
			return
		}
		if res.Replay != nil {
			w2Replay(w, res.Replay)
			return
		}
		w2Etag(w, res.Channel.Version)
		if service {
			w.Header().Set("Location", "/v1/admin/notification-channels/"+res.Channel.ID)
		} else {
			w.Header().Set("Location", base+teamID+"/notification-channels/"+res.Channel.ID)
		}
		WriteJSON(w, http.StatusCreated, toChannelBody(res.Channel))
	}
}

func (a *ChannelsAPI) update(service bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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
		raw, err := w2Body(r)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		if err := refuseInlineChannelCredential(raw); err != nil {
			WriteError(w, r, err)
			return
		}
		var req updateChannelRequest
		if err := Decode(r, &req); err != nil {
			WriteError(w, r, err)
			return
		}
		var ch domain.NotificationChannel
		if service {
			ch, err = a.Svc.UpdateService(r.Context(), c, r.PathValue("id"), req.patch(), expect)
		} else {
			ch, err = a.Svc.UpdateTeam(r.Context(), c, teamOf(r), r.PathValue("id"), req.patch(), expect)
		}
		if err != nil {
			WriteError(w, r, err)
			return
		}
		w2Etag(w, ch.Version)
		WriteJSON(w, http.StatusOK, toChannelBody(ch))
	}
}

func (a *ChannelsAPI) del(service bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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
		var ch domain.NotificationChannel
		if service {
			ch, err = a.Svc.DeleteService(r.Context(), c, r.PathValue("id"), expect)
		} else {
			ch, err = a.Svc.DeleteTeam(r.Context(), c, teamOf(r), r.PathValue("id"), expect)
		}
		if err != nil {
			WriteError(w, r, err)
			return
		}
		w2Etag(w, ch.Version)
		WriteJSON(w, http.StatusAccepted, toChannelBody(ch))
	}
}

// test serves TestNotificationChannel: 200 with the outcome, or 202 with pending (OQ-S1, CD-3).
func (a *ChannelsAPI) test(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	teamID := teamOf(r)
	raw, err := w2Body(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	idem, err := w2Idem(r, "TestNotificationChannel:"+r.PathValue("id"), teamID, raw, func(v any) (int, json.RawMessage, error) {
		return w2Render(http.StatusAccepted, toTestResultBody(v.(domain.TestResult)))
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	out, err := a.Svc.TestChannel(r.Context(), c, teamID, r.PathValue("id"), idem)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if out.Replay != nil {
		w2Replay(w, out.Replay)
		return
	}
	status := http.StatusOK
	if out.Result.Pending {
		status = http.StatusAccepted
	}
	WriteJSON(w, status, toTestResultBody(out.Result))
}
