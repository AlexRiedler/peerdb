package connpostgres

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/log"

	"github.com/PeerDB-io/peer-flow/connectors/utils"
	"github.com/PeerDB-io/peer-flow/geo"
	"github.com/PeerDB-io/peer-flow/model"
	"github.com/PeerDB-io/peer-flow/model/numeric"
	"github.com/PeerDB-io/peer-flow/model/qvalue"
	"github.com/PeerDB-io/peer-flow/shared"
)

type QRepQueryExecutor struct {
	*PostgresConnector
	snapshot    string
	testEnv     bool
	flowJobName string
	partitionID string
	logger      log.Logger
}

func (c *PostgresConnector) NewQRepQueryExecutor(flowJobName string, partitionID string) *QRepQueryExecutor {
	return &QRepQueryExecutor{
		PostgresConnector: c,
		snapshot:          "",
		flowJobName:       flowJobName,
		partitionID:       partitionID,
		logger:            log.With(c.logger, slog.String(string(shared.PartitionIDKey), partitionID)),
	}
}

func (c *PostgresConnector) NewQRepQueryExecutorSnapshot(snapshot string, flowJobName string, partitionID string) *QRepQueryExecutor {
	return &QRepQueryExecutor{
		PostgresConnector: c,
		snapshot:          snapshot,
		flowJobName:       flowJobName,
		partitionID:       partitionID,
		logger:            log.With(c.logger, slog.String(string(shared.PartitionIDKey), partitionID)),
	}
}

func (qe *QRepQueryExecutor) SetTestEnv(testEnv bool) {
	qe.testEnv = testEnv
}

func (qe *QRepQueryExecutor) ExecuteQuery(ctx context.Context, query string, args ...interface{}) (pgx.Rows, error) {
	rows, err := qe.conn.Query(ctx, query, args...)
	if err != nil {
		qe.logger.Error("[pg_query_executor] failed to execute query", slog.Any("error", err))
		return nil, err
	}
	return rows, nil
}

func (qe *QRepQueryExecutor) executeQueryInTx(ctx context.Context, tx pgx.Tx, cursorName string, fetchSize int) (pgx.Rows, error) {
	qe.logger.Info("Executing query in transaction")
	q := fmt.Sprintf("FETCH %d FROM %s", fetchSize, cursorName)

	if !qe.testEnv {
		shutdown := utils.HeartbeatRoutine(ctx, func() string {
			qe.logger.Info(fmt.Sprintf("still running '%s'...", q))
			return fmt.Sprintf("running '%s'", q)
		})
		defer shutdown()
	}

	rows, err := tx.Query(ctx, q)
	if err != nil {
		qe.logger.Error("[pg_query_executor] failed to execute query in tx", slog.Any("error", err))
		return nil, err
	}

	return rows, nil
}

// FieldDescriptionsToSchema converts a slice of pgconn.FieldDescription to a QRecordSchema.
func (qe *QRepQueryExecutor) fieldDescriptionsToSchema(fds []pgconn.FieldDescription) *model.QRecordSchema {
	qfields := make([]model.QField, len(fds))
	for i, fd := range fds {
		cname := fd.Name
		ctype := qe.postgresOIDToQValueKind(fd.DataTypeOID)
		if ctype == qvalue.QValueKindInvalid {
			typeName, ok := qe.customTypesMapping[fd.DataTypeOID]
			if ok {
				ctype = customTypeToQKind(typeName)
			} else {
				ctype = qvalue.QValueKindString
			}
		}
		// there isn't a way to know if a column is nullable or not
		// TODO fix this.
		cnullable := true
		if ctype == qvalue.QValueKindNumeric {
			precision, scale := numeric.ParseNumericTypmod(fd.TypeModifier)
			qfields[i] = model.QField{
				Name:      cname,
				Type:      ctype,
				Nullable:  cnullable,
				Precision: precision,
				Scale:     scale,
			}
		} else {
			qfields[i] = model.QField{
				Name:     cname,
				Type:     ctype,
				Nullable: cnullable,
			}
		}
	}
	return model.NewQRecordSchema(qfields)
}

func (qe *QRepQueryExecutor) ProcessRows(
	rows pgx.Rows,
	fieldDescriptions []pgconn.FieldDescription,
) (*model.QRecordBatch, error) {
	// Initialize the record slice
	records := make([][]qvalue.QValue, 0)
	qe.logger.Info("Processing rows")
	// Iterate over the rows
	for rows.Next() {
		record, err := qe.mapRowToQRecord(rows, fieldDescriptions)
		if err != nil {
			qe.logger.Error("[pg_query_executor] failed to map row to QRecord", slog.Any("error", err))
			return nil, fmt.Errorf("failed to map row to QRecord: %w", err)
		}
		records = append(records, record)
	}

	// Check for any errors encountered during iteration
	if rows.Err() != nil {
		return nil, fmt.Errorf("row iteration failed: %w", rows.Err())
	}

	batch := &model.QRecordBatch{
		Records: records,
		Schema:  qe.fieldDescriptionsToSchema(fieldDescriptions),
	}

	qe.logger.Info(fmt.Sprintf("[postgres] pulled %d records", len(batch.Records)))

	return batch, nil
}

func (qe *QRepQueryExecutor) processRowsStream(
	ctx context.Context,
	cursorName string,
	stream *model.QRecordStream,
	rows pgx.Rows,
	fieldDescriptions []pgconn.FieldDescription,
) (int, error) {
	numRows := 0
	const heartBeatNumRows = 5000

	// Iterate over the rows
	for rows.Next() {
		select {
		case <-ctx.Done():
			qe.logger.Info("Context canceled, exiting processRowsStream early")
			return numRows, ctx.Err()
		default:
			// Process the row as before
			record, err := qe.mapRowToQRecord(rows, fieldDescriptions)
			if err != nil {
				qe.logger.Error("[pg_query_executor] failed to map row to QRecord", slog.Any("error", err))
				stream.Records <- model.QRecordOrError{
					Err: fmt.Errorf("failed to map row to QRecord: %w", err),
				}
				return 0, fmt.Errorf("failed to map row to QRecord: %w", err)
			}

			stream.Records <- model.QRecordOrError{
				Record: record,
				Err:    nil,
			}

			if numRows%heartBeatNumRows == 0 {
				qe.recordHeartbeat(ctx, "cursor: %s - fetched %d records", cursorName, numRows)
			}

			numRows++
		}
	}

	qe.recordHeartbeat(ctx, "cursor %s - fetch completed - %d records", cursorName, numRows)
	qe.logger.Info("processed row stream")
	return numRows, nil
}

func (qe *QRepQueryExecutor) recordHeartbeat(ctx context.Context, x string, args ...interface{}) {
	if qe.testEnv {
		qe.logger.Info(fmt.Sprintf(x, args...))
		return
	}
	msg := fmt.Sprintf(x, args...)
	activity.RecordHeartbeat(ctx, msg)
}

func (qe *QRepQueryExecutor) processFetchedRows(
	ctx context.Context,
	query string,
	tx pgx.Tx,
	cursorName string,
	fetchSize int,
	stream *model.QRecordStream,
) (int, error) {
	rows, err := qe.executeQueryInTx(ctx, tx, cursorName, fetchSize)
	if err != nil {
		stream.Records <- model.QRecordOrError{
			Err: err,
		}
		qe.logger.Error("[pg_query_executor] failed to execute query in tx",
			slog.Any("error", err), slog.String("query", query))
		return 0, fmt.Errorf("[pg_query_executor] failed to execute query in tx: %w", err)
	}

	defer rows.Close()

	fieldDescriptions := rows.FieldDescriptions()
	if !stream.IsSchemaSet() {
		schema := qe.fieldDescriptionsToSchema(fieldDescriptions)
		_ = stream.SetSchema(schema)
	}

	numRows, err := qe.processRowsStream(ctx, cursorName, stream, rows, fieldDescriptions)
	if err != nil {
		qe.logger.Error("[pg_query_executor] failed to process rows", slog.Any("error", err))
		return 0, fmt.Errorf("failed to process rows: %w", err)
	}

	if rows.Err() != nil {
		stream.Records <- model.QRecordOrError{
			Err: rows.Err(),
		}
		qe.logger.Error("[pg_query_executor] row iteration failed",
			slog.String("query", query), slog.Any("error", rows.Err()))
		return 0, fmt.Errorf("[pg_query_executor] row iteration failed '%s': %w", query, rows.Err())
	}

	return numRows, nil
}

func (qe *QRepQueryExecutor) ExecuteAndProcessQuery(
	ctx context.Context,
	query string,
	args ...interface{},
) (*model.QRecordBatch, error) {
	stream := model.NewQRecordStream(1024)
	errors := make(chan error, 1)
	qe.logger.Info("Executing and processing query", slog.String("query", query))

	// must wait on errors to close before returning to maintain qe.conn exclusion
	go func() {
		defer close(errors)
		_, err := qe.ExecuteAndProcessQueryStream(ctx, stream, query, args...)
		if err != nil {
			qe.logger.Error("[pg_query_executor] failed to execute and process query stream", slog.Any("error", err))
			errors <- err
		}
	}()

	select {
	case err := <-errors:
		return nil, err
	case schema := <-stream.SchemaChan():
		if schema.Err != nil {
			qe.logger.Error("[pg_query_executor] failed to get schema from stream", slog.Any("error", schema.Err))
			<-errors
			return nil, fmt.Errorf("failed to get schema from stream: %w", schema.Err)
		}
		batch := &model.QRecordBatch{
			Records: make([][]qvalue.QValue, 0),
			Schema:  schema.Schema,
		}
		for record := range stream.Records {
			if record.Err == nil {
				batch.Records = append(batch.Records, record.Record)
			} else {
				<-errors
				return nil, fmt.Errorf("[pg] failed to get record from stream: %w", record.Err)
			}
		}
		<-errors
		return batch, nil
	}
}

func (qe *QRepQueryExecutor) ExecuteAndProcessQueryStream(
	ctx context.Context,
	stream *model.QRecordStream,
	query string,
	args ...interface{},
) (int, error) {
	qe.logger.Info("Executing and processing query stream", slog.String("query", query))
	defer close(stream.Records)

	tx, err := qe.conn.BeginTx(ctx, pgx.TxOptions{
		AccessMode: pgx.ReadOnly,
		IsoLevel:   pgx.RepeatableRead,
	})
	if err != nil {
		qe.logger.Error("[pg_query_executor] failed to begin transaction", slog.Any("error", err))
		return 0, fmt.Errorf("[pg_query_executor] failed to begin transaction: %w", err)
	}

	totalRecordsFetched, err := qe.ExecuteAndProcessQueryStreamWithTx(ctx, tx, stream, query, args...)
	return totalRecordsFetched, err
}

func (qe *QRepQueryExecutor) ExecuteAndProcessQueryStreamGettingCurrentSnapshotXmin(
	ctx context.Context,
	stream *model.QRecordStream,
	query string,
	args ...interface{},
) (int, int64, error) {
	var currentSnapshotXmin pgtype.Int8
	qe.logger.Info("Executing and processing query stream", slog.String("query", query))
	defer close(stream.Records)

	tx, err := qe.conn.BeginTx(ctx, pgx.TxOptions{
		AccessMode: pgx.ReadOnly,
		IsoLevel:   pgx.RepeatableRead,
	})
	if err != nil {
		qe.logger.Error("[pg_query_executor] failed to begin transaction", slog.Any("error", err))
		return 0, currentSnapshotXmin.Int64, fmt.Errorf("[pg_query_executor] failed to begin transaction: %w", err)
	}

	err = tx.QueryRow(ctx, "select txid_snapshot_xmin(txid_current_snapshot())").Scan(&currentSnapshotXmin)
	if err != nil {
		qe.logger.Error("[pg_query_executor] failed to get current snapshot xmin", slog.Any("error", err))
		return 0, currentSnapshotXmin.Int64, err
	}

	totalRecordsFetched, err := qe.ExecuteAndProcessQueryStreamWithTx(ctx, tx, stream, query, args...)
	return totalRecordsFetched, currentSnapshotXmin.Int64, err
}

func (qe *QRepQueryExecutor) ExecuteAndProcessQueryStreamWithTx(
	ctx context.Context,
	tx pgx.Tx,
	stream *model.QRecordStream,
	query string,
	args ...interface{},
) (int, error) {
	var err error

	defer func() {
		err := tx.Rollback(ctx)
		if err != nil && err != pgx.ErrTxClosed {
			qe.logger.Error("[pg_query_executor] failed to rollback transaction", slog.Any("error", err))
		}
	}()

	if qe.snapshot != "" {
		_, err = tx.Exec(ctx, "SET TRANSACTION SNAPSHOT "+QuoteLiteral(qe.snapshot))
		if err != nil {
			stream.Records <- model.QRecordOrError{
				Err: fmt.Errorf("failed to set snapshot: %w", err),
			}
			qe.logger.Error("[pg_query_executor] failed to set snapshot",
				slog.Any("error", err), slog.String("query", query))
			return 0, fmt.Errorf("[pg_query_executor] failed to set snapshot: %w", err)
		}
	}

	randomUint, err := shared.RandomUInt64()
	if err != nil {
		qe.logger.Error("[pg_query_executor] failed to generate random uint", slog.Any("error", err))
		stream.Records <- model.QRecordOrError{
			Err: fmt.Errorf("failed to generate random uint: %w", err),
		}
		return 0, fmt.Errorf("[pg_query_executor] failed to generate random uint: %w", err)
	}

	cursorName := fmt.Sprintf("peerdb_cursor_%d", randomUint)
	fetchSize := shared.FetchAndChannelSize
	cursorQuery := fmt.Sprintf("DECLARE %s CURSOR FOR %s", cursorName, query)
	qe.logger.Info(fmt.Sprintf("[pg_query_executor] executing cursor declaration for %v with args %v", cursorQuery, args))
	_, err = tx.Exec(ctx, cursorQuery, args...)
	if err != nil {
		stream.Records <- model.QRecordOrError{
			Err: fmt.Errorf("failed to declare cursor: %w", err),
		}
		qe.logger.Info("[pg_query_executor] failed to declare cursor",
			slog.String("cursorQuery", cursorQuery), slog.Any("error", err))
		return 0, fmt.Errorf("[pg_query_executor] failed to declare cursor: %w", err)
	}

	qe.logger.Info(fmt.Sprintf("[pg_query_executor] declared cursor '%s' for query '%s'", cursorName, query))

	totalRecordsFetched := 0
	numFetchOpsComplete := 0
	for {
		numRows, err := qe.processFetchedRows(ctx, query, tx, cursorName, fetchSize, stream)
		if err != nil {
			qe.logger.Error("[pg_query_executor] failed to process fetched rows", slog.Any("error", err))
			return 0, err
		}

		qe.logger.Info(fmt.Sprintf("[pg_query_executor] fetched %d rows for query '%s'", numRows, query))
		totalRecordsFetched += numRows

		if numRows == 0 {
			break
		}

		numFetchOpsComplete += 1
		qe.recordHeartbeat(ctx, "#%d fetched %d rows", numFetchOpsComplete, numRows)
	}

	qe.logger.Info("Committing transaction")
	err = tx.Commit(ctx)
	if err != nil {
		qe.logger.Error("[pg_query_executor] failed to commit transaction", slog.Any("error", err))
		stream.Records <- model.QRecordOrError{
			Err: fmt.Errorf("failed to commit transaction: %w", err),
		}
		return 0, fmt.Errorf("[pg_query_executor] failed to commit transaction: %w", err)
	}

	qe.logger.Info(fmt.Sprintf("[pg_query_executor] committed transaction for query '%s', rows = %d",
		query, totalRecordsFetched))
	return totalRecordsFetched, nil
}

func (qe *QRepQueryExecutor) mapRowToQRecord(
	row pgx.Rows,
	fds []pgconn.FieldDescription,
) ([]qvalue.QValue, error) {
	// make vals an empty array of QValue of size len(fds)
	record := make([]qvalue.QValue, len(fds))

	values, err := row.Values()
	if err != nil {
		qe.logger.Error("[pg_query_executor] failed to get values from row", slog.Any("error", err))
		return nil, fmt.Errorf("failed to scan row: %w", err)
	}

	for i, fd := range fds {
		// Check if it's a custom type first
		typeName, ok := qe.customTypesMapping[fd.DataTypeOID]
		if !ok {
			tmp, err := qe.parseFieldFromPostgresOID(fd.DataTypeOID, values[i])
			if err != nil {
				qe.logger.Error("[pg_query_executor] failed to parse field", slog.Any("error", err))
				return nil, fmt.Errorf("failed to parse field: %w", err)
			}
			record[i] = tmp
		} else {
			customQKind := customTypeToQKind(typeName)
			if customQKind == qvalue.QValueKindGeography || customQKind == qvalue.QValueKindGeometry {
				wkbString, ok := values[i].(string)
				wkt, err := geo.GeoValidate(wkbString)
				if err != nil || !ok {
					values[i] = nil
				} else {
					values[i] = wkt
				}
			}
			record[i] = qvalue.QValue{
				Kind:  customQKind,
				Value: values[i],
			}
		}
	}

	return record, nil
}
