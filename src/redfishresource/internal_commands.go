package domain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	eh "github.com/looplab/eventhorizon"
	log "github.com/superchalupa/sailfish/src/log"
	"github.com/superchalupa/sailfish/src/looplab/event"
)

func init() {
	eh.RegisterCommand(func() eh.Command { return &UpdateMetricRedfishResource{} })
	eh.RegisterCommand(func() eh.Command { return &CreateRedfishResource{} })
	eh.RegisterCommand(func() eh.Command { return &RemoveRedfishResource{} })
	eh.RegisterCommand(func() eh.Command { return &UpdateRedfishResourceProperties{} })
	eh.RegisterCommand(func() eh.Command { return &UpdateRedfishResourceProperties2{} })
	eh.RegisterCommand(func() eh.Command { return &RemoveRedfishResourceProperty{} })
	eh.RegisterCommand(func() eh.Command { return &InjectEvent{} })
}

const (
	CreateRedfishResourceCommand                 = eh.CommandType("internal:RedfishResource:Create")
	RemoveRedfishResourceCommand                 = eh.CommandType("internal:RedfishResource:Remove")
	UpdateMetricRedfishResourcePropertiesCommand = eh.CommandType("internal:RedfishResourceProperties:UpdateMetric")
	UpdateRedfishResourcePropertiesCommand       = eh.CommandType("internal:RedfishResourceProperties:Update")
	UpdateRedfishResourcePropertiesCommand2      = eh.CommandType("internal:RedfishResourceProperties:Update:2")
	RemoveRedfishResourcePropertyCommand         = eh.CommandType("internal:RedfishResourceProperties:Remove")
	InjectEventCommand                           = eh.CommandType("internal:Event:Inject")
)

// Static type checking for commands to prevent runtime errors due to typos
var _ = eh.Command(&CreateRedfishResource{})
var _ = eh.Command(&RemoveRedfishResource{})
var _ = eh.Command(&UpdateRedfishResourceProperties{})
var _ = eh.Command(&UpdateRedfishResourceProperties2{})
var _ = eh.Command(&RemoveRedfishResourceProperty{})
var _ = eh.Command(&InjectEvent{})

var immutableProperties = []string{"@odata.id", "@odata.type", "@odata.context"}

// CreateRedfishResource Command
type CreateRedfishResource struct {
	ID          eh.UUID `json:"id"`
	ResourceURI string
	Type        string
	Context     string
	Privileges  map[string]interface{}

	// optional stuff
	Headers       map[string]string      `eh:"optional"`
	Plugin        string                 `eh:"optional"`
	DefaultFilter string                 `eh:"optional"`
	Properties    map[string]interface{} `eh:"optional"`
	Meta          map[string]interface{} `eh:"optional"`
	Private       map[string]interface{} `eh:"optional"`
}

// AggregateType satisfies base Aggregate interface
func (c *CreateRedfishResource) AggregateType() eh.AggregateType { return AggregateType }

// AggregateID satisfies base Aggregate interface
func (c *CreateRedfishResource) AggregateID() eh.UUID { return c.ID }

// CommandType satisfies base Command interface
func (c *CreateRedfishResource) CommandType() eh.CommandType { return CreateRedfishResourceCommand }

func (c *CreateRedfishResource) Handle(ctx context.Context, a *RedfishResourceAggregate) error {

	requestLogger := ContextLogger(ctx, "internal_commands")
	requestLogger.Info("CreateRedfishResource", "META", a.Properties.Meta)

	if a.ID != eh.UUID("") {
		requestLogger.Error("Aggregate already exists!", "command", "CreateRedfishResource", "UUID", a.ID, "URI", a.ResourceURI, "request_URI", c.ResourceURI)
		return errors.New("Already created!")
	}
	a.ID = c.ID
	a.ResourceURI = c.ResourceURI
	a.DefaultFilter = c.DefaultFilter
	a.Plugin = c.Plugin
	a.Headers = make(map[string]string, len(c.Headers))
	for k, v := range c.Headers {
		a.Headers[k] = v
	}

	a.PrivilegeMap = make(map[HTTPReqType]interface{}, len(c.Privileges))
	for k, v := range c.Privileges {
		a.PrivilegeMap[MapStringToHTTPReq(k)] = v
	}

	// ensure no collisions
	for _, p := range immutableProperties {
		delete(c.Properties, p)
	}

	d := &RedfishResourcePropertiesUpdatedData{
		ID:            c.ID,
		ResourceURI:   a.ResourceURI,
		PropertyNames: []string{},
	}
	e := &RedfishResourcePropertyMetaUpdatedData{
		ID:          c.ID,
		ResourceURI: a.ResourceURI,
		Meta:        map[string]interface{}{},
	}

	v := map[string]interface{}{}
	a.Properties.Value = v
	a.Properties.Parse(c.Properties)
	a.Properties.Meta = c.Meta

	var resourceURI []string
	// preserve slashes
	for _, x := range strings.Split(c.ResourceURI, "/") {
		resourceURI = append(resourceURI, url.PathEscape(x))
	}

	v["@odata.id"] = strings.Join(resourceURI, "/")
	v["@odata.type"] = c.Type
	v["@odata.context"] = c.Context

	// send out event that it's created first
	a.PublishEvent(eh.NewEvent(RedfishResourceCreated, &RedfishResourceCreatedData{
		ID:          c.ID,
		ResourceURI: c.ResourceURI,
	}, time.Now()))

	// then send out possible notifications about changes in the properties or meta
	if len(d.PropertyNames) > 0 {
		a.PublishEvent(eh.NewEvent(RedfishResourcePropertiesUpdated, d, time.Now()))
	}
	if len(e.Meta) > 0 {
		a.PublishEvent(eh.NewEvent(RedfishResourcePropertyMetaUpdated, e, time.Now()))
	}

	return nil
}

// RemoveRedfishResource Command
type RemoveRedfishResource struct {
	ID          eh.UUID `json:"id"`
	ResourceURI string  `eh:"optional"`
}

// AggregateType satisfies base Aggregate interface
func (c *RemoveRedfishResource) AggregateType() eh.AggregateType { return AggregateType }

// AggregateID satisfies base Aggregate interface
func (c *RemoveRedfishResource) AggregateID() eh.UUID { return c.ID }

// CommandType satisfies base Command interface
func (c *RemoveRedfishResource) CommandType() eh.CommandType { return RemoveRedfishResourceCommand }

func (c *RemoveRedfishResource) Handle(ctx context.Context, a *RedfishResourceAggregate) error {
	a.PublishEvent(eh.NewEvent(RedfishResourceRemoved, &RedfishResourceRemovedData{
		ID:          c.ID,
		ResourceURI: a.ResourceURI,
	}, time.Now()))
	return nil
}

type RemoveRedfishResourceProperty struct {
	ID       eh.UUID `json:"id"`
	Property string  `eh:"optional"`
}

// AggregateType satisfies base Aggregate interface
func (c *RemoveRedfishResourceProperty) AggregateType() eh.AggregateType { return AggregateType }

// AggregateID satisfies base Aggregate interface
func (c *RemoveRedfishResourceProperty) AggregateID() eh.UUID { return c.ID }

// CommandType satisfies base Command interface
func (c *RemoveRedfishResourceProperty) CommandType() eh.CommandType {
	return RemoveRedfishResourcePropertyCommand
}
func (c *RemoveRedfishResourceProperty) Handle(ctx context.Context, a *RedfishResourceAggregate) error {
	properties := a.Properties.Value.(map[string]interface{})
	for key, _ := range properties {
		if key == c.Property {
			delete(properties, key)
		}
	}
	return nil
}

// toUpdate	{path2key : value}
type UpdateRedfishResourceProperties2 struct {
	ID         eh.UUID `json:"id"`
	Properties map[string]interface{}
}

// AggregateType satisfies base Aggregate interface
func (c *UpdateRedfishResourceProperties2) AggregateType() eh.AggregateType { return AggregateType }

// AggregateID satisfies base Aggregate interface
func (c *UpdateRedfishResourceProperties2) AggregateID() eh.UUID { return c.ID }

// CommandType satisfies base Command interface
func (c *UpdateRedfishResourceProperties2) CommandType() eh.CommandType {
	return UpdateRedfishResourcePropertiesCommand2
}

// aggregate is a.Properties.(RedfishresourceProperty)
// going through the aggregate it is [map]*RedfishResourceProperty...
// Updated to append to list.  TODO need a way to clean lists and prevent duplicates
func UpdateAgg(a *RedfishResourceAggregate, pathSlice []string, v interface{}, appendLimit int) error {
	loc, ok := a.Properties.Value.(map[string]interface{})
	if !ok {
		return errors.New(fmt.Sprintf("Updateagg: aggregate wis wrong type %T", a.Properties.Value))
	}

	plen := len(pathSlice) - 1
	for i, p := range pathSlice {
		k, ok := loc[p]
		if !ok {
			return fmt.Errorf("UpdateAgg Failed can not find %s in %+v", p, loc)
		}
		switch k.(type) {
		case *RedfishResourceProperty:
			k2, ok := k.(*RedfishResourceProperty)
			if !ok {
				return fmt.Errorf("UpdateAgg Failed, RedfishResourcePropertyFailed")
			}
			// metric events have the data appended
			switch v.(type) {
			case []interface{}, []map[string]interface{}:
				j := k2.Value.([]interface{})
				aggSLen := len(j)
				v2 := v.([]interface{})
				if aggSLen >= appendLimit {
					continue
				}
				if appendLimit < aggSLen+len(v2) {
					k2.Parse(v2[appendLimit-aggSLen:])
				} else {
					k2.Parse(v2)
				}
				k2.Parse(j)

				return nil
			default:
				if (plen == i) && (k2.Value != v) {
					k2.Value = v
				} else if plen == i {
					return nil
				} else {
					tmp := k2.Value
					loc, ok = tmp.(map[string]interface{})
					if !ok {
						return fmt.Errorf("UpdateAgg Failed %s type cast to map[string]interface{} for %+v  errored for %+v", a.ResourceURI, p, pathSlice)
					}
				}
			}

		default:
			return fmt.Errorf("agg update for slice %+v, received type %T instead of *RedfishResourceProperty", pathSlice, k)
		}
	}
	return nil

}

func GetValueinAgg(a *RedfishResourceAggregate, pathSlice []string) interface{} {
	a.Properties.Lock()
	defer a.Properties.Unlock()
	loc, ok := a.Properties.Value.(map[string]interface{})
	if !ok {
		return errors.New(fmt.Sprintf("GetValueinAgg: aggregate value is not a map[string]interface{}, but %T", a.Properties.Value))
	}

	plen := len(pathSlice) - 1
	for i, p := range pathSlice {
		k, ok := loc[p]
		if !ok {
			return fmt.Errorf("UpdateAgg Failed can not find %s in %+v", p, loc)
		}
		switch k.(type) {
		case *RedfishResourceProperty:
			k2, ok := k.(*RedfishResourceProperty)
			if !ok {
				return fmt.Errorf("UpdateAgg Failed, RedfishResourcePropertyFailed")
			}
			// metric events have the data appended
			if plen == i {
				return k2.Value
			} else if plen == i {
				return nil
			} else {
				tmp := k2.Value
				loc, ok = tmp.(map[string]interface{})
				if !ok {
					return fmt.Errorf("UpdateAgg Failed %s type cast to map[string]interface{} for %+v  errored for %+v", a.ResourceURI, p, pathSlice)
				}
			}

		default:
			return fmt.Errorf("agg update for slice %+v, received type %T instead of *RedfishResourceProperty", pathSlice, k)
		}
	}

	return nil

}

func validateValue(val interface{}) error {
	switch val.(type) {
	case []interface{}, map[string]interface{}:
		return fmt.Errorf("Update Agg does not support type %T", val)
	default:
		return nil
	}
}

//  This is handled by eventhorizon code.
//  When a CommandHandler "Handle" is called it will retrieve the aggregate from the DB.  and call this Handle. Then save the aggregate 'a' back to the db.  no locking is required..
// provide error when no change made..
func (c *UpdateRedfishResourceProperties2) Handle(ctx context.Context, a *RedfishResourceAggregate) error {

	if a.ID == eh.UUID("") {
		requestLogger := ContextLogger(ctx, "internal_commands")
		requestLogger.Error("Aggregate does not exist!", "UUID", a.ID, "URI", a.ResourceURI)
		return errors.New("non existent aggregate")
	}

	var err error = nil

	d := &RedfishResourcePropertiesUpdatedData2{
		ID:            c.ID,
		ResourceURI:   a.ResourceURI,
		PropertyNames: make(map[string]interface{}),
	}

	// update properties in aggregate
	for k, v := range c.Properties {
		pathSlice := strings.Split(k, "/")

		err := UpdateAgg(a, pathSlice, v, 0)

		if err == nil {
			d.PropertyNames[k] = v
		}
	}

	if len(d.PropertyNames) > 0 {
		a.PublishEvent(eh.NewEvent(RedfishResourcePropertiesUpdated2, d, time.Now()))
	}
	return err
}

type UpdateMetricRedfishResource struct {
	ID               eh.UUID                `json:"id"`
	Properties       map[string]interface{} `eh:"optional"`
	AppendLimit      int
	ReportUpdateType string
}

// AggregateType satisfies base Aggregate interface
func (c *UpdateMetricRedfishResource) AggregateType() eh.AggregateType { return AggregateType }

// AggregateID satisfies base Aggregate interface
func (c *UpdateMetricRedfishResource) AggregateID() eh.UUID { return c.ID }

// CommandType satisfies base Command interface
func (c *UpdateMetricRedfishResource) CommandType() eh.CommandType {
	return UpdateMetricRedfishResourcePropertiesCommand
}

//reportUpdateType int // 0-AppendStopsWhenFull, 1-AppendWrapsWhenFull, 3- NewReport, 4-Overwrite

// assume AppendStopsWhenFull
func (c *UpdateMetricRedfishResource) Handle(ctx context.Context, a *RedfishResourceAggregate) error {
	for k, v := range c.Properties {
		pathSlice := strings.Split(k, "/")
		if err := UpdateAgg(a, pathSlice, v, int(c.AppendLimit)); err != nil {
			fmt.Println("failed to updated agg")
			return err
		}
	}

	return nil
}

type UpdateRedfishResourceProperties struct {
	ID         eh.UUID                `json:"id"`
	Properties map[string]interface{} `eh:"optional"`
}

// AggregateType satisfies base Aggregate interface
func (c *UpdateRedfishResourceProperties) AggregateType() eh.AggregateType { return AggregateType }

// AggregateID satisfies base Aggregate interface
func (c *UpdateRedfishResourceProperties) AggregateID() eh.UUID { return c.ID }

// CommandType satisfies base Command interface
func (c *UpdateRedfishResourceProperties) CommandType() eh.CommandType {
	return UpdateRedfishResourcePropertiesCommand
}
func (c *UpdateRedfishResourceProperties) Handle(ctx context.Context, a *RedfishResourceAggregate) error {
	// ensure no collisions with immutable properties
	for _, p := range immutableProperties {
		delete(c.Properties, p)
	}

	d := &RedfishResourcePropertiesUpdatedData{
		ID:            c.ID,
		ResourceURI:   a.ResourceURI,
		PropertyNames: []string{},
	}
	e := &RedfishResourcePropertyMetaUpdatedData{
		ID:          c.ID,
		ResourceURI: a.ResourceURI,
		Meta:        map[string]interface{}{},
	}

	a.Properties.Parse(c.Properties)

	if len(d.PropertyNames) > 0 {
		a.PublishEvent(eh.NewEvent(RedfishResourcePropertiesUpdated, d, time.Now()))
	}
	if len(e.Meta) > 0 {
		a.PublishEvent(eh.NewEvent(RedfishResourcePropertyMetaUpdated, e, time.Now()))
	}

	return nil
}

type InjectEvent struct {
	Wg sync.WaitGroup `eh:"optional"`

	EventSeq   int64             `json:"event_seq" eh:"optional"`
	EventData  json.RawMessage   `json:"data" eh:"optional"`
	EventArray []json.RawMessage `json:"event_array" eh:"optional"`
	ID         eh.UUID           `json:"id" eh:"optional"`
	Name       eh.EventType      `json:"name"`
	Encoding   string            `eh:"optional" json:"encoding"`

	// EventBarrier is set if this event should block subsequent events until it is processed
	Barrier bool `eh:"optional"`

	// Synchronous set if POST should not return until the message is processed
	Synchronous bool `eh:"optional"`

	// context is if the upstream HTTP request is cancelled before we finish (for Synchronous messages)
	Ctx context.Context `eh:"optional"`
}

// AggregateType satisfies base Aggregate interface
func (c *InjectEvent) AggregateType() eh.AggregateType { return AggregateType }

// AggregateID satisfies base Aggregate interface
func (c *InjectEvent) AggregateID() eh.UUID { return c.ID }

// CommandType satisfies base Command interface
func (c *InjectEvent) CommandType() eh.CommandType {
	return InjectEventCommand
}

type eventBundle struct {
	event   event.SyncEvent
	barrier bool
}

var injectChanSlice chan *InjectEvent
var injectChan chan *eventBundle

// inject event timeout
var IETIMEOUT time.Duration = 250 * time.Millisecond

func StartInjectService(logger log.Logger, d *DomainObjects) {
	injectChanSlice = make(chan *InjectEvent, 100)
	injectChan = make(chan *eventBundle, 10)
	logger = logger.New("module", "injectservice")
	eb := d.EventBus
	ew := d.EventWaiter

	var s closeNotifier
	s, err := NewSdnotify()
	if err != nil {
		fmt.Printf("Error setting up SD_NOTIFY, using simulation instead: %s\n", err)
		s = SimulateSdnotify()
	}

	go func() {
		defer s.Close()
		interval := s.GetIntervalUsec()
		if interval == 0 {
			fmt.Printf("Watchdog interval is not set, so skipping watchdog setup. Set WATCHDOG_USEC to set.\n")
			return
		}

		// send watchdogs 3x per interval
		fmt.Printf("Setting up watchdog\n")
		interval = interval / 3
		seq := 0

		// set up listener for the watchdog events
		listener, err := ew.Listen(context.Background(), func(event eh.Event) bool {
			if event.EventType() == WatchdogEvent {
				return true
			}
			return false
		})

		if err != nil {
			panic("Could not start listener")
		}

		// endless loop generating and responding to watchdog events
		for {
			select {
			// pet watchdog when we get an event
			case ev := <-listener.Inbox():
				if evtS, ok := ev.(event.SyncEvent); ok {
					evtS.Done()
				}
				s.Notify("WATCHDOG=1")

			// periodically send event on bus to force watchdog
			case <-time.After(time.Duration(interval) * time.Microsecond):
				ev := event.NewSyncEvent(WatchdogEvent, &WatchdogEventData{Seq: seq}, time.Now())
				ev.Add(1)
				// use watchdogs to clean out cruft. Maybe a good idea, not sure.
				injectChan <- &eventBundle{ev, true}
				seq++
			}
		}
	}()

	// goroutine to synchronously handle the event inject queue
	go func() {
		queued := []*InjectEvent{}
		internalSeq := int64(0)
		// The 'standard' way to create a stopped timer
		sequenceTimer := time.NewTimer(math.MaxInt64)
		if !sequenceTimer.Stop() {
			<-sequenceTimer.C
		}
		timerActive := false

		tryToPublish := func(tryHard bool) {
			// iterate through our queue until we find a message beyond our current sequence, then stop
			i := 0
			for i = 0; i < len(queued); i++ {
				injectCmd := queued[i]

				// resync to earliest event if needed
				if injectCmd.EventSeq == -1 {
					internalSeq = -1
				}

				if injectCmd.EventSeq < internalSeq {
					// event is older than last published event, drop
					dropped_event := &DroppedEventData{
						Name:     injectCmd.Name,
						EventSeq: injectCmd.EventSeq,
					}
					injectChan <- &eventBundle{event.NewSyncEvent(DroppedEvent, dropped_event, time.Now()), false}
					continue
				}

				// if the seq is correct, send it
				//  or if internal seq has been reset, send and take the identity of that seq
				//  or if we are in a "force" send all events, send it.
				if injectCmd.EventSeq == internalSeq || internalSeq == 0 || tryHard {
					injectCmd.sendToChn()
					// command with seq == -1 will "reset". The counter increments to 0 and the next event becomes the new starting sequence
					internalSeq = injectCmd.EventSeq
					internalSeq++
					continue
				}

				break //  injectCmd.EventSeq > internalSeq, no sense going through the rest
			}

			// trim off any processed commands
			queued = append([]*InjectEvent{}, queued[i:]...)
		}

		for {
			select {
			case event := <-injectChanSlice:
				fmt.Printf("POP  injectChanSlice LEN: %s\n", len(injectChanSlice))
				queued = append(queued, event)
				if len(queued) > 1 {
					sort.SliceStable(queued, func(i, j int) bool {
						return queued[i].EventSeq < queued[j].EventSeq
					})
				}

				if len(queued) < 1 {
					break
				}

				fmt.Printf("\tqueue len %d  timer(%s)\n", len(queued), timerActive)

				// queue is sorted, so first event seq can be checked
				//   any events less than or equal to internalSeq can be dealt with
				//   either by dropping them or sending them
				if queued[0].EventSeq <= internalSeq {
					tryToPublish(false)
				}

				fmt.Printf("\tqueue len %d  timer(%s)\n", len(queued), timerActive)

				// oops, we have some left, start a new timer
				if len(queued) > 0 && !timerActive {
					sequenceTimer.Reset(IETIMEOUT)
					timerActive = true
				}

				// we got everything, stop any timers
				if len(queued) == 0 && timerActive {
					if !sequenceTimer.Stop() {
						<-sequenceTimer.C
					}
					timerActive = false
				}

			case <-sequenceTimer.C:
				logger.Crit("TIMEOUT waiting for missing sequence events. force send.")
				// we timed out waiting
				timerActive = false
				internalSeq = 0
				tryToPublish(true)

				// oops, we have some left, start a timer
				if len(queued) > 0 {
					sequenceTimer.Reset(IETIMEOUT)
				}
			}
		}
	}()

	go func() {
		for {
			select {
			case evb := <-injectChan:
				eb.PublishEvent(context.Background(), evb.event)
				// barrier is set if this event should block events after it
				if evb.barrier {
					evb.event.Wait()
				}
			case <-time.After(time.Duration(5) * time.Second):
				// debug if we start getting full channels
				if len(injectChan) > 0 {
					fmt.Printf("InjectChan queue: %d / %d\n", len(injectChan), cap(injectChan))
				}
			}
		}
	}()

}

const MAX_CONSOLIDATED_EVENTS = 5
const injectUUID = eh.UUID("49467bb4-5c1f-473b-0000-00000000000f")

type Decoder interface {
	Decode(d map[string]interface{}) error
}

func (c *InjectEvent) Handle(ctx context.Context, a *RedfishResourceAggregate) error {
	testLogger := ContextLogger(ctx, "internal_commands").New("module", "inject_event")
	// testLogger.Crit("Event handle", "Sequence", c.EventSeq, "Name", c.Name)
	a.ID = injectUUID
	c.Ctx = ctx

	c.Wg.Add(1)
	fmt.Printf("PUSH injectChanSlice LEN: %s\n", len(injectChanSlice))
	injectChanSlice <- c
	if c.Synchronous {
		testLogger.Info("WAIT", "Sequence", c.EventSeq, "Name", c.Name)
		c.Wg.Wait()
	}

	return nil
}

// markBarrier will mark specific events as barrier events, ie. that they
// prevent any events from being added behind it in the queue until it has been
// fully processed
//
// This is somewhat arbitrary and is domain-specific knowledge
//
func (c *InjectEvent) markBarrier() {
	switch c.Name {
	// can create objects that are needed by subsequent events
	case "ComponentEvent",
		"LogEvent",
		"FaultEntryAdd":
		c.Barrier = true

		// rare
		c.Barrier = false

	// these can overwhelm, but want to process quickly
	case "AttributeUpdated":
		// just a swag: barrier every 5th one
		c.Barrier = false
		if c.EventSeq%5 == 0 {
			c.Barrier = true
		}

	case "AvgPowerConsumptionStatDataObjEvent",
		"FileReadEvent",
		"FanEvent",
		"PowerConsumptionDataObjEvent",
		"PowerSupplyObjEvent",
		"TemperatureHistoryEvent",
		"ThermalSensorEvent",
		"thp_fan_data_object":
		c.Barrier = false

	// rare events, or events that can't arrive quickly
	case "HealthEvent", "IomCapability":
		c.Barrier = false

	}
}

func (c *InjectEvent) sendToChn() error {
	//requestLogger := ContextLogger(ctx, "internal_commands").New("module", "inject_event")
	//requestLogger.Crit("InjectService: preparing event", "Sequence", c.EventSeq, "Name", c.Name)

	waits := []func(){}
	defer func() {
		defer c.Wg.Done() // this is what supports "Synchronous" commands
		for _, fn := range waits {
			defer fn()
		}
	}()
	trainload := make([]eh.EventData, 0, MAX_CONSOLIDATED_EVENTS)
	sendTrain := func(force bool) {
		// limit number of consolidated events to prevent overflowing queues and deadlocking
		if (force && len(trainload) > 0) || len(trainload) >= MAX_CONSOLIDATED_EVENTS {
			// for now, specific check for events that should be barrier events
			c.markBarrier()

			e := &eventBundle{event: event.NewSyncEvent(c.Name, trainload, time.Now()), barrier: c.Barrier}
			e.event.Add(1) // for EVENT "barrier"
			if c.Synchronous {
				waits = append(waits, e.event.Wait)
			}
			select {
			case injectChan <- e:
			case <-c.Ctx.Done():
			}

			trainload = make([]eh.EventData, 0, MAX_CONSOLIDATED_EVENTS)
		}
	}

	decode := func(m json.RawMessage) {
		if m == nil {
			fmt.Printf("Decode: rawmessage is nil\n")
			return
		}
		var data interface{}
		data, err := eh.CreateEventData(c.Name)
		if err != nil {
			fmt.Printf("Decode(%s): fallback to map[string]interface{}: %s\n", c.Name, err)
			data = map[string]interface{}{}
		}

		// check if event wants to deserialize itself with a custom decoder
		// this will handle DM objects
		if ds, ok := data.(Decoder); ok {
			fmt.Printf("Decode: try Decoder\n")
			eventData := map[string]interface{}{}
			err := json.Unmarshal(m, &eventData)
			if err != nil {
				fmt.Printf("Decode: unmarshal rawmessage failed: %s\n", err)
				return
			}

			err = ds.Decode(eventData)
			if err != nil {
				// failed decode, just send the raw binary data and see what happens
				fmt.Printf("ERROR DECODING EVENT: %s\n", err)
				trainload = append(trainload, eventData) //preallocated
				return
			}
			trainload = append(trainload, data) //preallocated
			return
		}

		err = json.Unmarshal(c.EventData, &data)
		if err != nil {
			fmt.Printf("Decode message: unmarshal rawmessage failed: %s\n", err)
			return
		}
		trainload = append(trainload, data)
	}

	// create a new, empty event of the requested type. The data will be deserialized into it.
	decode(c.EventData)
	for _, d := range c.EventArray {
		sendTrain(false)
		decode(d)
	}

	// finally, force send the final load
	sendTrain(true)

	return nil
}
