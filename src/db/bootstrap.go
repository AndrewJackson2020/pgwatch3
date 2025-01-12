package db

import (
	"context"
	"time"

	"github.com/cybertec-postgresql/pgwatch3/log"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/tracelog"
	retry "github.com/sethvargo/go-retry"
)

const (
	pgConnRecycleSeconds = 1800       // applies for monitored nodes
	applicationName      = "pgwatch3" // will be set on all opened PG connections for informative purposes
)

func TryDatabaseConnection(ctx context.Context, connStr string) error {
	c, err := pgx.Connect(ctx, connStr)
	if c != nil {
		_ = c.Close(ctx)
	}
	return err
}

type ConnConfigCallback = func(*pgxpool.Config) error

func GetPostgresDBConnection(ctx context.Context, connStr string, callbacks ...ConnConfigCallback) (PgxPoolIface, error) {
	connConfig, err := pgxpool.ParseConfig(connStr)
	if err != nil {
		return nil, err
	}
	for _, f := range callbacks {
		if err = f(connConfig); err != nil {
			return nil, err
		}
	}
	if connConfig.ConnConfig.ConnectTimeout == 0 {
		connConfig.ConnConfig.ConnectTimeout = time.Second * 5
	}
	connConfig.MaxConnIdleTime = 15 * time.Second
	connConfig.MaxConnLifetime = pgConnRecycleSeconds * time.Second
	tracelogger := &tracelog.TraceLog{
		Logger:   log.NewPgxLogger(log.GetLogger(ctx)),
		LogLevel: tracelog.LogLevelDebug, //map[bool]tracelog.LogLevel{false: tracelog.LogLevelWarn, true: tracelog.LogLevelDebug}[true],
	}
	connConfig.ConnConfig.Tracer = tracelogger
	return pgxpool.NewWithConfig(ctx, connConfig)
}

var backoff = retry.WithMaxRetries(3, retry.NewConstant(1*time.Second))

func InitAndTestConfigStoreConnection(ctx context.Context, connStr string) (configDb PgxPoolIface, err error) {
	logger := log.GetLogger(ctx)
	if err = retry.Do(ctx, backoff, func(ctx context.Context) error {
		if configDb, err = GetPostgresDBConnection(ctx, connStr); err == nil {
			err = configDb.Ping(ctx)
		}
		if err != nil {
			logger.WithError(err).Error("Connection failed")
			logger.Info("Sleeping before reconnecting...")
			return retry.RetryableError(err)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	err = ExecuteConfigSchemaScripts(ctx, configDb)
	return
}

func InitAndTestMetricStoreConnection(ctx context.Context, connStr string) (metricDb PgxPoolIface, err error) {
	logger := log.GetLogger(ctx)
	if err = retry.Do(ctx, backoff, func(ctx context.Context) error {
		if metricDb, err = GetPostgresDBConnection(ctx, connStr); err == nil {
			err = metricDb.Ping(ctx)
		}
		if err != nil {
			logger.WithError(err).Error("Connection failed")
			logger.Info("Sleeping before reconnecting...")
			return retry.RetryableError(err)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	err = ExecuteMetricSchemaScripts(ctx, metricDb)
	return
}

var (
	configSchemaSQLs = []string{
		sqlConfigSchema,
		sqlConfigDefinitions,
	}
	metricSchemaSQLs = []string{
		sqlMetricAdminSchema,
		sqlMetricAdminFunctions,
		sqlMetricEnsurePartitionPostgres,
		sqlMetricEnsurePartitionTimescale,
		sqlMetricChangeChunkIntervalTimescale,
		sqlMetricChangeCompressionIntervalTimescale,
	}
)

func ExecuteConfigSchemaScripts(ctx context.Context, conn PgxIface) error {
	log.GetLogger(ctx).Info("Executing configuration schema scripts: ", len(configSchemaSQLs))
	return executeSchemaScripts(ctx, conn, "pgwatch3", configSchemaSQLs)
}

func ExecuteMetricSchemaScripts(ctx context.Context, conn PgxIface) error {
	log.GetLogger(ctx).Info("Executing metric storage schema scripts: ", len(metricSchemaSQLs))
	return executeSchemaScripts(ctx, conn, "admin", metricSchemaSQLs)
}

// executeSchemaScripts executes initial schema scripts
func executeSchemaScripts(ctx context.Context, conn PgxIface, schema string, sqls []string) (err error) {
	var exists bool
	sqlSchemaExists := "SELECT EXISTS(SELECT 1 FROM pg_namespace WHERE nspname = $1)"
	err = conn.QueryRow(ctx, sqlSchemaExists, schema).Scan(&exists)
	if err != nil || exists {
		return
	}
	for _, sql := range sqls {
		if _, err = conn.Exec(ctx, sql); err != nil {
			return err
		}
	}
	return nil
}

func GetTableColumns(ctx context.Context, conn PgxIface, table string) (cols []string, err error) {
	sql := `SELECT attname FROM pg_attribute a WHERE a.attrelid = to_regclass($1) and a.attnum > 0 and not a.attisdropped`
	r, err := conn.Query(ctx, sql, table)
	if err != nil {
		return
	}
	return pgx.CollectRows(r, pgx.RowTo[string])
}
