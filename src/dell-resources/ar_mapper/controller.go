package ar_mapper

import (
	"context"
	"sync"
	"time"

	"github.com/spf13/viper"

	eh "github.com/looplab/eventhorizon"
	"github.com/looplab/eventhorizon/utils"
	"github.com/superchalupa/go-redfish/src/log"

	"github.com/superchalupa/go-redfish/src/dell-resources/attributes"
	"github.com/superchalupa/go-redfish/src/ocp/event"
	"github.com/superchalupa/go-redfish/src/ocp/model"
)

type mapping struct {
	Property string
	FQDD     string
	Group    string
	Index    string
	Name     string
}

type ARMappingController struct {
	mappings   []mapping
	mappingsMu sync.RWMutex
	logger     log.Logger
	name       string

	eb eh.EventBus
}

func New(ctx context.Context, logger log.Logger, m *model.Model, name string, ch eh.CommandHandler, eb eh.EventBus, ew *utils.EventWaiter) (*ARMappingController, error) {
	c := &ARMappingController{
		mappings: []mapping{},
		name:     name,
		logger:   logger,
		eb:       eb,
	}

	// stream processor for action events
	sp, err := event.NewEventStreamProcessor(ctx, ew, event.CustomFilter(selectAttributeUpdate()))
	if err != nil {
		logger.Error("Failed to create event stream processor", "err", err)
		return nil, err
	}
	sp.RunForever(func(event eh.Event) {
		if data, ok := event.Data().(*attributes.AttributeUpdatedData); ok {
			c.mappingsMu.RLock()
			defer c.mappingsMu.RUnlock()
			logger.Debug("Process Event", "data", data)
			for _, mapping := range c.mappings {
				if data.FQDD != mapping.FQDD {
					continue
				}
				if data.Group != mapping.Group {
					continue
				}
				if data.Index != mapping.Index {
					continue
				}
				if data.Name != mapping.Name {
					continue
				}

				logger.Info("Updating Model", "mapping", mapping, "data", data)
				m.UpdateProperty(mapping.Property, data.Value)
			}
		} else {
			logger.Warn("Should never happen: got an invalid event in the event handler")
		}
	})

	return c, nil
}

func (c *ARMappingController) UpdateRequest(ctx context.Context, property string, value interface{}) (interface{}, error) {
	for _, mapping := range c.mappings {
		if property != mapping.Property {
			continue
		}

		c.logger.Info("Sending Update Request", "mapping", mapping, "value", value)
		reqUUID := eh.NewUUID()

		data := attributes.AttributeUpdateRequestData{
			ReqID: reqUUID,
			FQDD:  mapping.FQDD,
			Group: mapping.Group,
			Index: mapping.Index,
			Name:  mapping.Name,
			Value: value,
		}
		c.eb.PublishEvent(ctx, eh.NewEvent(attributes.AttributeUpdateRequest, data, time.Now()))

		// TODO: wait for event to come back matching request
	}

	return value, nil
}

// this is the function that viper will call whenever the configuration changes at runtime
func (c *ARMappingController) ConfigChangedFn(ctx context.Context, cfg *viper.Viper) {
	c.mappingsMu.Lock()
	defer c.mappingsMu.Unlock()

	k := cfg.Sub("mappings")
	if k == nil {
		c.logger.Warn("missing config file section: 'mappings'")
		return
	}
	err := k.UnmarshalKey(c.name, &c.mappings)
	if err != nil {
		c.logger.Warn("unamrshal failed", "err", err)
	}
	c.logger.Info("updating mappings", "mappings", c.mappings)

	go c.initialStartupBootstrap(ctx)
}

//
// background thread that sends messages to the data pump to ask for startup values
//
func (c *ARMappingController) initialStartupBootstrap(ctx context.Context) {
	// bypass for now
	return

	for {
		time.Sleep(120 * time.Second)
		for _, m := range c.mappings {
			c.logger.Info("SENDING ATTRIBUTE REQUEST", "mapping", m)
			data := attributes.AttributeGetCurrentValueRequestData{
				FQDD:  m.FQDD,
				Group: m.Group,
				Index: m.Index,
				Name:  m.Name,
			}
			c.eb.PublishEvent(ctx, eh.NewEvent(attributes.AttributeGetCurrentValueRequest, data, time.Now()))
		}
	}
}

func selectAttributeUpdate() func(eh.Event) bool {
	return func(event eh.Event) bool {
		if event.EventType() == attributes.AttributeUpdated {
			return true
		}
		return false
	}
}
