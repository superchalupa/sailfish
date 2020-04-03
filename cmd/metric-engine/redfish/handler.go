// +build redfish

package redfish

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/gorilla/mux"
	"github.com/spf13/viper"

	eh "github.com/looplab/eventhorizon"

	"github.com/superchalupa/sailfish/cmd/metric-engine/metric"
	"github.com/superchalupa/sailfish/cmd/metric-engine/telemetry"
	log "github.com/superchalupa/sailfish/src/log"
	"github.com/superchalupa/sailfish/src/looplab/eventwaiter"
)

const (
	// purely redfish centric events
	SubmitTestMetricReportCommandEvent  eh.EventType = "SubmitTestMetricReportCommandEvent"
	SubmitTestMetricReportResponseEvent eh.EventType = "SubmitTestMetricReportResponseEvent"

	defaultRequestTimeout = 5 * time.Second
)

type busComponents interface {
	GetBus() eh.EventBus
	GetWaiter() *eventwaiter.EventWaiter
}

type RFServer struct {
	logger log.Logger
	d      busComponents
}

type SubmitTestMetricReportCommandData struct {
	metric.Command
	MetricReport json.RawMessage
}

type SubmitTestMetricReportResponseData struct {
	metric.CommandResponse
}

func (u *SubmitTestMetricReportCommandData) UseInput(ctx context.Context, logger log.Logger, r io.Reader) error {
	decoder := json.NewDecoder(r)
	return decoder.Decode(&u.MetricReport)
}

func RegisterEvents() {
	// register events
	eh.RegisterEventData(SubmitTestMetricReportCommandEvent, func() eh.EventData {
		return &SubmitTestMetricReportCommandData{Command: metric.NewCommand(SubmitTestMetricReportResponseEvent)}
	})
	eh.RegisterEventData(SubmitTestMetricReportResponseEvent, func() eh.EventData { return &SubmitTestMetricReportResponseData{} })
}

func NewRedfishServer(logger log.Logger, d busComponents) *RFServer {
	return &RFServer{logger: logger, d: d}
}

func (rf *RFServer) AddHandlersToRouter(m *mux.Router) {
	m.HandleFunc("/redfish/v1/TelemetryService/", rf.makeCommand(telemetry.AddMetricReportDefinition)).Methods("POST")
	m.HandleFunc("/redfish/v1/TelemetryService/Actions/TelemetryService.SubmitTestMetricReport", rf.makeCommand(SubmitTestMetricReportCommandEvent)).Methods("POST")
	m.HandleFunc("/redfish/v1/TelemetryService/MetricReportDefinitions", rf.makeCommand(telemetry.AddMetricReportDefinition)).Methods("POST")
	m.HandleFunc("/redfish/v1/TelemetryService/MetricReportDefinitions/{ID}", rf.makeCommand(telemetry.UpdateMetricReportDefinition)).Methods("PATCH")
	m.HandleFunc("/redfish/v1/TelemetryService/MetricReportDefinitions/{ID}", rf.makeCommand(telemetry.UpdateMetricReportDefinition)).Methods("PUT")
	m.HandleFunc("/redfish/v1/TelemetryService/MetricReportDefinitions/{ID}", rf.makeCommand(telemetry.DeleteMetricReportDefinition)).Methods("DELETE")
	m.HandleFunc("/redfish/v1/TelemetryService/MetricReports/{ID}", rf.makeCommand(telemetry.DeleteMetricReport)).Methods("DELETE")
	// generic handler last
	m.PathPrefix("/redfish/v1/TelemetryService").HandlerFunc(rf.makeCommand(telemetry.GenericGETCommandEvent)).Methods("GET")
}

type eventHandler interface {
	AddEventHandler(string, eh.EventType, func(eh.Event))
}

func Startup(logger log.Logger, cfg *viper.Viper, am3Svc eventHandler, d busComponents) error {
	// Important: don't leak 'cfg' outside the scope of this function!
	am3Svc.AddEventHandler("Submit Test Metric Report", SubmitTestMetricReportCommandEvent, MakeHandlerSubmitTestMR(logger, d.GetBus()))
	return nil
}

func MakeHandlerSubmitTestMR(logger log.Logger, bus eh.EventBus) func(eh.Event) {
	// TODO: this function will need to open pipes and write out the MR
	return func(event eh.Event) {
		testMR, ok := event.Data().(*SubmitTestMetricReportCommandData)
		if !ok {
			logger.Crit("handler got event of incorrect format")
			return
		}

		fmt.Printf("\nSUBMIT TEST METRIC REPORT\n")

		// Generate a "response" event that carries status back to initiator
		respEvent, err := testMR.NewResponseEvent(nil)
		if err != nil {
			logger.Crit("Error creating response event", "err", err, "testmr", testMR)
			return
		}

		// Should add the populated metric report definition event as a response?
		err = bus.PublishEvent(context.Background(), respEvent)
		if err != nil {
			logger.Crit("Error publishing", "err", err)
		}
	}
}

type Commander interface {
	GetRequestID() eh.UUID
	ResponseWaitFn() func(eh.Event) bool
}

type inputUser interface {
	UseInput(context.Context, log.Logger, io.Reader) error
}

type varUser interface {
	UseVars(context.Context, log.Logger, map[string]string) error
}

func requestContextFromCommand(r *http.Request, cmd interface{}) (context.Context, Commander) {
	intCmd, ok := cmd.(Commander)
	if ok {
		return log.WithRequestID(r.Context(), intCmd.GetRequestID()), intCmd
	}
	return r.Context(), nil
}

type Response interface {
	GetError() error
	GetStatusCode() int
	StreamResponse(io.Writer)
}

func (rf *RFServer) makeCommand(eventType eh.EventType) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		fn := telemetry.Factory(eventType)
		evt, err := fn()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		cmd := evt.Data()
		reqCtx, intCmd := requestContextFromCommand(r, cmd)
		timeoutCtx, cancel := context.WithTimeout(reqCtx, defaultRequestTimeout)
		defer cancel()
		requestLogger := log.ContextLogger(timeoutCtx, "REDFISH_HANDLER")

		if d, ok := cmd.(inputUser); ok {
			err := d.UseInput(timeoutCtx, requestLogger, r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		}

		if d, ok := cmd.(varUser); ok {
			vars := mux.Vars(r)
			vars["uri"] = r.URL.Path
			err := d.UseVars(timeoutCtx, requestLogger, vars)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		}

		if intCmd == nil {
			http.Error(w, "internal error: not a command", http.StatusInternalServerError)
			return
		}

		l := eventwaiter.NewListener(timeoutCtx, requestLogger, rf.d.GetWaiter(), intCmd.ResponseWaitFn())
		l.Name = "Redfish Response Listener"
		defer l.Close()

		requestLogger.Crit("HANDLE", "Method", r.Method, "Event", fmt.Sprintf("%v", evt), "Command", fmt.Sprintf("%+v", cmd))

		err = rf.d.GetBus().PublishEvent(context.Background(), evt)
		if err != nil {
			requestLogger.Crit("Error publishing event. This should never happen!", "err", err)
			http.Error(w, "internal error publishing", http.StatusInternalServerError)
			return
		}

		ret, err := l.Wait(timeoutCtx)
		if err != nil {
			// most likely user disconnected before we sent response
			requestLogger.Info("Wait ERROR", "err", err)
			http.Error(w, "internal error waiting", http.StatusInternalServerError)
			return
		}
		requestLogger.Crit("RESPONSE", "Event", fmt.Sprintf("%v", ret), "RESPONSE", fmt.Sprintf("%+v", ret.Data()))
		d := ret.Data()
		resp, ok := d.(Response)
		if !ok {
			requestLogger.Info("Got a non-response", "err", err)
			http.Error(w, "internal error with response", http.StatusInternalServerError)
			return
		}

		if resp.GetError() != nil {
			w.WriteHeader(http.StatusBadRequest)
		}

		// TODO: need to get this up to redfish standards for return
		// Need:
		// - HTTP headers. Location header for collection POST
		// - Return the created object. Is this optional?

		w.WriteHeader(resp.GetStatusCode())
		resp.StreamResponse(w)
	}
}
