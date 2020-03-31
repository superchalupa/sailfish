package telemetry

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
	eh "github.com/looplab/eventhorizon"
	"github.com/spf13/viper"
	"golang.org/x/xerrors"

	"github.com/superchalupa/sailfish/cmd/metric-engine/metric"
	log "github.com/superchalupa/sailfish/src/log"
)

// "configuration" -- TODO: need to move to config file
const (
	clockPeriod             = 1000 * time.Millisecond
	maxMetricTimestampDelta = 1 * time.Hour

	// NOTE: the numbers below are selected as PRIME numbers so that they run concurrently as infrequently as possible
	// With the default 73/3607/10831, they will run concurrently every ~90 years.
	cleanMVTime  = 307 * time.Second
	vacuumTime   = 3607 * time.Second
	optimizeTime = 10831 * time.Second
)

type busComponents interface {
	GetBus() eh.EventBus
}

type eventHandler interface {
	AddEventHandler(string, eh.EventType, func(eh.Event))
	AddMultiHandler(string, eh.EventType, func(eh.Event))
}

// publishHelper will log/eat the error from PublishEvent since we can't do anything useful with it
func publishHelper(logger log.Logger, bus eh.EventBus, event eh.Event) {
	err := bus.PublishEvent(context.Background(), event)
	if err != nil {
		logger.Crit("Error publishing event. This should never happen!", "err", err)
	}
}

func backgroundTasks(logger log.Logger, bus eh.EventBus) {
	clockTicker := time.NewTicker(clockPeriod)
	cleanValuesTicker := time.NewTicker(cleanMVTime)
	vacuumTicker := time.NewTicker(vacuumTime)
	optimizeTicker := time.NewTicker(optimizeTime)

	defer cleanValuesTicker.Stop()
	defer vacuumTicker.Stop()
	defer optimizeTicker.Stop()
	defer clockTicker.Stop()
	for {
		select {
		case <-cleanValuesTicker.C:
			publishHelper(logger, bus, eh.NewEvent(DatabaseMaintenance, "clean values", time.Now()))
		case <-vacuumTicker.C:
			publishHelper(logger, bus, eh.NewEvent(DatabaseMaintenance, "vacuum", time.Now()))
		case <-optimizeTicker.C:
			publishHelper(logger, bus, eh.NewEvent(DatabaseMaintenance, "optimize", time.Now()))
			publishHelper(logger, bus, eh.NewEvent(DatabaseMaintenance, "delete orphans", time.Now())) // belt and suspenders
		case <-clockTicker.C:
			publishHelper(logger, bus, eh.NewEvent(PublishClock, nil, time.Now()))
		}
	}
}

// StartupTelemetryBase registers event handlers with the awesome mapper and
// starts up timers and maintenance goroutines
func StartupTelemetryBase(logger log.Logger, cfg *viper.Viper, am3Svc eventHandler, d busComponents) error {
	RegisterEvents()

	cfg.SetDefault("main.databasepath",
		"file:/run/telemetryservice/telemetry_timeseries_database.db?_foreign_keys=on&cache=shared&mode=rwc&_busy_timeout=1000")

	database, err := sqlx.Open("sqlite3", cfg.GetString("main.databasepath"))
	if err != nil {
		return xerrors.Errorf("could not open database(%s): %w", cfg.GetString("main.databasepath"))
	}

	// If we run in WAL mode, you can only do one connection. Seems like a base
	// library limitation that's reflected up into the golang implementation.
	// SO: we will ensure that we have ONLY ONE GOROUTINE that does transactions.
	// This isn't a terrible limitation as it is sort of what we want to do
	// anyways.
	database.SetMaxOpenConns(1)

	// Create tables and views from sql stored in our YAML
	for _, sqltext := range cfg.GetStringSlice("createdb") {
		_, err = database.Exec(sqltext)
		if err != nil {
			// ignore drop errors. can happen if we have old telemetry db and are ok
			if strings.HasPrefix(err.Error(), "use DROP") {
				logger.Info("Ignoring SQL error dropping table/view", "err", err, "sql", sqltext)
				continue
			}
			return xerrors.Errorf("Error running DB Create statement. SQL: %s: ERROR: %w", sqltext, err)
		}
	}

	telemetryMgr, err := newTelemetryManager(logger, database, cfg)
	if err != nil {
		return xerrors.Errorf("telemetry manager initialization failed: %w", err)
	}

	// hint to runtime that we dont need cfg after this point. dont pass this into functions below here
	cfg = nil

	bus := d.GetBus()
	// converted to command request/response
	am3Svc.AddEventHandler("Create Metric Report Definition", AddMetricReportDefinition, MakeHandlerCreateMRD(logger, telemetryMgr, bus))
	am3Svc.AddEventHandler("Delete Metric Report Definition", DeleteMetricReportDefinition, MakeHandlerDeleteMRD(logger, telemetryMgr, bus))
	am3Svc.AddEventHandler("Delete Metric Report", DeleteMetricReport, MakeHandlerDeleteMR(logger, telemetryMgr, bus))
	am3Svc.AddEventHandler("Update Metric Report Definition", UpdateMetricReportDefinition, MakeHandlerUpdateMRD(logger, telemetryMgr, bus))

	//Metric Defintion Add event
	am3Svc.AddEventHandler("Create Metric Definition", AddMetricDefinition, MakeHandlerCreateMD(logger, telemetryMgr, bus))

	// just events for now
	am3Svc.AddEventHandler("Generate Metric Report", metric.RequestReport, MakeHandlerGenReport(logger, telemetryMgr, bus))
	am3Svc.AddEventHandler("Clock", PublishClock, MakeHandlerClock(logger, telemetryMgr, bus))
	am3Svc.AddEventHandler("Database Maintenance", DatabaseMaintenance, MakeHandlerMaintenance(logger, telemetryMgr, bus))

	// multi handler
	am3Svc.AddMultiHandler("Store Metric Value(s)", metric.MetricValueEvent, MakeHandlerMV(logger, telemetryMgr, bus))

	// database cleanup on start
	telemetryMgr.DeleteOrphans()      //nolint:errcheck
	telemetryMgr.DeleteOldestValues() //nolint:errcheck
	telemetryMgr.Optimize()           //nolint:errcheck
	telemetryMgr.Vacuum()             //nolint:errcheck

	// start background thread publishing regular maintenance tasks
	go backgroundTasks(logger, bus)

	return nil
}

func MakeHandlerCreateMRD(logger log.Logger, telemetryMgr *telemetryManager, bus eh.EventBus) func(eh.Event) {
	return func(event eh.Event) {
		reportDef, ok := event.Data().(*AddMetricReportDefinitionData)
		if !ok {
			logger.Crit("AddMetricReportDefinition handler got event of incorrect format")
			return
		}

		// Can't write to event sent in, so make a local copy
		localReportDefCopy := *reportDef
		addError := telemetryMgr.addMRD(&localReportDefCopy.MetricReportDefinitionData)
		if addError != nil {
			logger.Crit("Failed to create or update the Report Definition", "Name", reportDef.Name, "err", addError)
		}

		// After we've done the adjustments to ReportDefinitionToMetricMeta, there
		// might be orphan rows. Errors there dont need to be reported back as part of the command response
		err := telemetryMgr.DeleteOrphans()
		if err != nil {
			logger.Crit("Orphan delete failed", "err", err)
		}

		// Generate a "response" event that carries status back to initiator
		respEvent, err := reportDef.NewResponseEvent(addError)
		if err != nil {
			logger.Crit("Error creating response event", "err", err, "ReportDefintion", reportDef.Name)
			return
		}

		//data, ok := respEvent.Data().(*AddMetricReportDefinitionResponseData)
		// Should add the populated metric report definition event as a response?
		publishHelper(logger, bus, respEvent)
	}
}

func MakeHandlerUpdateMRD(logger log.Logger, telemetryMgr *telemetryManager, bus eh.EventBus) func(eh.Event) {
	return func(event eh.Event) {
		update, ok := event.Data().(*UpdateMetricReportDefinitionData)
		if !ok {
			return
		}

		// make a local by-value copy of the pointer passed in
		localUpdate := *update
		updError := telemetryMgr.updateMRD(localUpdate.ReportDefinitionName, localUpdate.Patch)
		if updError != nil {
			logger.Crit("Failed to create or update the Report Definition", "Name", update.ReportDefinitionName, "err", updError)
			return
		}

		// After we've done the adjustments to ReportDefinitionToMetricMeta, there
		// might be orphan rows.
		err := telemetryMgr.DeleteOrphans()
		if err != nil {
			logger.Crit("Orphan delete failed", "err", err)
		}

		// Generate a "response" event that carries status back to initiator
		respEvent, err := update.NewResponseEvent(updError)
		if err != nil {
			logger.Crit("Error creating response event", "err", err, "ReportDefintion", update.ReportDefinitionName)
			return
		}

		//data, ok := respEvent.Data().(*AddMetricReportDefinitionResponseData)
		// Should add the populated metric report definition event as a response?
		publishHelper(logger, bus, respEvent)
	}
}

func MakeHandlerDeleteMRD(logger log.Logger, telemetryMgr *telemetryManager, bus eh.EventBus) func(eh.Event) {
	return func(event eh.Event) {
		reportDef, ok := event.Data().(*DeleteMetricReportDefinitionData)
		if !ok {
			return
		}

		// Handle the requested command
		delError := telemetryMgr.deleteMRD(reportDef.Name)
		if delError != nil {
			logger.Crit("Error deleting Metric Report Definition", "Name", reportDef.Name, "err", delError)
		}
		err := telemetryMgr.DeleteOrphans()
		if err != nil {
			logger.Crit("Orphan delete failed", "err", err)
		}

		// Generate a "response" event that carries status back to initiator
		respEvent, err := reportDef.NewResponseEvent(delError)
		if err != nil {
			logger.Crit("Error creating response event", "err", err, "ReportDefintion", reportDef.Name)
			return
		}

		publishHelper(logger, bus, respEvent)
	}
}

// MD event handlers
func MakeHandlerCreateMD(logger log.Logger, telemetryMgr *telemetryManager, bus eh.EventBus) func(eh.Event) {
	return func(event eh.Event) {
		fmt.Println("MakeHandlerCreateMD")
		mdDef, ok := event.Data().(*AddMetricDefinitionData)
		if !ok {
			fmt.Println("AddMetricDefinition handler got event of incorrect format")
			logger.Crit("AddMetricDefinition handler got event of incorrect format")
			return
		}

		// Can't write to event sent in, so make a local copy
		locaMdDefCopy := *mdDef
		addError := telemetryMgr.addMD(&locaMdDefCopy.MetricDefinitionData)
		if addError != nil {
			logger.Crit("Failed to create or update the Metric Definition", "MetricId", mdDef.MetricDefinitionData.MetricId, "err", addError)
		}

		// Generate a "response" event that carries status back to initiator
		respEvent, err := mdDef.NewResponseEvent(addError)
		if err != nil {
			logger.Crit("Error creating response event", "err", err, "MetricDefintion", mdDef.MetricDefinitionData.MetricId)
			return
		}

		//data, ok := respEvent.Data().(*AddMetricReportDefinitionResponseData)
		// Should add the populated metric report definition event as a response?
		publishHelper(logger, bus, respEvent)
	}
}

func MakeHandlerDeleteMR(logger log.Logger, telemetryMgr *telemetryManager, bus eh.EventBus) func(eh.Event) {
	return func(event eh.Event) {
		report, ok := event.Data().(*DeleteMetricReportData)
		if !ok {
			return
		}

		// Handle the requested command
		delError := telemetryMgr.deleteMR(report.Name)
		if delError != nil {
			logger.Crit("Error deleting Metric Report", "Name", report.Name, "err", delError)
		}

		// Generate a "response" event that carries status back to initiator
		respEvent, err := report.NewResponseEvent(delError)
		if err != nil {
			logger.Crit("Error creating response event", "err", err, "Report", report.Name)
			return
		}

		publishHelper(logger, bus, respEvent)
	}
}

func MakeHandlerGenReport(logger log.Logger, telemetryMgr *telemetryManager, bus eh.EventBus) func(eh.Event) {
	return func(event eh.Event) {
		report, ok := event.Data().(*metric.RequestReportData)
		if !ok {
			return
		}

		// input event is a pointer to shared data struct, dont directly use, make a copy
		name := report.Name
		reportError := telemetryMgr.GenerateMetricReport(nil, name)
		if reportError != nil {
			logger.Crit("Error generating metric report", "err", reportError, "ReportDefintion", name)
			// dont return, because we are going to return the error to the caller
		}

		respEvent, err := report.NewResponseEvent(reportError)
		if err != nil {
			logger.Crit("Error creating response event", "err", err, "ReportDefintion", name)
			return
		}

		data, ok := respEvent.Data().(*metric.ReportGeneratedData)
		if ok {
			data.Name = name
			publishHelper(logger, bus, respEvent)
		}
	}
}

func MakeHandlerMV(logger log.Logger, telemetryMgr *telemetryManager, bus eh.EventBus) func(eh.Event) {
	return func(event eh.Event) {
		// This is a MULTI Handler! This function is called with an ARRAY of event
		// data, not the normal single event data.  This means we can wrap the
		// insert in a transaction and insert everything in the array in a single
		// transaction for a good performance boost.
		instancesUpdated := map[int64]struct{}{}
		err := telemetryMgr.wrapWithTX(func(tx *sqlx.Tx) error {
			dataArray, ok := event.Data().([]eh.EventData)
			if !ok {
				return nil
			}
			for _, eventData := range dataArray {
				metricValue, ok := eventData.(*metric.MetricValueEventData)
				if !ok {
					continue
				}

				err := telemetryMgr.InsertMetricValue(tx, metricValue, func(instanceid int64) { instancesUpdated[instanceid] = struct{}{} })
				if err != nil {
					logger.Crit("Error Inserting Metric Value", "Metric", metricValue, "err", err)
					continue
				}

				delta := telemetryMgr.MetricTSHWM.Sub(metricValue.Timestamp.Time)

				if (!telemetryMgr.MetricTSHWM.IsZero()) && (delta > maxMetricTimestampDelta || delta < -maxMetricTimestampDelta) {
					// if you see this warning consistently, check the import to ensure it's using UTC and not localtime
					fmt.Printf("Warning: Metric Value Event TIME OFF >1hr - (delta: %s)  Metric: %+v\n", delta, metricValue)
				}

				if telemetryMgr.MetricTSHWM.Before(metricValue.Timestamp.Time) {
					telemetryMgr.MetricTSHWM = metricValue.Timestamp.Time
				}
			}
			return nil
		})
		if err != nil {
			logger.Crit("critical error storing metric value", "err", err)
		}

		// this will set telemetryMgr.NextMRTS = telemetryMgr.LastMRTS+5s for any reports that have changes
		err = telemetryMgr.CheckOnChangeReports(nil, instancesUpdated)
		if err != nil {
			logger.Crit("Error Finding OnChange Reports for metrics", "instancesUpdated", instancesUpdated, "err", err)
		}
	}
}

func MakeHandlerClock(logger log.Logger, telemetryMgr *telemetryManager, bus eh.EventBus) func(eh.Event) {
	// close over lastHWM
	lastHWM := time.Time{}
	return func(event eh.Event) {
		// if no events have kickstarted the clock, bail
		if telemetryMgr.MetricTSHWM.IsZero() {
			return
		}

		// if no events come in during time between clock publishes, we'll artificially bump HWM forward.
		// if time is uninitialized, wait for an event to come in to set it
		if telemetryMgr.MetricTSHWM.Equal(lastHWM) {
			telemetryMgr.MetricTSHWM = telemetryMgr.MetricTSHWM.Add(clockPeriod)
		}
		lastHWM = telemetryMgr.MetricTSHWM

		// Generate any metric reports that need it
		reportList, _ := telemetryMgr.FastCheckForNeededMRUpdates()
		for _, report := range reportList {
			publishHelper(logger, bus, eh.NewEvent(metric.ReportGenerated, &metric.ReportGeneratedData{Name: report}, time.Now()))
		}
	}
}

func MakeHandlerMaintenance(logger log.Logger, telemetryMgr *telemetryManager, bus eh.EventBus) func(eh.Event) {
	return func(event eh.Event) {
		command, ok := event.Data().(string)
		if !ok {
			return
		}
		var err error

		switch command {
		case "optimize":
			fmt.Printf("Running scheduled database optimization\n")
			err = telemetryMgr.Optimize()
			if err != nil {
				logger.Crit("Optimize failed", "err", err)
			}

		case "vacuum":
			fmt.Printf("Running scheduled database storage recovery\n")
			err = telemetryMgr.Vacuum()
			if err != nil {
				logger.Crit("Vacuum failed", "err", err)
			}

		case "clean values": // keep us under database size limits
			fmt.Printf("Running scheduled cleanup of the stored Metric Values\n")
			err = telemetryMgr.DeleteOldestValues()
			if err != nil {
				logger.Crit("DeleteOldestValues failed.", "err", err)
			}

		case "delete orphans": // see factory comment for details.
			fmt.Printf("Running scheduled database consistency cleanup\n")
			err = telemetryMgr.DeleteOrphans()
			if err != nil {
				logger.Crit("Orphan delete failed", "err", err)
			}

		case "prune unused metric values":
			fmt.Printf("Running scheduled cleanup of the stored Metric Values\n")
			err = telemetryMgr.DeleteOldestValues()
			if err != nil {
				logger.Crit("DeleteOldestValues failed.", "err", err)
			}
			fmt.Printf("Running scheduled database consistency cleanup\n")
			err = telemetryMgr.DeleteOrphans()
			if err != nil {
				logger.Crit("Orphan delete failed", "err", err)
			}

		default:
			logger.Warn("Unknown database maintenance command string received", "command", command)
		}
	}
}
