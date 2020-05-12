package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"runtime/debug"
	"sync"
	"syscall"
	"time"

	flag "github.com/spf13/pflag"
	"github.com/spf13/viper"

	eh "github.com/looplab/eventhorizon"
	eventpublisher "github.com/looplab/eventhorizon/publisher/local"
	"github.com/superchalupa/sailfish/src/fileutils"
	log "github.com/superchalupa/sailfish/src/log"
	"github.com/superchalupa/sailfish/src/looplab/eventbus"
	"github.com/superchalupa/sailfish/src/looplab/eventwaiter"

	"github.com/superchalupa/sailfish/src/log15adapter"
)

const (
	forceGCInterval = 30 * time.Second
)

type busComponents struct {
	EventBus       eh.EventBus
	EventWaiter    *eventwaiter.EventWaiter
	EventPublisher eh.EventPublisher
}

type busIntf interface {
	GetBus() eh.EventBus
	GetWaiter() *eventwaiter.EventWaiter
	GetPublisher() eh.EventPublisher
}

func (d *busComponents) GetBus() eh.EventBus                 { return d.EventBus }
func (d *busComponents) GetWaiter() *eventwaiter.EventWaiter { return d.EventWaiter }
func (d *busComponents) GetPublisher() eh.EventPublisher     { return d.EventPublisher }

// nolint: gochecknoglobals
// couldnt really find a better way of doing the compile time optional registration, so basically need some globals
var (
	initOptionalLock   = sync.Once{}
	optionalComponents []func(log.Logger, *viper.Viper, busIntf) func()
)

func initOptional() {
	initOptionalLock.Do(func() {
		optionalComponents = []func(log.Logger, *viper.Viper, busIntf) func(){}
	})
}

func parseConfigFiles(baseFileName string) *viper.Viper {
	cfgMgr := viper.New()
	if err := cfgMgr.BindPFlags(flag.CommandLine); err != nil {
		panic(fmt.Sprintf("Could not bind viper flags: %s", err))
	}
	cfgMgr.SetEnvPrefix("ME") // set up viper env variable mappings
	cfgMgr.AutomaticEnv()     // automatically pull in overrides from env

	// load main config file
	cfgMgr.SetConfigName(baseFileName)
	cfgMgr.AddConfigPath(".")
	cfgMgr.AddConfigPath("/etc/")
	if err := cfgMgr.ReadInConfig(); err != nil {
		panic(fmt.Sprintf("Could not read config file(%s): %s", baseFileName, err))
	}

	// Local config for running from the build tree
	localFileName := "local-" + baseFileName + ".yaml"
	if fileutils.FileExists(localFileName) {
		fmt.Fprintf(os.Stderr, "Reading %s config\n", localFileName)
		cfgMgr.SetConfigFile(localFileName)
		if err := cfgMgr.MergeInConfig(); err != nil {
			panic(fmt.Sprintf("Error reading local config file(%s): %s", localFileName, err))
		}
	}
	return cfgMgr
}

func main() {
	flag.StringSliceP("listen", "l", []string{}, "Listen address.  Formats: (http:[ip]:nn, https:[ip]:port)")
	cfgMgr := parseConfigFiles("metric-engine")
	flag.Parse() // read command line flags after config files

	logger := log15adapter.Make()
	logger.SetupLogHandlersFromConfig(cfgMgr)
	ctx, cancel := context.WithCancel(context.Background())
	intr := make(chan os.Signal, 1)
	signal.Notify(intr, os.Interrupt, syscall.SIGTERM)
	go func() {
		// wait until <CTRL>-C
		<-intr
		logger.Crit("INTERRUPTED, Cancelling...")
		cancel()
	}()

	cfgMgr.SetDefault("main.queuelen", 40)
	cfgMgr.SetDefault("main.queuewarnpct", 80)
	cfgMgr.SetDefault("eemi.msgregpath", "/usr/local/lib/msgreg/MessageRegistry-en.xml.gz")
	d := &busComponents{
		EventBus:       eventbus.NewEventBus(),
		EventPublisher: eventpublisher.NewEventPublisher(),
		EventWaiter: eventwaiter.NewEventWaiter(
			eventwaiter.WithLogger(logger),
			eventwaiter.SetName("Main"),
			eventwaiter.NoAutoRun,
			eventwaiter.QueueLen(cfgMgr.GetInt("main.queuelen")),
			eventwaiter.WarnPct(cfgMgr.GetInt("main.queuewarnpct"))),
	}

	d.EventBus.AddHandler(eh.MatchAny(), d.EventPublisher)
	d.EventPublisher.AddObserver(d.EventWaiter)
	go d.GetWaiter().Run()

	shutdownFns := []func(){}
	for _, startupFunction := range optionalComponents {
		// The startupFunction returns a function to call for shutdown
		// pass all our globally needed stuff to whoever needs it
		shutdownFunction := startupFunction(logger, cfgMgr, d)
		if shutdownFunction == nil {
			continue
		}

		// run shutdown fns in reverse order of startup, so append accordingly
		shutdownFns = append([]func(){shutdownFunction}, shutdownFns...)
	}

	blockUntilExit(ctx) // blocks until the context is cancelled

	for _, fn := range shutdownFns {
		fn()
	}
}

func blockUntilExit(ctx context.Context) {
	// aggressively free memory after working startup events in the background
	time.Sleep(time.Second)
	debug.FreeOSMemory()
	time.Sleep(time.Second)
	debug.FreeOSMemory()
	t := time.Tick(forceGCInterval)
untilExit:
	for {
		select {
		case <-ctx.Done(): // wait until everything is done (CTRL-C or other signal)
			break untilExit
		case <-t:
			debug.FreeOSMemory()
		}
	}
}