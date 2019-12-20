package udb

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jmoiron/sqlx"
	eh "github.com/looplab/eventhorizon"
	"github.com/spf13/viper"
	"github.com/superchalupa/sailfish/src/looplab/event"

	log "github.com/superchalupa/sailfish/src/log"
)

const (
	UDBDatabaseEvent eh.EventType = "UDBDatabaseEvent"
	UDBChangeEvent   eh.EventType = "UDBChangeEvent"
)

type BusComponents interface {
	GetBus() eh.EventBus
}

type EventHandlingService interface {
	AddEventHandler(string, eh.EventType, func(eh.Event))
}

func RegisterAM3(logger log.Logger, cfg *viper.Viper, am3Svc EventHandlingService, d BusComponents) {
	database, err := sqlx.Open("sqlite3", ":memory:")
	if err != nil {
		logger.Crit("Could not open udb database", "err", err)
		return
	}

	// attach UDB db
	attach := "Attach '" + cfg.GetString("main.udbdatabasepath") + "' as udbdm"
	fmt.Println(attach)
	_, err = database.Exec(attach)
	if err != nil {
		logger.Crit("Could not attach UDB database", "attach", attach, "err", err)
		return
	}

	// attach SHM db
	attach = "Attach '" + cfg.GetString("main.shmdatabasepath") + "' as udbsm"
	fmt.Println(attach)
	_, err = database.Exec(attach)
	if err != nil {
		logger.Crit("Could not attach SM database", "attach", attach, "err", err)
		return
	}

	// we have a separate goroutine for this, so we should be safe to busy-wait
	_, err = database.Exec("PRAGMA query_only = 1")
	_, err = database.Exec("PRAGMA busy_timeout = 1000")
	//_, err = database.Exec("PRAGMA cache_size = 0")
	//_, err = database.Exec("PRAGMA udbdm.cache_size = 0")
	//_, err = database.Exec("PRAGMA udbsm.cache_size = 0")
	//_, err = database.Exec("PRAGMA mmap_size = 0")
	_, err = database.Exec("PRAGMA synchronous = off")
	_, err = database.Exec("PRAGMA       journal_mode  = off")
	_, err = database.Exec("PRAGMA udbdm.journal_mode  = off")
	_, err = database.Exec("PRAGMA udbsm.journal_mode  = off")

	// udb db not opened in WAL mode... in fact should be read-only, so this isn't really necessary, but might as well
	database.SetMaxOpenConns(1)

	UDBFactory, err := NewUDBFactory(logger, database, d, cfg)
	if err != nil {
		logger.Crit("Error creating udb integration", "err", err)
		database.Close()
		return
	}

	go handleUDBNotifyPipe(logger, cfg, d)

	// for now, trigger automatic imports on a periodic basis (5s for now, we can up to 1s later to catch power stuff)
	go func() {
		importTicker := time.NewTicker(5 * time.Second)
		defer importTicker.Stop()
		for {
			select {
			case <-importTicker.C:
				d.GetBus().PublishEvent(context.Background(), eh.NewEvent(UDBDatabaseEvent, "periodic_import", time.Now()))
			}
		}
	}()

	// This is the event to trigger UDB imports
	am3Svc.AddEventHandler("Import UDB Metric Values", UDBDatabaseEvent, func(event eh.Event) {
		command, ok := event.Data().(string)
		if !ok {
			logger.Crit("UDB Metric DB message handler got an invalid data event", "event", event, "eventdata", event.Data())
			return
		}

		switch {
		case command == "periodic_import":
			UDBFactory.IterUDBTables(func(name string, meta UDBMeta) error {
				UDBFactory.ConditionalImport(name, meta, true)
				return nil
			})

		default:
			logger.Crit("GOT A COMMAND THAT I CANT HANDLE", "command", command)
		}
	})

	am3Svc.AddEventHandler("UDB Change Notification", UDBChangeEvent, func(event eh.Event) {
		notify, ok := event.Data().(*changeNotify)
		if !ok {
			logger.Crit("UDB Change Notifier message handler got an invalid data event", "event", event, "eventdata", event.Data())
			return
		}
		UDBFactory.DBChanged(strings.ToLower(notify.database), strings.ToLower(notify.table))
	})
}

type changeNotify struct {
	database  string
	table     string
	rowid     int64
	operation int64
}

func handleUDBNotifyPipe(logger log.Logger, cfg *viper.Viper, d BusComponents) {
	pipePath := cfg.GetString("main.udbnotifypipe")

	fmt.Printf("STARTING UDB NOTIFY PIPE HANDLER\n")
	// Data format we get:
	//    DB                      TBL                  ROWID     operationid
	// ||DMLiveObjectDatabase.db|TblNic_Port_Stats_Obj|167445167|23||

	err := syscall.Mkfifo(pipePath, 0660)
	if err != nil && !os.IsExist(err) {
		logger.Warn("Error creating UDB pipe", "err", err)
	}

	file, err := os.OpenFile(pipePath, os.O_CREATE, os.ModeNamedPipe)
	if err != nil {
		logger.Crit("Error opening UDB pipe", "err", err)
	}

	defer file.Close()

	// The reader of the named pipe gets an EOF when the last writer exits to
	// avoid this, we'll simply open it ourselves for writing and never close it.
	// This will ensure the pipe stays around forever without eof.

	nullWriter, err := os.OpenFile(pipePath, os.O_WRONLY, os.ModeNamedPipe)
	if err != nil {
		logger.Crit("Error opening UDB pipe for (placeholder) write", "err", err)
	}

	defer nullWriter.Close()

	n := &changeNotify{}
	split := func(data []byte, atEOF bool) (advance int, token []byte, err error) {
		if atEOF {
			return 0, nil, io.EOF
		}
		start := bytes.Index(data, []byte("||"))
		if start == -1 {
			// didnt find starting ||, skip over everything
			return len(data), nil, nil
		}

		end := bytes.Index(data[start+2:], []byte("||"))
		if end == -1 {
			// didnt find ending ||
			return 0, nil, nil
		}

		// adjust 'end' here to take into account that we indexed off the start+2
		// of the data array
		fields := bytes.Split(data[start+2:end+start+2], []byte("|"))
		if len(fields) != 4 {
			n.database = ""
			n.table = ""
			n.rowid = 0
			n.operation = 0
			// skip over starting || plus any intervening data, leave the trailing || as potential start of next record
			return start + end + 2, []byte("s"), nil
		}

		n.database = string(fields[0])
		n.table = string(fields[1])
		n.rowid, _ = strconv.ParseInt(string(fields[2]), 10, 64)
		n.operation, _ = strconv.ParseInt(string(fields[3]), 10, 64)

		// consume the whole thing
		return start + 2 + end + 2, []byte("t"), nil
	}

	s := bufio.NewScanner(file)
	s.Split(split)
	for s.Scan() {
		if s.Text() == "t" {
			// publish change notification
			evt := event.NewSyncEvent(UDBChangeEvent, n, time.Now())
			evt.Add(1)
			d.GetBus().PublishEvent(context.Background(), evt)
			evt.Wait()
			// new struct for the next notify so we dont have data races while other goroutines process the struct above
			n = &changeNotify{}
		}
	}
	return
}
