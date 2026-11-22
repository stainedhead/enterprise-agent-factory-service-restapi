package main

import "context"

// The M4-E report jobs join the registry here (the registry of jobs.go is the one mechanism), so the
// wiring of wire_e_reports.go needs no flag of its own.
//
//	report-schedules  every 15 minutes (Helm cronjobs.report-schedules): runs each due report schedule
//	showback          daily (Helm cronjobs.showback): previous month and month to date, replacing the files
func init() {
	register("report-schedules", reportSchedulesJob)
	register("showback", showbackJob)
}

func reportSchedulesJob(ctx context.Context, a *app, _ jobInput) error {
	j, err := newReportJobs(ctx, a.pool, a.env, a.log)
	if err != nil {
		return err
	}
	return j.RunSchedules(ctx)
}

func showbackJob(ctx context.Context, a *app, _ jobInput) error {
	j, err := newReportJobs(ctx, a.pool, a.env, a.log)
	if err != nil {
		return err
	}
	return j.RunShowback(ctx)
}
