package domain

import (
	"context"
	"errors"
	eh "github.com/superchalupa/eventhorizon"
	"net/http"

	"fmt"
)
var _ = fmt.Println

const (
	// EVENTS
	HTTPCmdProcessedEvent eh.EventType = "HTTPCmdProcessed"
)

type HTTPSaga struct {
}

// can put URI, odata.type, or odata.context as key
type HTTPSagaList struct {
    sagaList map[string]HTTPSaga
    commandBus eh.CommandBus
    repo eh.ReadRepo
}

func NewHTTPSagaList(commandbus eh.CommandBus, repo eh.ReadRepo) *HTTPSagaList {
    return &HTTPSagaList{
            sagaList: map[string]HTTPSaga{},
            commandBus: commandbus,
            repo: repo,
        }
}

type HTTPCmdProcessedData struct {
	CommandID eh.UUID
	Results   map[string]interface{}
    StatusCode int
    Headers map[string]string
}

func SetupHTTP() {
	// COMMAND registration
	eh.RegisterCommand(func() eh.Command { return &HandleHTTP{} })

	// EVENT registration
	eh.RegisterEventData(HTTPCmdProcessedEvent, func() eh.EventData { return &HTTPCmdProcessedData{} })
}

// LookupCommand will try to find a command for an http operation
// Search path:
//      ${METHOD}@odata.id
//      ${METHOD}@odata.type
//      ${METHOD}@odata.context
func (l *HTTPSagaList) RunHTTPOperation(ctx context.Context, treeID, cmdID eh.UUID, resource *RedfishResource, r *http.Request) (error) {
	aggregateID := resource.Properties["@odata.id"].(string)
	typ := resource.Properties["@odata.type"].(string)
	context := resource.Properties["@odata.context"].(string)
	method := r.Method

	search := []string{
		method + ":" + aggregateID,
		method + ":" + typ,
		method + ":" + context,
		"HandleHTTP",
	}

	for _, s := range search {
		cmd, err := eh.CreateCommand(eh.CommandType(s))
		if err == nil {
			cmdInit, ok := cmd.(Initializer)
			if ok {
				cmdInit.Initialize(l.repo, treeID, eh.UUID(aggregateID), cmdID, r)
                return l.commandBus.HandleCommand(ctx, cmd)
			}
		}
	}
	return errors.New("Command not found")
}

const (
	HandleHTTPCommand eh.CommandType = "HandleHTTP"
)

type HandleHTTP struct {
	UUID        eh.UUID
	CommandID   eh.UUID
	HTTPRequest *http.Request `eh:"optional"`

	// below is everything needed for command side to query the read side, if necessary.
	// This should be done in only very limited circumstances
	// also keep in mind that read side is only ***eventually*** consistent
	ReadSide eh.ReadRepo
	TreeID   eh.UUID
}

type Initializer interface {
	Initialize(eh.ReadRepo, eh.UUID, eh.UUID, eh.UUID, *http.Request)
}

func (c *HandleHTTP) Initialize(repo eh.ReadRepo, treeID, aggregateID, cmdid eh.UUID, r *http.Request) {
	c.UUID = aggregateID
	c.CommandID = cmdid
	c.HTTPRequest = r
	c.ReadSide = repo
	c.TreeID = treeID
}

func (c HandleHTTP) AggregateID() eh.UUID            { return c.UUID }
func (c HandleHTTP) AggregateType() eh.AggregateType { return RedfishResourceAggregateType }
func (c HandleHTTP) CommandType() eh.CommandType     { return HandleHTTPCommand }
func (c HandleHTTP) Handle(ctx context.Context, a *RedfishResourceAggregate) error {

	// Store HTTPCmdProcessedEvent in order to signal to the command is done
	// processing and to return the results that should be given back to the
	// user.
	a.StoreEvent(HTTPCmdProcessedEvent,
		&HTTPCmdProcessedData{
			CommandID: c.CommandID,
			Results:   map[string]interface{}{"MSG": "HELLO WORLD"},
		},
	)

	return nil
}
