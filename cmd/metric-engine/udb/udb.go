package udb

import (
	"strings"

	"github.com/jmoiron/sqlx"
	"github.com/spf13/viper"
	"golang.org/x/xerrors"

	log "github.com/superchalupa/sailfish/src/log"
)

type constErr string

func (e constErr) Error() string { return string(e) }

const disabled = constErr("importer disabled")
const stopIter = constErr("stop iteration")

type DataImporter interface {
	PeriodicImport(bool) error
	ProcessDBChange(string, string) error
}

type importManager map[string]DataImporter

func newImportManager(logger log.Logger, database *sqlx.DB, d busComponents, cfg *viper.Viper) (*importManager, error) {
	ret := importManager{}

	// trigger to disable udb setup
	subcfgskip := cfg.GetString("UDBmetricreport")
	if strings.Contains(subcfgskip, "disable") {
		logger.Crit("DISABLE UDB SETUP")
		return &ret, nil
	}

	// Parse the YAML file to set up database imports
	subcfg := cfg.Sub("UDB-Metric-Import")
	if subcfg == nil {
		return nil, xerrors.Errorf("config file parse error. missing secion 'UDB-Metric-Import'")
	}

	createFns := map[string]func(logger log.Logger, DB *sqlx.DB, d busComponents, cfg *viper.Viper, n string) (DataImporter, error){
		"DISABLED":      newDisabledSource,
		"DirectMetric":  newDirectMetric,
		"MetricColumns": newImportByColumn,
	}

	for k, settings := range subcfg.AllSettings() {
		logger.Info("Loading Import Config", "section", k)
		// if this panics, the config is messed up. Not going to protect against malformed config here.
		fn, ok := createFns[settings.(map[string]interface{})["type"].(string)]
		if !ok {
			logger.Warn("Import function type not found", "type", settings.(map[string]interface{})["type"].(string))
			continue
		}
		meta, err := fn(logger, database, d, subcfg.Sub(k), k)
		if err != nil {
			return nil, xerrors.Errorf("failed to parse config section(UDB-Metric-Import.%s): %s", k, err)
		}

		ret[k] = meta
	}

	return &ret, nil
}

func (impMgr *importManager) runPeriodicImports(periodic bool) error {
	// TODO: get smarter about this. We ought to calculate time until next report and set a timer for that
	return impMgr.iterUDBSources(func(name string, src DataImporter) error {
		return src.PeriodicImport(periodic)
	})
}

func (impMgr *importManager) runUDBChangeImports(database, table string) (err error) {
	return impMgr.iterUDBSources(func(udbImportName string, src DataImporter) error {
		return src.ProcessDBChange(database, table)
	})
}

func (impMgr *importManager) iterUDBSources(fn func(string, DataImporter) error) error {
	for udbImportName, source := range *impMgr {
		err := fn(udbImportName, source)
		if err != nil && err != disabled && err != stopIter {
			return xerrors.Errorf("Stopped iter at source (%s): %w", source, err)
		}
	}
	return nil
}