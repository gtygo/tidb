// Copyright 2013 The ql Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSES/QL-LICENSE file.

// Copyright 2015 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package session

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ngaut/pools"
	"github.com/opentracing/opentracing-go"
	"github.com/pingcap/errors"
	"github.com/pingcap/failpoint"
	"github.com/pingcap/parser"
	"github.com/pingcap/parser/ast"
	"github.com/pingcap/parser/auth"
	"github.com/pingcap/parser/charset"
	"github.com/pingcap/parser/model"
	"github.com/pingcap/parser/mysql"
	"github.com/pingcap/parser/terror"
	"github.com/pingcap/tidb/bindinfo"
	"github.com/pingcap/tidb/config"
	"github.com/pingcap/tidb/domain"
	"github.com/pingcap/tidb/executor"
	"github.com/pingcap/tidb/kv"
	"github.com/pingcap/tidb/meta"
	"github.com/pingcap/tidb/metrics"
	"github.com/pingcap/tidb/owner"
	"github.com/pingcap/tidb/planner"
	plannercore "github.com/pingcap/tidb/planner/core"
	"github.com/pingcap/tidb/plugin"
	"github.com/pingcap/tidb/privilege"
	"github.com/pingcap/tidb/privilege/privileges"
	"github.com/pingcap/tidb/sessionctx"
	"github.com/pingcap/tidb/sessionctx/binloginfo"
	"github.com/pingcap/tidb/sessionctx/stmtctx"
	"github.com/pingcap/tidb/sessionctx/variable"
	"github.com/pingcap/tidb/statistics/handle"
	"github.com/pingcap/tidb/store/tikv"
	"github.com/pingcap/tidb/types"
	"github.com/pingcap/tidb/util"
	"github.com/pingcap/tidb/util/chunk"
	"github.com/pingcap/tidb/util/execdetails"
	"github.com/pingcap/tidb/util/kvcache"
	"github.com/pingcap/tidb/util/logutil"
	"github.com/pingcap/tidb/util/sqlexec"
	"github.com/pingcap/tidb/util/timeutil"
	"github.com/pingcap/tipb/go-binlog"
	"go.uber.org/zap"
)

var (
	statementPerTransactionInternalOK    = metrics.StatementPerTransaction.WithLabelValues(metrics.LblInternal, "ok")
	statementPerTransactionInternalError = metrics.StatementPerTransaction.WithLabelValues(metrics.LblInternal, "error")
	statementPerTransactionGeneralOK     = metrics.StatementPerTransaction.WithLabelValues(metrics.LblGeneral, "ok")
	statementPerTransactionGeneralError  = metrics.StatementPerTransaction.WithLabelValues(metrics.LblGeneral, "error")
	transactionDurationInternalOK        = metrics.TransactionDuration.WithLabelValues(metrics.LblInternal, "ok")
	transactionDurationInternalError     = metrics.TransactionDuration.WithLabelValues(metrics.LblInternal, "error")
	transactionDurationGeneralOK         = metrics.TransactionDuration.WithLabelValues(metrics.LblGeneral, "ok")
	transactionDurationGeneralError      = metrics.TransactionDuration.WithLabelValues(metrics.LblGeneral, "error")

	transactionCounterInternalOK             = metrics.TransactionCounter.WithLabelValues(metrics.LblInternal, metrics.LblOK)
	transactionCounterInternalErr            = metrics.TransactionCounter.WithLabelValues(metrics.LblInternal, metrics.LblError)
	transactionCounterGeneralOK              = metrics.TransactionCounter.WithLabelValues(metrics.LblGeneral, metrics.LblOK)
	transactionCounterGeneralErr             = metrics.TransactionCounter.WithLabelValues(metrics.LblGeneral, metrics.LblError)
	transactionCounterInternalCommitRollback = metrics.TransactionCounter.WithLabelValues(metrics.LblInternal, metrics.LblComRol)
	transactionCounterGeneralCommitRollback  = metrics.TransactionCounter.WithLabelValues(metrics.LblGeneral, metrics.LblComRol)
	transactionRollbackCounterInternal       = metrics.TransactionCounter.WithLabelValues(metrics.LblInternal, metrics.LblRollback)
	transactionRollbackCounterGeneral        = metrics.TransactionCounter.WithLabelValues(metrics.LblGeneral, metrics.LblRollback)

	sessionExecuteRunDurationInternal = metrics.SessionExecuteRunDuration.WithLabelValues(metrics.LblInternal)
	sessionExecuteRunDurationGeneral  = metrics.SessionExecuteRunDuration.WithLabelValues(metrics.LblGeneral)

	sessionExecuteCompileDurationInternal = metrics.SessionExecuteCompileDuration.WithLabelValues(metrics.LblInternal)
	sessionExecuteCompileDurationGeneral  = metrics.SessionExecuteCompileDuration.WithLabelValues(metrics.LblGeneral)
	sessionExecuteParseDurationInternal   = metrics.SessionExecuteParseDuration.WithLabelValues(metrics.LblInternal)
	sessionExecuteParseDurationGeneral    = metrics.SessionExecuteParseDuration.WithLabelValues(metrics.LblGeneral)
)

// Session context, it is consistent with the lifecycle of a client connection.
type Session interface {
	sessionctx.Context
	Status() uint16                                               // Flag of current status, such as autocommit.
	LastInsertID() uint64                                         // LastInsertID is the last inserted auto_increment ID.
	LastMessage() string                                          // LastMessage is the info message that may be generated by last command
	AffectedRows() uint64                                         // Affected rows by latest executed stmt.
	Execute(context.Context, string) ([]sqlexec.RecordSet, error) // Execute a sql statement.
	String() string                                               // String is used to debug.
	CommitTxn(context.Context) error
	RollbackTxn(context.Context)
	// PrepareStmt executes prepare statement in binary protocol.
	PrepareStmt(sql string) (stmtID uint32, paramCount int, fields []*ast.ResultField, err error)
	// ExecutePreparedStmt executes a prepared statement.
	ExecutePreparedStmt(ctx context.Context, stmtID uint32, param []types.Datum) (sqlexec.RecordSet, error)
	DropPreparedStmt(stmtID uint32) error
	SetClientCapability(uint32) // Set client capability flags.
	SetConnectionID(uint64)
	SetCommandValue(byte)
	SetProcessInfo(string, time.Time, byte, uint64)
	SetTLSState(*tls.ConnectionState)
	SetCollation(coID int) error
	SetSessionManager(util.SessionManager)
	Close()
	Auth(user *auth.UserIdentity, auth []byte, salt []byte) bool
	ShowProcess() *util.ProcessInfo
	// PrePareTxnCtx is exported for test.
	PrepareTxnCtx(context.Context)
	// FieldList returns fields list of a table.
	FieldList(tableName string) (fields []*ast.ResultField, err error)
}

var (
	_ Session = (*session)(nil)
)

type stmtRecord struct {
	st      sqlexec.Statement
	stmtCtx *stmtctx.StatementContext
}

// StmtHistory holds all histories of statements in a txn.
type StmtHistory struct {
	history []*stmtRecord
}

// Add appends a stmt to history list.
func (h *StmtHistory) Add(st sqlexec.Statement, stmtCtx *stmtctx.StatementContext) {
	s := &stmtRecord{
		st:      st,
		stmtCtx: stmtCtx,
	}
	h.history = append(h.history, s)
}

// Count returns the count of the history.
func (h *StmtHistory) Count() int {
	return len(h.history)
}

type session struct {
	// processInfo is used by ShowProcess(), and should be modified atomically.
	processInfo atomic.Value
	txn         TxnState

	mu struct {
		sync.RWMutex
		values map[fmt.Stringer]interface{}
	}

	currentPlan plannercore.Plan

	store kv.Storage

	parser *parser.Parser

	preparedPlanCache *kvcache.SimpleLRUCache

	sessionVars    *variable.SessionVars
	sessionManager util.SessionManager

	statsCollector *handle.SessionStatsCollector
	// ddlOwnerChecker is used in `select tidb_is_ddl_owner()` statement;
	ddlOwnerChecker owner.DDLOwnerChecker
	// lockedTables use to record the table locks hold by the session.
	lockedTables map[int64]model.TableLockTpInfo

	// shared coprocessor client per session
	client kv.Client
}

// AddTableLock adds table lock to the session lock map.
func (s *session) AddTableLock(locks []model.TableLockTpInfo) {
	for _, l := range locks {
		s.lockedTables[l.TableID] = l
	}
}

// ReleaseTableLocks releases table lock in the session lock map.
func (s *session) ReleaseTableLocks(locks []model.TableLockTpInfo) {
	for _, l := range locks {
		delete(s.lockedTables, l.TableID)
	}
}

// ReleaseTableLockByTableIDs releases table lock in the session lock map by table ID.
func (s *session) ReleaseTableLockByTableIDs(tableIDs []int64) {
	for _, tblID := range tableIDs {
		delete(s.lockedTables, tblID)
	}
}

// CheckTableLocked checks the table lock.
func (s *session) CheckTableLocked(tblID int64) (bool, model.TableLockType) {
	lt, ok := s.lockedTables[tblID]
	if !ok {
		return false, model.TableLockNone
	}
	return true, lt.Tp
}

// GetAllTableLocks gets all table locks table id and db id hold by the session.
func (s *session) GetAllTableLocks() []model.TableLockTpInfo {
	lockTpInfo := make([]model.TableLockTpInfo, 0, len(s.lockedTables))
	for _, tl := range s.lockedTables {
		lockTpInfo = append(lockTpInfo, tl)
	}
	return lockTpInfo
}

// HasLockedTables uses to check whether this session locked any tables.
// If so, the session can only visit the table which locked by self.
func (s *session) HasLockedTables() bool {
	b := len(s.lockedTables) > 0
	return b
}

// ReleaseAllTableLocks releases all table locks hold by the session.
func (s *session) ReleaseAllTableLocks() {
	s.lockedTables = make(map[int64]model.TableLockTpInfo)
}

// DDLOwnerChecker returns s.ddlOwnerChecker.
func (s *session) DDLOwnerChecker() owner.DDLOwnerChecker {
	return s.ddlOwnerChecker
}

func (s *session) getMembufCap() int {
	return kv.DefaultTxnMembufCap
}

func (s *session) cleanRetryInfo() {
	if s.sessionVars.RetryInfo.Retrying {
		return
	}

	retryInfo := s.sessionVars.RetryInfo
	defer retryInfo.Clean()
	if len(retryInfo.DroppedPreparedStmtIDs) == 0 {
		return
	}

	planCacheEnabled := plannercore.PreparedPlanCacheEnabled()
	var cacheKey kvcache.Key
	var preparedAst *ast.Prepared
	if planCacheEnabled {
		firstStmtID := retryInfo.DroppedPreparedStmtIDs[0]
		if preparedPointer, ok := s.sessionVars.PreparedStmts[firstStmtID]; ok {
			preparedObj, ok := preparedPointer.(*plannercore.CachedPrepareStmt)
			if ok {
				preparedAst = preparedObj.PreparedAst
				cacheKey = plannercore.NewPSTMTPlanCacheKey(s.sessionVars, firstStmtID, preparedAst.SchemaVersion)
			}
		}
	}
	for i, stmtID := range retryInfo.DroppedPreparedStmtIDs {
		if planCacheEnabled {
			if i > 0 && preparedAst != nil {
				plannercore.SetPstmtIDSchemaVersion(cacheKey, stmtID, preparedAst.SchemaVersion)
			}
			s.PreparedPlanCache().Delete(cacheKey)
		}
		s.sessionVars.RemovePreparedStmt(stmtID)
	}
}

func (s *session) Status() uint16 {
	return s.sessionVars.Status
}

func (s *session) LastInsertID() uint64 {
	if s.sessionVars.StmtCtx.LastInsertID > 0 {
		return s.sessionVars.StmtCtx.LastInsertID
	}
	return s.sessionVars.StmtCtx.InsertID
}

func (s *session) LastMessage() string {
	return s.sessionVars.StmtCtx.GetMessage()
}

func (s *session) AffectedRows() uint64 {
	return s.sessionVars.StmtCtx.AffectedRows()
}

func (s *session) SetClientCapability(capability uint32) {
	s.sessionVars.ClientCapability = capability
}

func (s *session) SetConnectionID(connectionID uint64) {
	s.sessionVars.ConnectionID = connectionID
}

func (s *session) SetTLSState(tlsState *tls.ConnectionState) {
	// If user is not connected via TLS, then tlsState == nil.
	if tlsState != nil {
		s.sessionVars.TLSConnectionState = tlsState
	}
}

func (s *session) SetCommandValue(command byte) {
	atomic.StoreUint32(&s.sessionVars.CommandValue, uint32(command))
}

func (s *session) SetCollation(coID int) error {
	cs, co, err := charset.GetCharsetInfoByID(coID)
	if err != nil {
		return err
	}
	for _, v := range variable.SetNamesVariables {
		terror.Log(s.sessionVars.SetSystemVar(v, cs))
	}
	terror.Log(s.sessionVars.SetSystemVar(variable.CollationConnection, co))
	return nil
}

func (s *session) PreparedPlanCache() *kvcache.SimpleLRUCache {
	return s.preparedPlanCache
}

func (s *session) SetSessionManager(sm util.SessionManager) {
	s.sessionManager = sm
}

func (s *session) GetSessionManager() util.SessionManager {
	return s.sessionManager
}

func (s *session) StoreQueryFeedback(feedback interface{}) {
	if s.statsCollector != nil {
		do, err := GetDomain(s.store)
		if err != nil {
			logutil.BgLogger().Debug("domain not found", zap.Error(err))
			metrics.StoreQueryFeedbackCounter.WithLabelValues(metrics.LblError).Inc()
			return
		}
		err = s.statsCollector.StoreQueryFeedback(feedback, do.StatsHandle())
		if err != nil {
			logutil.BgLogger().Debug("store query feedback", zap.Error(err))
			metrics.StoreQueryFeedbackCounter.WithLabelValues(metrics.LblError).Inc()
			return
		}
		metrics.StoreQueryFeedbackCounter.WithLabelValues(metrics.LblOK).Inc()
	}
}

// FieldList returns fields list of a table.
func (s *session) FieldList(tableName string) ([]*ast.ResultField, error) {
	is := executor.GetInfoSchema(s)
	dbName := model.NewCIStr(s.GetSessionVars().CurrentDB)
	tName := model.NewCIStr(tableName)
	table, err := is.TableByName(dbName, tName)
	if err != nil {
		return nil, err
	}

	cols := table.Cols()
	fields := make([]*ast.ResultField, 0, len(cols))
	for _, col := range table.Cols() {
		rf := &ast.ResultField{
			ColumnAsName: col.Name,
			TableAsName:  tName,
			DBName:       dbName,
			Table:        table.Meta(),
			Column:       col.ColumnInfo,
		}
		fields = append(fields, rf)
	}
	return fields, nil
}

func (s *session) doCommit(ctx context.Context) error {
	if !s.txn.Valid() {
		return nil
	}
	defer func() {
		s.txn.changeToInvalid()
		s.sessionVars.SetStatusFlag(mysql.ServerStatusInTrans, false)
	}()
	if s.txn.IsReadOnly() {
		return nil
	}

	// mockCommitError and mockGetTSErrorInRetry use to test PR #8743.
	failpoint.Inject("mockCommitError", func(val failpoint.Value) {
		if val.(bool) && kv.IsMockCommitErrorEnable() {
			kv.MockCommitErrorDisable()
			failpoint.Return(kv.ErrTxnRetryable)
		}
	})

	if s.sessionVars.BinlogClient != nil {
		prewriteValue := binloginfo.GetPrewriteValue(s, false)
		if prewriteValue != nil {
			prewriteData, err := prewriteValue.Marshal()
			if err != nil {
				return errors.Trace(err)
			}
			info := &binloginfo.BinlogInfo{
				Data: &binlog.Binlog{
					Tp:            binlog.BinlogType_Prewrite,
					PrewriteValue: prewriteData,
				},
				Client: s.sessionVars.BinlogClient,
			}
			s.txn.SetOption(kv.BinlogInfo, info)
		}
	}

	// Get the related table IDs.
	relatedTables := s.GetSessionVars().TxnCtx.TableDeltaMap
	tableIDs := make([]int64, 0, len(relatedTables))
	for id := range relatedTables {
		tableIDs = append(tableIDs, id)
	}
	// Set this option for 2 phase commit to validate schema lease.
	s.txn.SetOption(kv.SchemaChecker, domain.NewSchemaChecker(domain.GetDomain(s), s.sessionVars.TxnCtx.SchemaVersion, tableIDs))

	return s.txn.Commit(sessionctx.SetCommitCtx(ctx, s))
}

func (s *session) doCommitWithRetry(ctx context.Context) error {
	var txnSize int
	var isPessimistic bool
	if s.txn.Valid() {
		txnSize = s.txn.Size()
		isPessimistic = s.txn.IsPessimistic()
	}
	if span := opentracing.SpanFromContext(ctx); span != nil && span.Tracer() != nil {
		span1 := span.Tracer().StartSpan("session.doCommitWitRetry", opentracing.ChildOf(span.Context()))
		defer span1.Finish()
		ctx = opentracing.ContextWithSpan(ctx, span1)
	}
	err := s.doCommit(ctx)
	if err != nil {
		commitRetryLimit := s.sessionVars.RetryLimit
		if !s.sessionVars.TxnCtx.CouldRetry {
			commitRetryLimit = 0
		}
		// Don't retry in BatchInsert mode. As a counter-example, insert into t1 select * from t2,
		// BatchInsert already commit the first batch 1000 rows, then it commit 1000-2000 and retry the statement,
		// Finally t1 will have more data than t2, with no errors return to user!
		if s.isTxnRetryableError(err) && !s.sessionVars.BatchInsert && commitRetryLimit > 0 && !isPessimistic {
			logutil.Logger(ctx).Warn("sql",
				zap.String("label", s.getSQLLabel()),
				zap.Error(err),
				zap.String("txn", s.txn.GoString()))
			// Transactions will retry 2 ~ commitRetryLimit times.
			// We make larger transactions retry less times to prevent cluster resource outage.
			txnSizeRate := float64(txnSize) / float64(kv.TxnTotalSizeLimit)
			maxRetryCount := commitRetryLimit - int64(float64(commitRetryLimit-1)*txnSizeRate)
			err = s.retry(ctx, uint(maxRetryCount))
		}
	}
	counter := s.sessionVars.TxnCtx.StatementCount
	duration := time.Since(s.GetSessionVars().TxnCtx.CreateTime).Seconds()
	s.recordOnTransactionExecution(err, counter, duration)
	s.cleanRetryInfo()

	if isoLevelOneShot := &s.sessionVars.TxnIsolationLevelOneShot; isoLevelOneShot.State != 0 {
		switch isoLevelOneShot.State {
		case 1:
			isoLevelOneShot.State = 2
		case 2:
			isoLevelOneShot.State = 0
			isoLevelOneShot.Value = ""
		}
	}

	if err != nil {
		logutil.Logger(ctx).Warn("commit failed",
			zap.String("finished txn", s.txn.GoString()),
			zap.Error(err))
		return err
	}
	mapper := s.GetSessionVars().TxnCtx.TableDeltaMap
	if s.statsCollector != nil && mapper != nil {
		for id, item := range mapper {
			s.statsCollector.Update(id, item.Delta, item.Count, &item.ColSize)
		}
	}
	return nil
}

func (s *session) CommitTxn(ctx context.Context) error {
	if span := opentracing.SpanFromContext(ctx); span != nil && span.Tracer() != nil {
		span1 := span.Tracer().StartSpan("session.CommitTxn", opentracing.ChildOf(span.Context()))
		defer span1.Finish()
		ctx = opentracing.ContextWithSpan(ctx, span1)
	}

	var commitDetail *execdetails.CommitDetails
	ctx = context.WithValue(ctx, execdetails.CommitDetailCtxKey, &commitDetail)
	err := s.doCommitWithRetry(ctx)
	if commitDetail != nil {
		s.sessionVars.StmtCtx.MergeExecDetails(nil, commitDetail)
	}

	failpoint.Inject("keepHistory", func(val failpoint.Value) {
		if val.(bool) {
			failpoint.Return(err)
		}
	})

	s.sessionVars.TxnCtx.Cleanup()
	s.recordTransactionCounter(nil, err)
	return err
}

func (s *session) RollbackTxn(ctx context.Context) {
	if span := opentracing.SpanFromContext(ctx); span != nil && span.Tracer() != nil {
		span1 := span.Tracer().StartSpan("session.RollbackTxn", opentracing.ChildOf(span.Context()))
		defer span1.Finish()
	}

	if s.txn.Valid() {
		terror.Log(s.txn.Rollback())
		if s.isInternal() {
			transactionRollbackCounterInternal.Inc()
		} else {
			transactionRollbackCounterGeneral.Inc()
		}
	}
	s.cleanRetryInfo()
	s.txn.changeToInvalid()
	s.sessionVars.TxnCtx.Cleanup()
	s.sessionVars.SetStatusFlag(mysql.ServerStatusInTrans, false)
}

func (s *session) GetClient() kv.Client {
	return s.client
}

func (s *session) String() string {
	// TODO: how to print binded context in values appropriately?
	sessVars := s.sessionVars
	data := map[string]interface{}{
		"id":         sessVars.ConnectionID,
		"user":       sessVars.User,
		"currDBName": sessVars.CurrentDB,
		"status":     sessVars.Status,
		"strictMode": sessVars.StrictSQLMode,
	}
	if s.txn.Valid() {
		// if txn is committed or rolled back, txn is nil.
		data["txn"] = s.txn.String()
	}
	if sessVars.SnapshotTS != 0 {
		data["snapshotTS"] = sessVars.SnapshotTS
	}
	if sessVars.StmtCtx.LastInsertID > 0 {
		data["lastInsertID"] = sessVars.StmtCtx.LastInsertID
	}
	if len(sessVars.PreparedStmts) > 0 {
		data["preparedStmtCount"] = len(sessVars.PreparedStmts)
	}
	b, err := json.MarshalIndent(data, "", "  ")
	terror.Log(errors.Trace(err))
	return string(b)
}

const sqlLogMaxLen = 1024

// SchemaChangedWithoutRetry is used for testing.
var SchemaChangedWithoutRetry bool

func (s *session) getSQLLabel() string {
	if s.sessionVars.InRestrictedSQL {
		return metrics.LblInternal
	}
	return metrics.LblGeneral
}

func (s *session) isInternal() bool {
	return s.sessionVars.InRestrictedSQL
}

func (s *session) isTxnRetryableError(err error) bool {
	if SchemaChangedWithoutRetry {
		return kv.IsTxnRetryableError(err)
	}
	return kv.IsTxnRetryableError(err) || domain.ErrInfoSchemaChanged.Equal(err)
}

func (s *session) checkTxnAborted(stmt sqlexec.Statement) error {
	if s.txn.doNotCommit == nil {
		return nil
	}
	// If the transaction is aborted, the following statements do not need to execute, except `commit` and `rollback`,
	// because they are used to finish the aborted transaction.
	if _, ok := stmt.(*executor.ExecStmt).StmtNode.(*ast.CommitStmt); ok {
		return nil
	}
	if _, ok := stmt.(*executor.ExecStmt).StmtNode.(*ast.RollbackStmt); ok {
		return nil
	}
	return errors.New("current transaction is aborted, commands ignored until end of transaction block:" + s.txn.doNotCommit.Error())
}

func (s *session) retry(ctx context.Context, maxCnt uint) (err error) {
	var retryCnt uint
	defer func() {
		s.sessionVars.RetryInfo.Retrying = false
		// retryCnt only increments on retryable error, so +1 here.
		metrics.SessionRetry.Observe(float64(retryCnt + 1))
		s.sessionVars.SetStatusFlag(mysql.ServerStatusInTrans, false)
		if err != nil {
			s.RollbackTxn(ctx)
		}
		s.txn.changeToInvalid()
	}()

	connID := s.sessionVars.ConnectionID
	s.sessionVars.RetryInfo.Retrying = true
	if s.sessionVars.TxnCtx.ForUpdate {
		err = ErrForUpdateCantRetry.GenWithStackByArgs(connID)
		return err
	}

	nh := GetHistory(s)
	var schemaVersion int64
	sessVars := s.GetSessionVars()
	orgStartTS := sessVars.TxnCtx.StartTS
	label := s.getSQLLabel()
	for {
		s.PrepareTxnCtx(ctx)
		s.sessionVars.RetryInfo.ResetOffset()
		for i, sr := range nh.history {
			st := sr.st
			s.sessionVars.StmtCtx = sr.stmtCtx
			s.sessionVars.StartTime = time.Now()
			s.sessionVars.DurationCompile = time.Duration(0)
			s.sessionVars.DurationParse = time.Duration(0)
			s.sessionVars.StmtCtx.ResetForRetry()
			s.sessionVars.PreparedParams = s.sessionVars.PreparedParams[:0]
			schemaVersion, err = st.RebuildPlan(ctx)
			if err != nil {
				return err
			}

			if retryCnt == 0 {
				// We do not have to log the query every time.
				// We print the queries at the first try only.
				logutil.Logger(ctx).Warn("retrying",
					zap.Int64("schemaVersion", schemaVersion),
					zap.Uint("retryCnt", retryCnt),
					zap.Int("queryNum", i),
					zap.String("sql", sqlForLog(st.OriginText())+sessVars.PreparedParams.String()))
			} else {
				logutil.Logger(ctx).Warn("retrying",
					zap.Int64("schemaVersion", schemaVersion),
					zap.Uint("retryCnt", retryCnt),
					zap.Int("queryNum", i))
			}
			_, err = st.Exec(ctx)
			if err != nil {
				s.StmtRollback()
				break
			}
			err = s.StmtCommit()
			if err != nil {
				return err
			}
		}
		logutil.Logger(ctx).Warn("transaction association",
			zap.Uint64("retrying txnStartTS", s.GetSessionVars().TxnCtx.StartTS),
			zap.Uint64("original txnStartTS", orgStartTS))
		if hook := ctx.Value("preCommitHook"); hook != nil {
			// For testing purpose.
			hook.(func())()
		}
		if err == nil {
			err = s.doCommit(ctx)
			if err == nil {
				break
			}
		}
		if !s.isTxnRetryableError(err) {
			logutil.Logger(ctx).Warn("sql",
				zap.String("label", label),
				zap.Stringer("session", s),
				zap.Error(err))
			metrics.SessionRetryErrorCounter.WithLabelValues(label, metrics.LblUnretryable)
			return err
		}
		retryCnt++
		if retryCnt >= maxCnt {
			logutil.Logger(ctx).Warn("sql",
				zap.String("label", label),
				zap.Uint("retry reached max count", retryCnt))
			metrics.SessionRetryErrorCounter.WithLabelValues(label, metrics.LblReachMax)
			return err
		}
		logutil.Logger(ctx).Warn("sql",
			zap.String("label", label),
			zap.Error(err),
			zap.String("txn", s.txn.GoString()))
		kv.BackOff(retryCnt)
		s.txn.changeToInvalid()
		s.sessionVars.SetStatusFlag(mysql.ServerStatusInTrans, false)
	}
	return err
}

func sqlForLog(sql string) string {
	if len(sql) > sqlLogMaxLen {
		sql = sql[:sqlLogMaxLen] + fmt.Sprintf("(len:%d)", len(sql))
	}
	return executor.QueryReplacer.Replace(sql)
}

type sessionPool interface {
	Get() (pools.Resource, error)
	Put(pools.Resource)
}

func (s *session) sysSessionPool() sessionPool {
	return domain.GetDomain(s).SysSessionPool()
}

// ExecRestrictedSQL implements RestrictedSQLExecutor interface.
// This is used for executing some restricted sql statements, usually executed during a normal statement execution.
// Unlike normal Exec, it doesn't reset statement status, doesn't commit or rollback the current transaction
// and doesn't write binlog.
func (s *session) ExecRestrictedSQL(sql string) ([]chunk.Row, []*ast.ResultField, error) {
	ctx := context.TODO()

	// Use special session to execute the sql.
	tmp, err := s.sysSessionPool().Get()
	if err != nil {
		return nil, nil, err
	}
	se := tmp.(*session)
	defer s.sysSessionPool().Put(tmp)
	metrics.SessionRestrictedSQLCounter.Inc()

	return execRestrictedSQL(ctx, se, sql)
}

// ExecRestrictedSQLWithSnapshot implements RestrictedSQLExecutor interface.
// This is used for executing some restricted sql statements with snapshot.
// If current session sets the snapshot timestamp, then execute with this snapshot timestamp.
// Otherwise, execute with the current transaction start timestamp if the transaction is valid.
func (s *session) ExecRestrictedSQLWithSnapshot(sql string) ([]chunk.Row, []*ast.ResultField, error) {
	ctx := context.TODO()

	// Use special session to execute the sql.
	tmp, err := s.sysSessionPool().Get()
	if err != nil {
		return nil, nil, err
	}
	se := tmp.(*session)
	defer s.sysSessionPool().Put(tmp)
	metrics.SessionRestrictedSQLCounter.Inc()
	var snapshot uint64
	txn, err := s.Txn(false)
	if err != nil {
		return nil, nil, err
	}
	if txn.Valid() {
		snapshot = s.txn.StartTS()
	}
	if s.sessionVars.SnapshotTS != 0 {
		snapshot = s.sessionVars.SnapshotTS
	}
	// Set snapshot.
	if snapshot != 0 {
		if err := se.sessionVars.SetSystemVar(variable.TiDBSnapshot, strconv.FormatUint(snapshot, 10)); err != nil {
			return nil, nil, err
		}
		defer func() {
			if err := se.sessionVars.SetSystemVar(variable.TiDBSnapshot, ""); err != nil {
				logutil.BgLogger().Error("set tidbSnapshot error", zap.Error(err))
			}
		}()
	}
	return execRestrictedSQL(ctx, se, sql)
}

func execRestrictedSQL(ctx context.Context, se *session, sql string) ([]chunk.Row, []*ast.ResultField, error) {
	startTime := time.Now()
	recordSets, err := se.Execute(ctx, sql)
	if err != nil {
		return nil, nil, err
	}

	var (
		rows   []chunk.Row
		fields []*ast.ResultField
	)
	// Execute all recordset, take out the first one as result.
	for i, rs := range recordSets {
		tmp, err := drainRecordSet(ctx, se, rs)
		if err != nil {
			return nil, nil, err
		}
		if err = rs.Close(); err != nil {
			return nil, nil, err
		}

		if i == 0 {
			rows = tmp
			fields = rs.Fields()
		}
	}
	metrics.QueryDurationHistogram.WithLabelValues(metrics.LblInternal).Observe(time.Since(startTime).Seconds())
	return rows, fields, nil
}

func createSessionFunc(store kv.Storage) pools.Factory {
	return func() (pools.Resource, error) {
		se, err := createSession(store)
		if err != nil {
			return nil, err
		}
		err = variable.SetSessionSystemVar(se.sessionVars, variable.AutoCommit, types.NewStringDatum("1"))
		if err != nil {
			return nil, err
		}
		err = variable.SetSessionSystemVar(se.sessionVars, variable.MaxExecutionTime, types.NewUintDatum(0))
		if err != nil {
			return nil, errors.Trace(err)
		}
		se.sessionVars.CommonGlobalLoaded = true
		se.sessionVars.InRestrictedSQL = true
		return se, nil
	}
}

func createSessionWithDomainFunc(store kv.Storage) func(*domain.Domain) (pools.Resource, error) {
	return func(dom *domain.Domain) (pools.Resource, error) {
		se, err := createSessionWithDomain(store, dom)
		if err != nil {
			return nil, err
		}
		err = variable.SetSessionSystemVar(se.sessionVars, variable.AutoCommit, types.NewStringDatum("1"))
		if err != nil {
			return nil, err
		}
		err = variable.SetSessionSystemVar(se.sessionVars, variable.MaxExecutionTime, types.NewUintDatum(0))
		if err != nil {
			return nil, errors.Trace(err)
		}
		se.sessionVars.CommonGlobalLoaded = true
		se.sessionVars.InRestrictedSQL = true
		return se, nil
	}
}

func drainRecordSet(ctx context.Context, se *session, rs sqlexec.RecordSet) ([]chunk.Row, error) {
	var rows []chunk.Row
	req := rs.NewChunk()
	for {
		err := rs.Next(ctx, req)
		if err != nil || req.NumRows() == 0 {
			return rows, err
		}
		iter := chunk.NewIterator4Chunk(req)
		for r := iter.Begin(); r != iter.End(); r = iter.Next() {
			rows = append(rows, r)
		}
		req = chunk.Renew(req, se.sessionVars.MaxChunkSize)
	}
}

// getExecRet executes restricted sql and the result is one column.
// It returns a string value.
func (s *session) getExecRet(ctx sessionctx.Context, sql string) (string, error) {
	rows, fields, err := s.ExecRestrictedSQL(sql)
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "", executor.ErrResultIsEmpty
	}
	d := rows[0].GetDatum(0, &fields[0].Column.FieldType)
	value, err := d.ToString()
	if err != nil {
		return "", err
	}
	return value, nil
}

// GetAllSysVars implements GlobalVarAccessor.GetAllSysVars interface.
func (s *session) GetAllSysVars() (map[string]string, error) {
	if s.Value(sessionctx.Initing) != nil {
		return nil, nil
	}
	sql := `SELECT VARIABLE_NAME, VARIABLE_VALUE FROM %s.%s;`
	sql = fmt.Sprintf(sql, mysql.SystemDB, mysql.GlobalVariablesTable)
	rows, _, err := s.ExecRestrictedSQL(sql)
	if err != nil {
		return nil, err
	}
	ret := make(map[string]string)
	for _, r := range rows {
		k, v := r.GetString(0), r.GetString(1)
		ret[k] = v
	}
	return ret, nil
}

// GetGlobalSysVar implements GlobalVarAccessor.GetGlobalSysVar interface.
func (s *session) GetGlobalSysVar(name string) (string, error) {
	if s.Value(sessionctx.Initing) != nil {
		// When running bootstrap or upgrade, we should not access global storage.
		return "", nil
	}
	sql := fmt.Sprintf(`SELECT VARIABLE_VALUE FROM %s.%s WHERE VARIABLE_NAME="%s";`,
		mysql.SystemDB, mysql.GlobalVariablesTable, name)
	sysVar, err := s.getExecRet(s, sql)
	if err != nil {
		if executor.ErrResultIsEmpty.Equal(err) {
			if sv, ok := variable.SysVars[name]; ok {
				return sv.Value, nil
			}
			return "", variable.UnknownSystemVar.GenWithStackByArgs(name)
		}
		return "", err
	}
	return sysVar, nil
}

// SetGlobalSysVar implements GlobalVarAccessor.SetGlobalSysVar interface.
func (s *session) SetGlobalSysVar(name, value string) error {
	if name == variable.SQLModeVar {
		value = mysql.FormatSQLModeStr(value)
		if _, err := mysql.GetSQLMode(value); err != nil {
			return err
		}
	}
	var sVal string
	var err error
	sVal, err = variable.ValidateSetSystemVar(s.sessionVars, name, value)
	if err != nil {
		return err
	}
	name = strings.ToLower(name)
	sql := fmt.Sprintf(`REPLACE %s.%s VALUES ('%s', '%s');`,
		mysql.SystemDB, mysql.GlobalVariablesTable, name, sVal)
	_, _, err = s.ExecRestrictedSQL(sql)
	return err
}

func (s *session) ParseSQL(ctx context.Context, sql, charset, collation string) ([]ast.StmtNode, []error, error) {
	if span := opentracing.SpanFromContext(ctx); span != nil && span.Tracer() != nil {
		span1 := span.Tracer().StartSpan("session.ParseSQL", opentracing.ChildOf(span.Context()))
		defer span1.Finish()
	}
	s.parser.SetSQLMode(s.sessionVars.SQLMode)
	s.parser.EnableWindowFunc(s.sessionVars.EnableWindowFunction)
	return s.parser.Parse(sql, charset, collation)
}

func (s *session) SetProcessInfo(sql string, t time.Time, command byte, maxExecutionTime uint64) {
	// If command == mysql.ComSleep, it means the SQL execution is finished. The processinfo is reset to SLEEP.
	// If the SQL finished and the session is not in transaction, the current start timestamp need to reset to 0.
	// Otherwise, it should be set to the transaction start timestamp.
	// Why not reset the transaction start timestamp to 0 when transaction committed?
	// Because the select statement and other statements need this timestamp to read data,
	// after the transaction is committed. e.g. SHOW MASTER STATUS;
	var curTxnStartTS uint64
	if command != mysql.ComSleep || s.GetSessionVars().InTxn() {
		curTxnStartTS = s.sessionVars.TxnCtx.StartTS
	}
	pi := util.ProcessInfo{
		ID:               s.sessionVars.ConnectionID,
		DB:               s.sessionVars.CurrentDB,
		Command:          command,
		Plan:             s.currentPlan,
		Time:             t,
		State:            s.Status(),
		Info:             sql,
		CurTxnStartTS:    curTxnStartTS,
		StmtCtx:          s.sessionVars.StmtCtx,
		StatsInfo:        plannercore.GetStatsInfo,
		MaxExecutionTime: maxExecutionTime,
	}
	if s.sessionVars.User != nil {
		pi.User = s.sessionVars.User.Username
		pi.Host = s.sessionVars.User.Hostname
	}
	s.processInfo.Store(&pi)
}

func (s *session) executeStatement(ctx context.Context, connID uint64, stmtNode ast.StmtNode, stmt sqlexec.Statement, recordSets []sqlexec.RecordSet, inMulitQuery bool) ([]sqlexec.RecordSet, error) {
	s.SetValue(sessionctx.QueryString, stmt.OriginText())
	if _, ok := stmtNode.(ast.DDLNode); ok {
		s.SetValue(sessionctx.LastExecuteDDL, true)
	} else {
		s.ClearValue(sessionctx.LastExecuteDDL)
	}
	logStmt(stmtNode, s.sessionVars)
	startTime := time.Now()
	recordSet, err := runStmt(ctx, s, stmt)
	if err != nil {
		if !kv.ErrKeyExists.Equal(err) {
			logutil.Logger(ctx).Warn("run statement failed",
				zap.Int64("schemaVersion", s.sessionVars.TxnCtx.SchemaVersion),
				zap.Error(err),
				zap.String("session", s.String()))
		}
		return nil, err
	}
	s.recordTransactionCounter(stmtNode, err)
	if s.isInternal() {
		sessionExecuteRunDurationInternal.Observe(time.Since(startTime).Seconds())
	} else {
		sessionExecuteRunDurationGeneral.Observe(time.Since(startTime).Seconds())
	}

	if inMulitQuery && recordSet == nil {
		recordSet = &multiQueryNoDelayRecordSet{
			affectedRows: s.AffectedRows(),
			lastMessage:  s.LastMessage(),
			warnCount:    s.sessionVars.StmtCtx.WarningCount(),
			lastInsertID: s.sessionVars.StmtCtx.LastInsertID,
			status:       s.sessionVars.Status,
		}
	}

	if recordSet != nil {
		recordSets = append(recordSets, recordSet)
	}
	return recordSets, nil
}

func (s *session) Execute(ctx context.Context, sql string) (recordSets []sqlexec.RecordSet, err error) {
	if span := opentracing.SpanFromContext(ctx); span != nil && span.Tracer() != nil {
		span1 := span.Tracer().StartSpan("session.Execute", opentracing.ChildOf(span.Context()))
		defer span1.Finish()
		ctx = opentracing.ContextWithSpan(ctx, span1)
		logutil.Eventf(ctx, "execute: %s", sql)
	}
	if recordSets, err = s.execute(ctx, sql); err != nil {
		s.sessionVars.StmtCtx.AppendError(err)
	}
	return
}

func (s *session) execute(ctx context.Context, sql string) (recordSets []sqlexec.RecordSet, err error) {
	s.PrepareTxnCtx(ctx)
	connID := s.sessionVars.ConnectionID
	err = s.loadCommonGlobalVariablesIfNeeded()
	if err != nil {
		return nil, err
	}

	charsetInfo, collation := s.sessionVars.GetCharsetInfo()

	// Step1: Compile query string to abstract syntax trees(ASTs).
	startTS := time.Now()
	s.GetSessionVars().StartTime = startTS
	stmtNodes, warns, err := s.ParseSQL(ctx, sql, charsetInfo, collation)
	if err != nil {
		s.rollbackOnError(ctx)
		logutil.Logger(ctx).Warn("parse SQL failed",
			zap.Error(err),
			zap.String("SQL", sql))
		return nil, util.SyntaxError(err)
	}
	durParse := time.Since(startTS)
	s.GetSessionVars().DurationParse = durParse
	isInternal := s.isInternal()
	if isInternal {
		sessionExecuteParseDurationInternal.Observe(durParse.Seconds())
	} else {
		sessionExecuteParseDurationGeneral.Observe(durParse.Seconds())
	}

	compiler := executor.Compiler{Ctx: s}
	multiQuery := len(stmtNodes) > 1
	for _, stmtNode := range stmtNodes {
		s.PrepareTxnCtx(ctx)

		// Step2: Transform abstract syntax tree to a physical plan(stored in executor.ExecStmt).
		startTS = time.Now()
		// Some executions are done in compile stage, so we reset them before compile.
		if err := executor.ResetContextOfStmt(s, stmtNode); err != nil {
			return nil, err
		}
		stmt, err := compiler.Compile(ctx, stmtNode)
		if err != nil {
			s.rollbackOnError(ctx)
			logutil.Logger(ctx).Warn("compile SQL failed",
				zap.Error(err),
				zap.String("SQL", sql))
			return nil, err
		}
		durCompile := time.Since(startTS)
		s.GetSessionVars().DurationCompile = durCompile
		if isInternal {
			sessionExecuteCompileDurationInternal.Observe(durCompile.Seconds())
		} else {
			sessionExecuteCompileDurationGeneral.Observe(durCompile.Seconds())
		}
		s.currentPlan = stmt.Plan

		// Step3: Execute the physical plan.
		if recordSets, err = s.executeStatement(ctx, connID, stmtNode, stmt, recordSets, multiQuery); err != nil {
			return nil, err
		}
	}

	if s.sessionVars.ClientCapability&mysql.ClientMultiResults == 0 && len(recordSets) > 1 {
		// return the first recordset if client doesn't support ClientMultiResults.
		recordSets = recordSets[:1]
	}

	for _, warn := range warns {
		s.sessionVars.StmtCtx.AppendWarning(util.SyntaxWarn(warn))
	}
	return recordSets, nil
}

// rollbackOnError makes sure the next statement starts a new transaction with the latest InfoSchema.
func (s *session) rollbackOnError(ctx context.Context) {
	if !s.sessionVars.InTxn() {
		s.RollbackTxn(ctx)
	}
}

// PrepareStmt is used for executing prepare statement in binary protocol
func (s *session) PrepareStmt(sql string) (stmtID uint32, paramCount int, fields []*ast.ResultField, err error) {
	if s.sessionVars.TxnCtx.InfoSchema == nil {
		// We don't need to create a transaction for prepare statement, just get information schema will do.
		s.sessionVars.TxnCtx.InfoSchema = domain.GetDomain(s).InfoSchema()
	}
	err = s.loadCommonGlobalVariablesIfNeeded()
	if err != nil {
		return
	}

	ctx := context.Background()
	inTxn := s.GetSessionVars().InTxn()
	// NewPrepareExec may need startTS to build the executor, for example prepare statement has subquery in int.
	// So we have to call PrepareTxnCtx here.
	s.PrepareTxnCtx(ctx)
	s.PrepareTxnFuture(ctx)
	prepareExec := executor.NewPrepareExec(s, executor.GetInfoSchema(s), sql)
	err = prepareExec.Next(ctx, nil)
	if err != nil {
		return
	}
	if !inTxn {
		// We could start a transaction to build the prepare executor before, we should rollback it here.
		s.RollbackTxn(ctx)
	}
	return prepareExec.ID, prepareExec.ParamCount, prepareExec.Fields, nil
}

func (s *session) CommonExec(ctx context.Context,
	stmtID uint32, prepareStmt *plannercore.CachedPrepareStmt, args []types.Datum) (sqlexec.RecordSet, error) {
	st, err := executor.CompileExecutePreparedStmt(ctx, s, stmtID, args)
	if err != nil {
		return nil, err
	}
	logQuery(st.OriginText(), s.sessionVars)
	return runStmt(ctx, s, st)
}

// CachedPlanExec short path currently ONLY for cached "point select plan" execution
func (s *session) CachedPlanExec(ctx context.Context,
	stmtID uint32, prepareStmt *plannercore.CachedPrepareStmt, args []types.Datum) (sqlexec.RecordSet, error) {
	prepared := prepareStmt.PreparedAst
	// compile ExecStmt
	is := executor.GetInfoSchema(s)
	execAst := &ast.ExecuteStmt{ExecID: stmtID}
	if err := executor.ResetContextOfStmt(s, execAst); err != nil {
		return nil, err
	}
	execAst.BinaryArgs = args
	execPlan, err := planner.OptimizeExecStmt(ctx, s, execAst, is)
	if err != nil {
		return nil, err
	}
	stmt := &executor.ExecStmt{
		InfoSchema:  is,
		Plan:        execPlan,
		StmtNode:    execAst,
		Ctx:         s,
		OutputNames: execPlan.OutputNames(),
	}
	s.GetSessionVars().DurationCompile = time.Since(s.sessionVars.StartTime)
	stmt.Text = prepared.Stmt.Text()
	s.GetSessionVars().StmtCtx.OriginalSQL = stmt.Text
	logQuery(stmt.OriginText(), s.sessionVars)

	// run ExecStmt
	var resultSet sqlexec.RecordSet
	switch prepared.CachedPlan.(type) {
	case *plannercore.PointGetPlan:
		resultSet, err = stmt.PointGet(ctx, is)
		s.txn.changeToInvalid()
	case *plannercore.Update:
		s.PrepareTxnFuture(ctx)
		s.GetSessionVars().StmtCtx.Priority = kv.PriorityHigh
		resultSet, err = runStmt(ctx, s, stmt)
	default:
		prepared.CachedPlan = nil
		return nil, errors.Errorf("invalid cached plan type")
	}
	return resultSet, err
}

// IsCachedExecOk check if we can execute using plan cached in prepared structure
// Be careful for the short path, current precondition is ths cached plan satisfying
// IsPointGetWithPKOrUniqueKeyByAutoCommit
func (s *session) IsCachedExecOk(ctx context.Context, preparedStmt *plannercore.CachedPrepareStmt) (bool, error) {
	prepared := preparedStmt.PreparedAst
	if prepared.CachedPlan == nil {
		return false, nil
	}
	// check auto commit
	if !s.GetSessionVars().IsAutocommit() {
		return false, nil
	}
	// check schema version
	is := executor.GetInfoSchema(s)
	if prepared.SchemaVersion != is.SchemaMetaVersion() {
		prepared.CachedPlan = nil
		return false, nil
	}
	// maybe we'd better check cached plan type here, current
	// only point select/update will be cached, see "getPhysicalPlan" func
	var ok bool
	var err error
	switch prepared.CachedPlan.(type) {
	case *plannercore.PointGetPlan:
		ok = true
	case *plannercore.Update:
		pointUpdate := prepared.CachedPlan.(*plannercore.Update)
		_, ok = pointUpdate.SelectPlan.(*plannercore.PointGetPlan)
		if !ok {
			err = errors.Errorf("cached update plan not point update")
			prepared.CachedPlan = nil
			return false, err
		}
	default:
		ok = false
	}
	return ok, err
}

// ExecutePreparedStmt executes a prepared statement.
func (s *session) ExecutePreparedStmt(ctx context.Context, stmtID uint32, args []types.Datum) (sqlexec.RecordSet, error) {
	s.PrepareTxnCtx(ctx)
	var err error
	s.sessionVars.StartTime = time.Now()
	preparedPointer, ok := s.sessionVars.PreparedStmts[stmtID]
	if !ok {
		err = plannercore.ErrStmtNotFound
		logutil.Logger(ctx).Error("prepared statement not found", zap.Uint32("stmtID", stmtID))
		return nil, err
	}
	preparedStmt, ok := preparedPointer.(*plannercore.CachedPrepareStmt)
	if !ok {
		return nil, errors.Errorf("invalid CachedPrepareStmt type")
	}
	ok, err = s.IsCachedExecOk(ctx, preparedStmt)
	if err != nil {
		return nil, err
	}
	if ok {
		return s.CachedPlanExec(ctx, stmtID, preparedStmt, args)
	}
	return s.CommonExec(ctx, stmtID, preparedStmt, args)
}

func (s *session) DropPreparedStmt(stmtID uint32) error {
	vars := s.sessionVars
	if _, ok := vars.PreparedStmts[stmtID]; !ok {
		return plannercore.ErrStmtNotFound
	}
	vars.RetryInfo.DroppedPreparedStmtIDs = append(vars.RetryInfo.DroppedPreparedStmtIDs, stmtID)
	return nil
}

func (s *session) Txn(active bool) (kv.Transaction, error) {
	if s.txn.pending() && active {
		// Transaction is lazy initialized.
		// PrepareTxnCtx is called to get a tso future, makes s.txn a pending txn,
		// If Txn() is called later, wait for the future to get a valid txn.
		txnCap := s.getMembufCap()
		if err := s.txn.changePendingToValid(txnCap); err != nil {
			logutil.BgLogger().Error("active transaction fail",
				zap.Error(err))
			s.txn.cleanup()
			s.sessionVars.TxnCtx.StartTS = 0
			return &s.txn, err
		}
		s.sessionVars.TxnCtx.StartTS = s.txn.StartTS()
		if s.sessionVars.TxnCtx.IsPessimistic {
			s.txn.SetOption(kv.Pessimistic, true)
		}
		if !s.sessionVars.IsAutocommit() {
			s.sessionVars.SetStatusFlag(mysql.ServerStatusInTrans, true)
		}
		s.sessionVars.TxnCtx.CouldRetry = s.isTxnRetryable()
		if s.sessionVars.GetReplicaRead().IsFollowerRead() {
			s.txn.SetOption(kv.ReplicaRead, kv.ReplicaReadFollower)
		}
	}
	return &s.txn, nil
}

// isTxnRetryable (if returns true) means the transaction could retry.
// If the transaction is in pessimistic mode, do not retry.
// If the session is already in transaction, enable retry or internal SQL could retry.
// If not, the transaction could always retry, because it should be auto committed transaction.
// Anyway the retry limit is 0, the transaction could not retry.
func (s *session) isTxnRetryable() bool {
	sessVars := s.sessionVars

	// The pessimistic transaction no need to retry.
	if sessVars.TxnCtx.IsPessimistic {
		return false
	}

	// If retry limit is 0, the transaction could not retry.
	if sessVars.RetryLimit == 0 {
		return false
	}

	// If the session is not InTxn, it is an auto-committed transaction.
	// The auto-committed transaction could always retry.
	if !sessVars.InTxn() {
		return true
	}

	// The internal transaction could always retry.
	if sessVars.InRestrictedSQL {
		return true
	}

	// If the retry is enabled, the transaction could retry.
	if !sessVars.DisableTxnAutoRetry {
		return true
	}

	return false
}

func (s *session) NewTxn(ctx context.Context) error {
	if s.txn.Valid() {
		txnID := s.txn.StartTS()
		err := s.CommitTxn(ctx)
		if err != nil {
			return err
		}
		vars := s.GetSessionVars()
		logutil.Logger(ctx).Info("NewTxn() inside a transaction auto commit",
			zap.Int64("schemaVersion", vars.TxnCtx.SchemaVersion),
			zap.Uint64("txnStartTS", txnID))
	}

	txn, err := s.store.Begin()
	if err != nil {
		return err
	}
	txn.SetCap(s.getMembufCap())
	txn.SetVars(s.sessionVars.KVVars)
	if s.GetSessionVars().GetReplicaRead().IsFollowerRead() {
		txn.SetOption(kv.ReplicaRead, kv.ReplicaReadFollower)
	}
	s.txn.changeInvalidToValid(txn)
	is := domain.GetDomain(s).InfoSchema()
	s.sessionVars.TxnCtx = &variable.TransactionContext{
		InfoSchema:    is,
		SchemaVersion: is.SchemaMetaVersion(),
		CreateTime:    time.Now(),
		StartTS:       txn.StartTS(),
	}
	return nil
}

func (s *session) SetValue(key fmt.Stringer, value interface{}) {
	s.mu.Lock()
	s.mu.values[key] = value
	s.mu.Unlock()
}

func (s *session) Value(key fmt.Stringer) interface{} {
	s.mu.RLock()
	value := s.mu.values[key]
	s.mu.RUnlock()
	return value
}

func (s *session) ClearValue(key fmt.Stringer) {
	s.mu.Lock()
	delete(s.mu.values, key)
	s.mu.Unlock()
}

// Close function does some clean work when session end.
// Close should release the table locks which hold by the session.
func (s *session) Close() {
	// TODO: do clean table locks when session exited without execute Close.
	// TODO: do clean table locks when tidb-server was `kill -9`.
	if s.HasLockedTables() && config.TableLockEnabled() {
		if ds := config.TableLockDelayClean(); ds > 0 {
			time.Sleep(time.Duration(ds) * time.Millisecond)
		}
		lockedTables := s.GetAllTableLocks()
		err := domain.GetDomain(s).DDL().UnlockTables(s, lockedTables)
		if err != nil {
			logutil.BgLogger().Error("release table lock failed", zap.Uint64("conn", s.sessionVars.ConnectionID))
		}
	}
	if s.statsCollector != nil {
		s.statsCollector.Delete()
	}
	bindValue := s.Value(bindinfo.SessionBindInfoKeyType)
	if bindValue != nil {
		bindValue.(*bindinfo.SessionHandle).Close()
	}
	ctx := context.TODO()
	s.RollbackTxn(ctx)
	if s.sessionVars != nil {
		s.sessionVars.WithdrawAllPreparedStmt()
	}
}

// GetSessionVars implements the context.Context interface.
func (s *session) GetSessionVars() *variable.SessionVars {
	return s.sessionVars
}

func (s *session) Auth(user *auth.UserIdentity, authentication []byte, salt []byte) bool {
	pm := privilege.GetPrivilegeManager(s)

	// Check IP or localhost.
	var success bool
	user.AuthUsername, user.AuthHostname, success = pm.ConnectionVerification(user.Username, user.Hostname, authentication, salt)
	if success {
		s.sessionVars.User = user
		s.sessionVars.ActiveRoles = pm.GetDefaultRoles(user.AuthUsername, user.AuthHostname)
		return true
	} else if user.Hostname == variable.DefHostname {
		logutil.BgLogger().Error("user connection verification failed",
			zap.Stringer("user", user))
		return false
	}

	// Check Hostname.
	for _, addr := range getHostByIP(user.Hostname) {
		u, h, success := pm.ConnectionVerification(user.Username, addr, authentication, salt)
		if success {
			s.sessionVars.User = &auth.UserIdentity{
				Username:     user.Username,
				Hostname:     addr,
				AuthUsername: u,
				AuthHostname: h,
			}
			s.sessionVars.ActiveRoles = pm.GetDefaultRoles(u, h)
			return true
		}
	}

	logutil.BgLogger().Error("user connection verification failed",
		zap.Stringer("user", user))
	return false
}

func getHostByIP(ip string) []string {
	if ip == "127.0.0.1" {
		return []string{variable.DefHostname}
	}
	addrs, err := net.LookupAddr(ip)
	terror.Log(errors.Trace(err))
	return addrs
}

// CreateSession4Test creates a new session environment for test.
func CreateSession4Test(store kv.Storage) (Session, error) {
	s, err := CreateSession(store)
	if err == nil {
		// initialize session variables for test.
		s.GetSessionVars().InitChunkSize = 2
		s.GetSessionVars().MaxChunkSize = 32
	}
	return s, err
}

// CreateSession creates a new session environment.
func CreateSession(store kv.Storage) (Session, error) {
	s, err := createSession(store)
	if err != nil {
		return nil, err
	}

	// Add auth here.
	do, err := domap.Get(store)
	if err != nil {
		return nil, err
	}
	pm := &privileges.UserPrivileges{
		Handle: do.PrivilegeHandle(),
	}
	privilege.BindPrivilegeManager(s, pm)

	sessionBindHandle := bindinfo.NewSessionBindHandle(s.parser)
	s.SetValue(bindinfo.SessionBindInfoKeyType, sessionBindHandle)
	// Add stats collector, and it will be freed by background stats worker
	// which periodically updates stats using the collected data.
	if do.StatsHandle() != nil && do.StatsUpdating() {
		s.statsCollector = do.StatsHandle().NewSessionStatsCollector()
	}

	return s, nil
}

// loadSystemTZ loads systemTZ from mysql.tidb
func loadSystemTZ(se *session) (string, error) {
	sql := `select variable_value from mysql.tidb where variable_name = 'system_tz'`
	rss, errLoad := se.Execute(context.Background(), sql)
	if errLoad != nil {
		return "", errLoad
	}
	// the record of mysql.tidb under where condition: variable_name = "system_tz" should shall only be one.
	defer func() {
		if err := rss[0].Close(); err != nil {
			logutil.BgLogger().Error("close result set error", zap.Error(err))
		}
	}()
	req := rss[0].NewChunk()
	if err := rss[0].Next(context.Background(), req); err != nil {
		return "", err
	}
	return req.GetRow(0).GetString(0), nil
}

// BootstrapSession runs the first time when the TiDB server start.
func BootstrapSession(store kv.Storage) (*domain.Domain, error) {
	cfg := config.GetGlobalConfig()
	if len(cfg.Plugin.Load) > 0 {
		err := plugin.Load(context.Background(), plugin.Config{
			Plugins:        strings.Split(cfg.Plugin.Load, ","),
			PluginDir:      cfg.Plugin.Dir,
			GlobalSysVar:   &variable.SysVars,
			PluginVarNames: &variable.PluginVarNames,
		})
		if err != nil {
			return nil, err
		}
	}

	initLoadCommonGlobalVarsSQL()

	ver := getStoreBootstrapVersion(store)
	if ver == notBootstrapped {
		runInBootstrapSession(store, bootstrap)
	} else if ver < currentBootstrapVersion {
		runInBootstrapSession(store, upgrade)
	}

	se, err := createSession(store)
	if err != nil {
		return nil, err
	}
	// get system tz from mysql.tidb
	tz, err := loadSystemTZ(se)
	if err != nil {
		return nil, err
	}

	timeutil.SetSystemTZ(tz)
	dom := domain.GetDomain(se)
	dom.InitExpensiveQueryHandle()

	if !config.GetGlobalConfig().Security.SkipGrantTable {
		err = dom.LoadPrivilegeLoop(se)
		if err != nil {
			return nil, err
		}
	}

	if len(cfg.Plugin.Load) > 0 {
		err := plugin.Init(context.Background(), plugin.Config{EtcdClient: dom.GetEtcdClient()})
		if err != nil {
			return nil, err
		}
	}

	err = executor.LoadExprPushdownBlacklist(se)
	if err != nil {
		return nil, err
	}

	err = executor.LoadOptRuleBlacklist(se)
	if err != nil {
		return nil, err
	}

	se1, err := createSession(store)
	if err != nil {
		return nil, err
	}
	err = dom.UpdateTableStatsLoop(se1)
	if err != nil {
		return nil, err
	}
	se2, err := createSession(store)
	if err != nil {
		return nil, err
	}
	err = dom.LoadBindInfoLoop(se2)
	if err != nil {
		return nil, err
	}
	if raw, ok := store.(tikv.EtcdBackend); ok {
		err = raw.StartGCWorker()
		if err != nil {
			return nil, err
		}
	}

	return dom, err
}

// GetDomain gets the associated domain for store.
func GetDomain(store kv.Storage) (*domain.Domain, error) {
	return domap.Get(store)
}

// runInBootstrapSession create a special session for boostrap to run.
// If no bootstrap and storage is remote, we must use a little lease time to
// bootstrap quickly, after bootstrapped, we will reset the lease time.
// TODO: Using a bootstrap tool for doing this may be better later.
func runInBootstrapSession(store kv.Storage, bootstrap func(Session)) {
	s, err := createSession(store)
	if err != nil {
		// Bootstrap fail will cause program exit.
		logutil.BgLogger().Fatal("createSession error", zap.Error(err))
	}

	s.SetValue(sessionctx.Initing, true)
	bootstrap(s)
	finishBootstrap(store)
	s.ClearValue(sessionctx.Initing)

	dom := domain.GetDomain(s)
	dom.Close()
	domap.Delete(store)
}

func createSession(store kv.Storage) (*session, error) {
	dom, err := domap.Get(store)
	if err != nil {
		return nil, err
	}
	s := &session{
		store:           store,
		parser:          parser.New(),
		sessionVars:     variable.NewSessionVars(),
		ddlOwnerChecker: dom.DDL().OwnerManager(),
		client:          store.GetClient(),
	}
	if plannercore.PreparedPlanCacheEnabled() {
		s.preparedPlanCache = kvcache.NewSimpleLRUCache(plannercore.PreparedPlanCacheCapacity,
			plannercore.PreparedPlanCacheMemoryGuardRatio, plannercore.PreparedPlanCacheMaxMemory.Load())
	}
	s.mu.values = make(map[fmt.Stringer]interface{})
	s.lockedTables = make(map[int64]model.TableLockTpInfo)
	domain.BindDomain(s, dom)
	// session implements variable.GlobalVarAccessor. Bind it to ctx.
	s.sessionVars.GlobalVarsAccessor = s
	s.sessionVars.BinlogClient = binloginfo.GetPumpsClient()
	s.txn.init()
	return s, nil
}

// createSessionWithDomain creates a new Session and binds it with a Domain.
// We need this because when we start DDL in Domain, the DDL need a session
// to change some system tables. But at that time, we have been already in
// a lock context, which cause we can't call createSesion directly.
func createSessionWithDomain(store kv.Storage, dom *domain.Domain) (*session, error) {
	s := &session{
		store:       store,
		parser:      parser.New(),
		sessionVars: variable.NewSessionVars(),
		client:      store.GetClient(),
	}
	if plannercore.PreparedPlanCacheEnabled() {
		s.preparedPlanCache = kvcache.NewSimpleLRUCache(plannercore.PreparedPlanCacheCapacity,
			plannercore.PreparedPlanCacheMemoryGuardRatio, plannercore.PreparedPlanCacheMaxMemory.Load())
	}
	s.mu.values = make(map[fmt.Stringer]interface{})
	s.lockedTables = make(map[int64]model.TableLockTpInfo)
	domain.BindDomain(s, dom)
	// session implements variable.GlobalVarAccessor. Bind it to ctx.
	s.sessionVars.GlobalVarsAccessor = s
	s.txn.init()
	return s, nil
}

const (
	notBootstrapped         = 0
	currentBootstrapVersion = 35
)

func getStoreBootstrapVersion(store kv.Storage) int64 {
	storeBootstrappedLock.Lock()
	defer storeBootstrappedLock.Unlock()
	// check in memory
	_, ok := storeBootstrapped[store.UUID()]
	if ok {
		return currentBootstrapVersion
	}

	var ver int64
	// check in kv store
	err := kv.RunInNewTxn(store, false, func(txn kv.Transaction) error {
		var err error
		t := meta.NewMeta(txn)
		ver, err = t.GetBootstrapVersion()
		return err
	})

	if err != nil {
		logutil.BgLogger().Fatal("check bootstrapped failed",
			zap.Error(err))
	}

	if ver > notBootstrapped {
		// here mean memory is not ok, but other server has already finished it
		storeBootstrapped[store.UUID()] = true
	}

	return ver
}

func finishBootstrap(store kv.Storage) {
	storeBootstrappedLock.Lock()
	storeBootstrapped[store.UUID()] = true
	storeBootstrappedLock.Unlock()

	err := kv.RunInNewTxn(store, true, func(txn kv.Transaction) error {
		t := meta.NewMeta(txn)
		err := t.FinishBootstrap(currentBootstrapVersion)
		return err
	})
	if err != nil {
		logutil.BgLogger().Fatal("finish bootstrap failed",
			zap.Error(err))
	}
}

const quoteCommaQuote = "', '"

var builtinGlobalVariable = []string{
	variable.AutoCommit,
	variable.SQLModeVar,
	variable.MaxAllowedPacket,
	variable.TimeZone,
	variable.BlockEncryptionMode,
	variable.WaitTimeout,
	variable.InteractiveTimeout,
	variable.MaxPreparedStmtCount,
	variable.InitConnect,
	variable.TxnIsolation,
	variable.TxReadOnly,
	variable.TransactionIsolation,
	variable.TransactionReadOnly,
	variable.NetBufferLength,
	variable.QueryCacheType,
	variable.QueryCacheSize,
	variable.CharacterSetServer,
	variable.AutoIncrementIncrement,
	variable.CollationServer,
	variable.NetWriteTimeout,
	variable.MaxExecutionTime,

	/* TiDB specific global variables: */
	variable.TiDBSkipUTF8Check,
	variable.TiDBIndexJoinBatchSize,
	variable.TiDBIndexLookupSize,
	variable.TiDBIndexLookupConcurrency,
	variable.TiDBIndexLookupJoinConcurrency,
	variable.TiDBIndexSerialScanConcurrency,
	variable.TiDBHashJoinConcurrency,
	variable.TiDBProjectionConcurrency,
	variable.TiDBHashAggPartialConcurrency,
	variable.TiDBHashAggFinalConcurrency,
	variable.TiDBBackoffLockFast,
	variable.TiDBBackOffWeight,
	variable.TiDBConstraintCheckInPlace,
	variable.TiDBDDLReorgWorkerCount,
	variable.TiDBDDLReorgBatchSize,
	variable.TiDBDDLErrorCountLimit,
	variable.TiDBOptInSubqToJoinAndAgg,
	variable.TiDBOptCorrelationThreshold,
	variable.TiDBOptCorrelationExpFactor,
	variable.TiDBOptCPUFactor,
	variable.TiDBOptCopCPUFactor,
	variable.TiDBOptNetworkFactor,
	variable.TiDBOptScanFactor,
	variable.TiDBOptDescScanFactor,
	variable.TiDBOptMemoryFactor,
	variable.TiDBOptConcurrencyFactor,
	variable.TiDBDistSQLScanConcurrency,
	variable.TiDBInitChunkSize,
	variable.TiDBMaxChunkSize,
	variable.TiDBEnableCascadesPlanner,
	variable.TiDBRetryLimit,
	variable.TiDBDisableTxnAutoRetry,
	variable.TiDBEnableWindowFunction,
	variable.TiDBEnableVectorizedExpression,
	variable.TiDBEnableFastAnalyze,
	variable.TiDBExpensiveQueryTimeThreshold,
	variable.TiDBEnableNoopFuncs,
	variable.TiDBEnableIndexMerge,
	variable.TiDBTxnMode,
	variable.TiDBEnableStmtSummary,
	variable.TiDBMaxDeltaSchemaCount,
	variable.TiDBUsePlanBaselines,
}

var (
	loadCommonGlobalVarsSQLOnce sync.Once
	loadCommonGlobalVarsSQL     string
)

func initLoadCommonGlobalVarsSQL() {
	loadCommonGlobalVarsSQLOnce.Do(func() {
		vars := append(make([]string, 0, len(builtinGlobalVariable)+len(variable.PluginVarNames)), builtinGlobalVariable...)
		if len(variable.PluginVarNames) > 0 {
			vars = append(vars, variable.PluginVarNames...)
		}
		loadCommonGlobalVarsSQL = "select HIGH_PRIORITY * from mysql.global_variables where variable_name in ('" + strings.Join(vars, quoteCommaQuote) + "')"
	})
}

// loadCommonGlobalVariablesIfNeeded loads and applies commonly used global variables for the session.
func (s *session) loadCommonGlobalVariablesIfNeeded() error {
	initLoadCommonGlobalVarsSQL()
	vars := s.sessionVars
	if vars.CommonGlobalLoaded {
		return nil
	}
	if s.Value(sessionctx.Initing) != nil {
		// When running bootstrap or upgrade, we should not access global storage.
		return nil
	}

	var err error
	// Use GlobalVariableCache if TiDB just loaded global variables within 2 second ago.
	// When a lot of connections connect to TiDB simultaneously, it can protect TiKV meta region from overload.
	gvc := domain.GetDomain(s).GetGlobalVarsCache()
	succ, rows, fields := gvc.Get()
	if !succ {
		// Set the variable to true to prevent cyclic recursive call.
		vars.CommonGlobalLoaded = true
		rows, fields, err = s.ExecRestrictedSQL(loadCommonGlobalVarsSQL)
		if err != nil {
			vars.CommonGlobalLoaded = false
			logutil.BgLogger().Error("failed to load common global variables.")
			return err
		}
		gvc.Update(rows, fields)
	}

	for _, row := range rows {
		varName := row.GetString(0)
		varVal := row.GetDatum(1, &fields[1].Column.FieldType)
		if _, ok := vars.GetSystemVar(varName); !ok {
			err = variable.SetSessionSystemVar(s.sessionVars, varName, varVal)
			if err != nil {
				return err
			}
		}
	}

	// when client set Capability Flags CLIENT_INTERACTIVE, init wait_timeout with interactive_timeout
	if vars.ClientCapability&mysql.ClientInteractive > 0 {
		if varVal, ok := vars.GetSystemVar(variable.InteractiveTimeout); ok {
			if err := vars.SetSystemVar(variable.WaitTimeout, varVal); err != nil {
				return err
			}
		}
	}

	vars.CommonGlobalLoaded = true
	return nil
}

// PrepareTxnCtx starts a goroutine to begin a transaction if needed, and creates a new transaction context.
// It is called before we execute a sql query.
func (s *session) PrepareTxnCtx(ctx context.Context) {
	if s.txn.validOrPending() {
		return
	}

	is := domain.GetDomain(s).InfoSchema()
	s.sessionVars.TxnCtx = &variable.TransactionContext{
		InfoSchema:    is,
		SchemaVersion: is.SchemaMetaVersion(),
		CreateTime:    time.Now(),
	}
	if !s.sessionVars.IsAutocommit() {
		pessTxnConf := config.GetGlobalConfig().PessimisticTxn
		if pessTxnConf.Enable {
			if s.sessionVars.TxnMode == ast.Pessimistic {
				s.sessionVars.TxnCtx.IsPessimistic = true
			}
		}
	}
}

// PrepareTxnFuture uses to try to get txn future.
func (s *session) PrepareTxnFuture(ctx context.Context) {
	if s.txn.validOrPending() {
		return
	}

	txnFuture := s.getTxnFuture(ctx)
	s.txn.changeInvalidToPending(txnFuture)
}

// RefreshTxnCtx implements context.RefreshTxnCtx interface.
func (s *session) RefreshTxnCtx(ctx context.Context) error {
	if err := s.doCommit(ctx); err != nil {
		return err
	}

	return s.NewTxn(ctx)
}

// InitTxnWithStartTS create a transaction with startTS.
func (s *session) InitTxnWithStartTS(startTS uint64) error {
	if s.txn.Valid() {
		return nil
	}

	// no need to get txn from txnFutureCh since txn should init with startTs
	txn, err := s.store.BeginWithStartTS(startTS)
	if err != nil {
		return err
	}
	s.txn.changeInvalidToValid(txn)
	s.txn.SetCap(s.getMembufCap())
	err = s.loadCommonGlobalVariablesIfNeeded()
	if err != nil {
		return err
	}
	return nil
}

// GetStore gets the store of session.
func (s *session) GetStore() kv.Storage {
	return s.store
}

func (s *session) ShowProcess() *util.ProcessInfo {
	var pi *util.ProcessInfo
	tmp := s.processInfo.Load()
	if tmp != nil {
		pi = tmp.(*util.ProcessInfo)
	}
	return pi
}

// logStmt logs some crucial SQL including: CREATE USER/GRANT PRIVILEGE/CHANGE PASSWORD/DDL etc and normal SQL
// if variable.ProcessGeneralLog is set.
func logStmt(node ast.StmtNode, vars *variable.SessionVars) {
	switch stmt := node.(type) {
	case *ast.CreateUserStmt, *ast.DropUserStmt, *ast.AlterUserStmt, *ast.SetPwdStmt, *ast.GrantStmt,
		*ast.RevokeStmt, *ast.AlterTableStmt, *ast.CreateDatabaseStmt, *ast.CreateIndexStmt, *ast.CreateTableStmt,
		*ast.DropDatabaseStmt, *ast.DropIndexStmt, *ast.DropTableStmt, *ast.RenameTableStmt, *ast.TruncateTableStmt:
		user := vars.User
		schemaVersion := vars.TxnCtx.SchemaVersion
		if ss, ok := node.(ast.SensitiveStmtNode); ok {
			logutil.BgLogger().Info("CRUCIAL OPERATION",
				zap.Uint64("conn", vars.ConnectionID),
				zap.Int64("schemaVersion", schemaVersion),
				zap.String("secure text", ss.SecureText()),
				zap.Stringer("user", user))
		} else {
			logutil.BgLogger().Info("CRUCIAL OPERATION",
				zap.Uint64("conn", vars.ConnectionID),
				zap.Int64("schemaVersion", schemaVersion),
				zap.String("cur_db", vars.CurrentDB),
				zap.String("sql", stmt.Text()),
				zap.Stringer("user", user))
		}
	default:
		logQuery(node.Text(), vars)
	}
}

func logQuery(query string, vars *variable.SessionVars) {
	if atomic.LoadUint32(&variable.ProcessGeneralLog) != 0 && !vars.InRestrictedSQL {
		query = executor.QueryReplacer.Replace(query)
		logutil.BgLogger().Info("GENERAL_LOG",
			zap.Uint64("conn", vars.ConnectionID),
			zap.Stringer("user", vars.User),
			zap.Int64("schemaVersion", vars.TxnCtx.SchemaVersion),
			zap.Uint64("txnStartTS", vars.TxnCtx.StartTS),
			zap.String("current_db", vars.CurrentDB),
			zap.String("sql", query+vars.PreparedParams.String()))
	}
}

func (s *session) recordOnTransactionExecution(err error, counter int, duration float64) {
	if s.isInternal() {
		if err != nil {
			statementPerTransactionInternalError.Observe(float64(counter))
			transactionDurationInternalError.Observe(duration)
		} else {
			statementPerTransactionInternalOK.Observe(float64(counter))
			transactionDurationInternalOK.Observe(duration)
		}
	} else {
		if err != nil {
			statementPerTransactionGeneralError.Observe(float64(counter))
			transactionDurationGeneralError.Observe(duration)
		} else {
			statementPerTransactionGeneralOK.Observe(float64(counter))
			transactionDurationGeneralOK.Observe(duration)
		}
	}
}

func (s *session) recordTransactionCounter(stmtNode ast.StmtNode, err error) {
	if stmtNode == nil {
		if s.isInternal() {
			if err != nil {
				transactionCounterInternalErr.Inc()
			} else {
				transactionCounterInternalOK.Inc()
			}
		} else {
			if err != nil {
				transactionCounterGeneralErr.Inc()
			} else {
				transactionCounterGeneralOK.Inc()
			}
		}
		return
	}

	var isTxn bool
	switch stmtNode.(type) {
	case *ast.CommitStmt:
		isTxn = true
	case *ast.RollbackStmt:
		isTxn = true
	}
	if !isTxn {
		return
	}
	if s.isInternal() {
		transactionCounterInternalCommitRollback.Inc()
	} else {
		transactionCounterGeneralCommitRollback.Inc()
	}
}

type multiQueryNoDelayRecordSet struct {
	sqlexec.RecordSet

	affectedRows uint64
	lastMessage  string
	status       uint16
	warnCount    uint16
	lastInsertID uint64
}

func (c *multiQueryNoDelayRecordSet) Close() error {
	return nil
}

func (c *multiQueryNoDelayRecordSet) AffectedRows() uint64 {
	return c.affectedRows
}

func (c *multiQueryNoDelayRecordSet) LastMessage() string {
	return c.lastMessage
}

func (c *multiQueryNoDelayRecordSet) WarnCount() uint16 {
	return c.warnCount
}

func (c *multiQueryNoDelayRecordSet) Status() uint16 {
	return c.status
}

func (c *multiQueryNoDelayRecordSet) LastInsertID() uint64 {
	return c.lastInsertID
}
