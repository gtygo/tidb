// Copyright 2017 PingCAP, Inc.
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

package core_test

import (
	"context"

	. "github.com/pingcap/check"
	"github.com/pingcap/parser"
	"github.com/pingcap/parser/model"
	"github.com/pingcap/parser/terror"
	"github.com/pingcap/tidb/infoschema"
	"github.com/pingcap/tidb/kv"
	"github.com/pingcap/tidb/planner"
	"github.com/pingcap/tidb/planner/core"
	"github.com/pingcap/tidb/session"
	"github.com/pingcap/tidb/sessionctx"
	"github.com/pingcap/tidb/sessionctx/stmtctx"
	"github.com/pingcap/tidb/util/testleak"
	"github.com/pingcap/tidb/util/testutil"
)

var _ = Suite(&testPlanSuite{})

type testPlanSuite struct {
	*parser.Parser
	is infoschema.InfoSchema

	testData testutil.TestData
}

func (s *testPlanSuite) SetUpSuite(c *C) {
	s.is = infoschema.MockInfoSchema([]*model.TableInfo{core.MockSignedTable(), core.MockUnsignedTable()})
	s.Parser = parser.New()
	s.Parser.EnableWindowFunc(true)

	var err error
	s.testData, err = testutil.LoadTestSuiteData("testdata", "plan_suite")
	c.Assert(err, IsNil)
}

func (s *testPlanSuite) TearDownSuite(c *C) {
	c.Assert(s.testData.GenerateOutputIfNeeded(), IsNil)
}

func (s *testPlanSuite) TestDAGPlanBuilderSimpleCase(c *C) {
	defer testleak.AfterTest(c)()
	store, dom, err := newStoreWithBootstrap()
	c.Assert(err, IsNil)
	defer func() {
		dom.Close()
		store.Close()
	}()
	se, err := session.CreateSession4Test(store)
	c.Assert(err, IsNil)
	_, err = se.Execute(context.Background(), "use test")
	c.Assert(err, IsNil)
	var input []string
	var output []struct {
		SQL  string
		Best string
	}
	s.testData.GetTestCases(c, &input, &output)
	for i, tt := range input {
		comment := Commentf("case:%v sql:%s", i, tt)
		stmt, err := s.ParseOneStmt(tt, "", "")
		c.Assert(err, IsNil, comment)

		err = se.NewTxn(context.Background())
		c.Assert(err, IsNil)
		p, err := planner.Optimize(context.TODO(), se, stmt, s.is)
		c.Assert(err, IsNil)
		s.testData.OnRecord(func() {
			output[i].SQL = tt
			output[i].Best = core.ToString(p)
		})
		c.Assert(core.ToString(p), Equals, output[i].Best, comment)
	}
}

func (s *testPlanSuite) TestDAGPlanBuilderJoin(c *C) {
	defer testleak.AfterTest(c)()
	store, dom, err := newStoreWithBootstrap()
	c.Assert(err, IsNil)
	defer func() {
		dom.Close()
		store.Close()
	}()
	se, err := session.CreateSession4Test(store)
	c.Assert(err, IsNil)
	_, err = se.Execute(context.Background(), "use test")
	c.Assert(err, IsNil)

	var input []string
	var output []struct {
		SQL  string
		Best string
	}
	s.testData.GetTestCases(c, &input, &output)
	for i, tt := range input {
		comment := Commentf("case:%v sql:%s", i, tt)
		stmt, err := s.ParseOneStmt(tt, "", "")
		c.Assert(err, IsNil, comment)

		p, err := planner.Optimize(context.TODO(), se, stmt, s.is)
		c.Assert(err, IsNil)
		s.testData.OnRecord(func() {
			output[i].SQL = tt
			output[i].Best = core.ToString(p)
		})
		c.Assert(core.ToString(p), Equals, output[i].Best, comment)
	}
}

func (s *testPlanSuite) TestDAGPlanBuilderSubquery(c *C) {
	defer testleak.AfterTest(c)()
	store, dom, err := newStoreWithBootstrap()
	c.Assert(err, IsNil)
	defer func() {
		dom.Close()
		store.Close()
	}()
	se, err := session.CreateSession4Test(store)
	c.Assert(err, IsNil)
	_, err = se.Execute(context.Background(), "use test")
	c.Assert(err, IsNil)
	se.Execute(context.Background(), "set sql_mode='STRICT_TRANS_TABLES'") // disable only full group by
	ctx := se.(sessionctx.Context)
	sessionVars := ctx.GetSessionVars()
	sessionVars.HashAggFinalConcurrency = 1
	sessionVars.HashAggPartialConcurrency = 1
	var input []string
	var output []struct {
		SQL  string
		Best string
	}
	s.testData.GetTestCases(c, &input, &output)
	for i, tt := range input {
		comment := Commentf("for %s", tt)
		stmt, err := s.ParseOneStmt(tt, "", "")
		c.Assert(err, IsNil, comment)

		p, err := planner.Optimize(context.TODO(), se, stmt, s.is)
		c.Assert(err, IsNil)
		s.testData.OnRecord(func() {
			output[i].SQL = tt
			output[i].Best = core.ToString(p)
		})
		c.Assert(core.ToString(p), Equals, output[i].Best, Commentf("for %s", tt))
	}
}

func (s *testPlanSuite) TestDAGPlanTopN(c *C) {
	defer testleak.AfterTest(c)()
	store, dom, err := newStoreWithBootstrap()
	c.Assert(err, IsNil)
	defer func() {
		dom.Close()
		store.Close()
	}()
	se, err := session.CreateSession4Test(store)
	c.Assert(err, IsNil)
	_, err = se.Execute(context.Background(), "use test")
	c.Assert(err, IsNil)

	var input []string
	var output []struct {
		SQL  string
		Best string
	}
	s.testData.GetTestCases(c, &input, &output)
	for i, tt := range input {
		comment := Commentf("case:%v sql:%s", i, tt)
		stmt, err := s.ParseOneStmt(tt, "", "")
		c.Assert(err, IsNil, comment)

		p, err := planner.Optimize(context.TODO(), se, stmt, s.is)
		c.Assert(err, IsNil)
		s.testData.OnRecord(func() {
			output[i].SQL = tt
			output[i].Best = core.ToString(p)
		})
		c.Assert(core.ToString(p), Equals, output[i].Best, comment)
	}
}

func (s *testPlanSuite) TestDAGPlanBuilderBasePhysicalPlan(c *C) {
	defer testleak.AfterTest(c)()
	store, dom, err := newStoreWithBootstrap()
	c.Assert(err, IsNil)
	defer func() {
		dom.Close()
		store.Close()
	}()
	se, err := session.CreateSession4Test(store)
	c.Assert(err, IsNil)

	_, err = se.Execute(context.Background(), "use test")
	c.Assert(err, IsNil)

	var input []string
	var output []struct {
		SQL   string
		Best  string
		Hints string
	}
	s.testData.GetTestCases(c, &input, &output)
	for i, tt := range input {
		comment := Commentf("for %s", tt)
		stmt, err := s.ParseOneStmt(tt, "", "")
		c.Assert(err, IsNil, comment)

		core.Preprocess(se, stmt, s.is)
		p, err := planner.Optimize(context.TODO(), se, stmt, s.is)
		c.Assert(err, IsNil)
		s.testData.OnRecord(func() {
			output[i].SQL = tt
			output[i].Best = core.ToString(p)
			output[i].Hints = core.GenHintsFromPhysicalPlan(p)
		})
		c.Assert(core.ToString(p), Equals, output[i].Best, Commentf("for %s", tt))
		c.Assert(core.GenHintsFromPhysicalPlan(p), Equals, output[i].Hints, Commentf("for %s", tt))
	}
}

func (s *testPlanSuite) TestDAGPlanBuilderUnion(c *C) {
	defer testleak.AfterTest(c)()
	store, dom, err := newStoreWithBootstrap()
	c.Assert(err, IsNil)
	defer func() {
		dom.Close()
		store.Close()
	}()
	se, err := session.CreateSession4Test(store)
	c.Assert(err, IsNil)
	_, err = se.Execute(context.Background(), "use test")
	c.Assert(err, IsNil)

	var input []string
	var output []struct {
		SQL  string
		Best string
	}
	s.testData.GetTestCases(c, &input, &output)
	for i, tt := range input {
		comment := Commentf("case:%v sql:%s", i, tt)
		stmt, err := s.ParseOneStmt(tt, "", "")
		c.Assert(err, IsNil, comment)

		p, err := planner.Optimize(context.TODO(), se, stmt, s.is)
		c.Assert(err, IsNil)
		s.testData.OnRecord(func() {
			output[i].SQL = tt
			output[i].Best = core.ToString(p)
		})
		c.Assert(core.ToString(p), Equals, output[i].Best, comment)
	}
}

func (s *testPlanSuite) TestDAGPlanBuilderUnionScan(c *C) {
	defer testleak.AfterTest(c)()
	store, dom, err := newStoreWithBootstrap()
	c.Assert(err, IsNil)
	defer func() {
		dom.Close()
		store.Close()
	}()
	se, err := session.CreateSession4Test(store)
	c.Assert(err, IsNil)
	_, err = se.Execute(context.Background(), "use test")
	c.Assert(err, IsNil)

	var input []string
	var output []struct {
		SQL  string
		Best string
	}
	for i, tt := range input {
		comment := Commentf("for %s", tt)
		stmt, err := s.ParseOneStmt(tt, "", "")
		c.Assert(err, IsNil, comment)

		err = se.NewTxn(context.Background())
		c.Assert(err, IsNil)
		// Make txn not read only.
		txn, err := se.Txn(true)
		c.Assert(err, IsNil)
		txn.Set(kv.Key("AAA"), []byte("BBB"))
		c.Assert(se.StmtCommit(), IsNil)
		p, err := planner.Optimize(context.TODO(), se, stmt, s.is)
		c.Assert(err, IsNil)
		s.testData.OnRecord(func() {
			output[i].SQL = tt
			output[i].Best = core.ToString(p)
		})
		c.Assert(core.ToString(p), Equals, output[i].Best, Commentf("for %s", tt))
	}
}

func (s *testPlanSuite) TestDAGPlanBuilderAgg(c *C) {
	defer testleak.AfterTest(c)()
	store, dom, err := newStoreWithBootstrap()
	c.Assert(err, IsNil)
	defer func() {
		dom.Close()
		store.Close()
	}()
	se, err := session.CreateSession4Test(store)
	c.Assert(err, IsNil)
	se.Execute(context.Background(), "use test")
	se.Execute(context.Background(), "set sql_mode='STRICT_TRANS_TABLES'") // disable only full group by
	c.Assert(err, IsNil)
	ctx := se.(sessionctx.Context)
	sessionVars := ctx.GetSessionVars()
	sessionVars.HashAggFinalConcurrency = 1
	sessionVars.HashAggPartialConcurrency = 1

	var input []string
	var output []struct {
		SQL  string
		Best string
	}
	s.testData.GetTestCases(c, &input, &output)
	for i, tt := range input {
		comment := Commentf("for %s", tt)
		stmt, err := s.ParseOneStmt(tt, "", "")
		c.Assert(err, IsNil, comment)

		p, err := planner.Optimize(context.TODO(), se, stmt, s.is)
		c.Assert(err, IsNil)
		s.testData.OnRecord(func() {
			output[i].SQL = tt
			output[i].Best = core.ToString(p)
		})
		c.Assert(core.ToString(p), Equals, output[i].Best, Commentf("for %s", tt))
	}
}

func (s *testPlanSuite) TestRefine(c *C) {
	defer testleak.AfterTest(c)()
	store, dom, err := newStoreWithBootstrap()
	c.Assert(err, IsNil)
	defer func() {
		dom.Close()
		store.Close()
	}()
	se, err := session.CreateSession4Test(store)
	c.Assert(err, IsNil)
	_, err = se.Execute(context.Background(), "use test")
	c.Assert(err, IsNil)

	var input []string
	var output []struct {
		SQL  string
		Best string
	}
	s.testData.GetTestCases(c, &input, &output)
	for i, tt := range input {
		comment := Commentf("for %s", tt)
		stmt, err := s.ParseOneStmt(tt, "", "")
		c.Assert(err, IsNil, comment)
		sc := se.(sessionctx.Context).GetSessionVars().StmtCtx
		sc.IgnoreTruncate = false
		p, err := planner.Optimize(context.TODO(), se, stmt, s.is)
		c.Assert(err, IsNil, comment)
		s.testData.OnRecord(func() {
			output[i].SQL = tt
			output[i].Best = core.ToString(p)
		})
		c.Assert(core.ToString(p), Equals, output[i].Best, comment)
	}
}

func (s *testPlanSuite) TestAggEliminator(c *C) {
	defer testleak.AfterTest(c)()
	store, dom, err := newStoreWithBootstrap()
	c.Assert(err, IsNil)
	defer func() {
		dom.Close()
		store.Close()
	}()
	se, err := session.CreateSession4Test(store)
	c.Assert(err, IsNil)
	_, err = se.Execute(context.Background(), "use test")
	c.Assert(err, IsNil)
	se.Execute(context.Background(), "set sql_mode='STRICT_TRANS_TABLES'") // disable only full group by
	var input []string
	var output []struct {
		SQL  string
		Best string
	}
	s.testData.GetTestCases(c, &input, &output)
	for i, tt := range input {
		comment := Commentf("for %s", tt)
		stmt, err := s.ParseOneStmt(tt, "", "")
		c.Assert(err, IsNil, comment)
		sc := se.(sessionctx.Context).GetSessionVars().StmtCtx
		sc.IgnoreTruncate = false
		p, err := planner.Optimize(context.TODO(), se, stmt, s.is)
		c.Assert(err, IsNil)
		s.testData.OnRecord(func() {
			output[i].SQL = tt
			output[i].Best = core.ToString(p)
		})
		c.Assert(core.ToString(p), Equals, output[i].Best, Commentf("for %s", tt))
	}
}

type overrideStore struct{ kv.Storage }

func (store overrideStore) GetClient() kv.Client {
	cli := store.Storage.GetClient()
	return overrideClient{cli}
}

type overrideClient struct{ kv.Client }

func (cli overrideClient) IsRequestTypeSupported(reqType, subType int64) bool {
	return false
}

func (s *testPlanSuite) TestRequestTypeSupportedOff(c *C) {
	defer testleak.AfterTest(c)()
	store, dom, err := newStoreWithBootstrap()
	c.Assert(err, IsNil)
	defer func() {
		dom.Close()
		store.Close()
	}()
	se, err := session.CreateSession4Test(overrideStore{store})
	c.Assert(err, IsNil)
	_, err = se.Execute(context.Background(), "use test")
	c.Assert(err, IsNil)

	sql := "select * from t where a in (1, 10, 20)"
	expect := "TableReader(Table(t))->Sel([in(Column#1, 1, 10, 20)])"

	stmt, err := s.ParseOneStmt(sql, "", "")
	c.Assert(err, IsNil)
	p, err := planner.Optimize(context.TODO(), se, stmt, s.is)
	c.Assert(err, IsNil)
	c.Assert(core.ToString(p), Equals, expect, Commentf("for %s", sql))
}

func (s *testPlanSuite) TestIndexJoinUnionScan(c *C) {
	defer testleak.AfterTest(c)()
	store, dom, err := newStoreWithBootstrap()
	c.Assert(err, IsNil)
	defer func() {
		dom.Close()
		store.Close()
	}()
	se, err := session.CreateSession4Test(store)
	c.Assert(err, IsNil)
	_, err = se.Execute(context.Background(), "use test")
	c.Assert(err, IsNil)

	var input []string
	var output []struct {
		SQL  string
		Best string
	}
	s.testData.GetTestCases(c, &input, &output)
	for i, tt := range input {
		comment := Commentf("case:%v sql:%s", i, tt)
		stmt, err := s.ParseOneStmt(tt, "", "")
		c.Assert(err, IsNil, comment)
		err = se.NewTxn(context.Background())
		c.Assert(err, IsNil)
		// Make txn not read only.
		txn, err := se.Txn(true)
		c.Assert(err, IsNil)
		txn.Set(kv.Key("AAA"), []byte("BBB"))
		c.Assert(se.StmtCommit(), IsNil)
		p, err := planner.Optimize(context.TODO(), se, stmt, s.is)
		c.Assert(err, IsNil, comment)
		s.testData.OnRecord(func() {
			output[i].SQL = tt
			output[i].Best = core.ToString(p)
		})
		c.Assert(core.ToString(p), Equals, output[i].Best, Commentf("for %s", tt))
	}

	definitions := []model.PartitionDefinition{
		{
			ID:       41,
			Name:     model.NewCIStr("p1"),
			LessThan: []string{"16"},
		},
		{
			ID:       42,
			Name:     model.NewCIStr("p2"),
			LessThan: []string{"32"},
		},
	}
	pis := core.MockPartitionInfoSchema(definitions)

	tests := []struct {
		sql  string
		best string
	}{
		// Index Join + Union Scan + Union All is not supported now.
		{
			sql:  "select /*+ TIDB_INLJ(t2) */ * from t t1, t t2 where t1.a = t2.a",
			best: "LeftHashJoin{UnionAll{TableReader(Table(t))->UnionScan([])->TableReader(Table(t))->UnionScan([])}->UnionAll{TableReader(Table(t))->UnionScan([])->TableReader(Table(t))->UnionScan([])}}(Column#1,Column#14)",
		},
	}
	for i, tt := range tests {
		comment := Commentf("case:%v sql:%s", i, tt.sql)
		stmt, err := s.ParseOneStmt(tt.sql, "", "")
		c.Assert(err, IsNil, comment)
		err = se.NewTxn(context.Background())
		c.Assert(err, IsNil)
		// Make txn not read only.
		txn, err := se.Txn(true)
		c.Assert(err, IsNil)
		txn.Set(kv.Key("AAA"), []byte("BBB"))
		c.Assert(se.StmtCommit(), IsNil)
		p, err := planner.Optimize(context.TODO(), se, stmt, pis)
		c.Assert(err, IsNil, comment)
		c.Assert(core.ToString(p), Equals, tt.best, comment)
	}
}

func (s *testPlanSuite) TestDoSubquery(c *C) {
	defer testleak.AfterTest(c)()
	store, dom, err := newStoreWithBootstrap()
	c.Assert(err, IsNil)
	defer func() {
		dom.Close()
		store.Close()
	}()
	se, err := session.CreateSession4Test(store)
	c.Assert(err, IsNil)
	_, err = se.Execute(context.Background(), "use test")
	c.Assert(err, IsNil)
	tests := []struct {
		sql  string
		best string
	}{
		{
			sql:  "do 1 in (select a from t)",
			best: "LeftHashJoin{Dual->TableReader(Table(t))}->Projection",
		},
	}
	for _, tt := range tests {
		comment := Commentf("for %s", tt.sql)
		stmt, err := s.ParseOneStmt(tt.sql, "", "")
		c.Assert(err, IsNil, comment)
		p, err := planner.Optimize(context.TODO(), se, stmt, s.is)
		c.Assert(err, IsNil)
		c.Assert(core.ToString(p), Equals, tt.best, comment)
	}
}

func (s *testPlanSuite) TestIndexLookupCartesianJoin(c *C) {
	defer testleak.AfterTest(c)()
	store, dom, err := newStoreWithBootstrap()
	c.Assert(err, IsNil)
	defer func() {
		dom.Close()
		store.Close()
	}()
	se, err := session.CreateSession4Test(store)
	c.Assert(err, IsNil)
	_, err = se.Execute(context.Background(), "use test")
	c.Assert(err, IsNil)
	sql := "select /*+ TIDB_INLJ(t1, t2) */ * from t t1 join t t2"
	stmt, err := s.ParseOneStmt(sql, "", "")
	c.Assert(err, IsNil)
	p, err := planner.Optimize(context.TODO(), se, stmt, s.is)
	c.Assert(err, IsNil)
	c.Assert(core.ToString(p), Equals, "LeftHashJoin{TableReader(Table(t))->TableReader(Table(t))}")
	warnings := se.GetSessionVars().StmtCtx.GetWarnings()
	lastWarn := warnings[len(warnings)-1]
	err = core.ErrInternal.GenWithStack("TIDB_INLJ hint is inapplicable without column equal ON condition")
	c.Assert(terror.ErrorEqual(err, lastWarn.Err), IsTrue)
}

func (s *testPlanSuite) TestSemiJoinToInner(c *C) {
	defer testleak.AfterTest(c)()
	store, dom, err := newStoreWithBootstrap()
	c.Assert(err, IsNil)
	defer func() {
		dom.Close()
		store.Close()
	}()
	se, err := session.CreateSession4Test(store)
	c.Assert(err, IsNil)
	_, err = se.Execute(context.Background(), "use test")
	c.Assert(err, IsNil)
	var input []string
	var output []struct {
		SQL  string
		Best string
	}
	s.testData.GetTestCases(c, &input, &output)
	for i, tt := range input {
		stmt, err := s.ParseOneStmt(tt, "", "")
		c.Assert(err, IsNil)
		p, err := planner.Optimize(context.TODO(), se, stmt, s.is)
		c.Assert(err, IsNil)
		s.testData.OnRecord(func() {
			output[i].SQL = tt
			output[i].Best = core.ToString(p)
		})
		c.Assert(core.ToString(p), Equals, output[i].Best)
	}
}

func (s *testPlanSuite) TestUnmatchedTableInHint(c *C) {
	defer testleak.AfterTest(c)()
	store, dom, err := newStoreWithBootstrap()
	c.Assert(err, IsNil)
	defer func() {
		dom.Close()
		store.Close()
	}()
	se, err := session.CreateSession4Test(store)
	c.Assert(err, IsNil)
	_, err = se.Execute(context.Background(), "use test")
	c.Assert(err, IsNil)
	var input []string
	var output []struct {
		SQL     string
		Warning string
	}
	s.testData.GetTestCases(c, &input, &output)
	for i, test := range input {
		se.GetSessionVars().StmtCtx.SetWarnings(nil)
		stmt, err := s.ParseOneStmt(test, "", "")
		c.Assert(err, IsNil)
		_, err = planner.Optimize(context.TODO(), se, stmt, s.is)
		c.Assert(err, IsNil)
		warnings := se.GetSessionVars().StmtCtx.GetWarnings()
		s.testData.OnRecord(func() {
			output[i].SQL = test
			if len(warnings) > 0 {
				output[i].Warning = warnings[0].Err.Error()
			}
		})
		if output[i].Warning == "" {
			c.Assert(len(warnings), Equals, 0)
		} else {
			c.Assert(len(warnings), Equals, 1)
			c.Assert(warnings[0].Level, Equals, stmtctx.WarnLevelWarning)
			c.Assert(warnings[0].Err.Error(), Equals, output[i].Warning)
		}
	}
}

func (s *testPlanSuite) TestHintScope(c *C) {
	defer testleak.AfterTest(c)()
	store, dom, err := newStoreWithBootstrap()
	c.Assert(err, IsNil)
	defer func() {
		dom.Close()
		store.Close()
	}()
	se, err := session.CreateSession4Test(store)
	c.Assert(err, IsNil)
	_, err = se.Execute(context.Background(), "use test")
	c.Assert(err, IsNil)

	var input []string
	var output []struct {
		SQL  string
		Best string
	}
	s.testData.GetTestCases(c, &input, &output)
	for i, test := range input {
		comment := Commentf("case:%v sql:%s", i, test)
		stmt, err := s.ParseOneStmt(test, "", "")
		c.Assert(err, IsNil, comment)

		p, err := planner.Optimize(context.Background(), se, stmt, s.is)
		c.Assert(err, IsNil)
		s.testData.OnRecord(func() {
			output[i].SQL = test
			output[i].Best = core.ToString(p)
		})
		c.Assert(core.ToString(p), Equals, output[i].Best)

		warnings := se.GetSessionVars().StmtCtx.GetWarnings()
		c.Assert(warnings, HasLen, 0, comment)
	}
}

func (s *testPlanSuite) TestJoinHints(c *C) {
	defer testleak.AfterTest(c)()
	store, dom, err := newStoreWithBootstrap()
	c.Assert(err, IsNil)
	defer func() {
		dom.Close()
		store.Close()
	}()
	se, err := session.CreateSession4Test(store)
	c.Assert(err, IsNil)
	_, err = se.Execute(context.Background(), "use test")
	c.Assert(err, IsNil)

	var input []string
	var output []struct {
		SQL     string
		Best    string
		Warning string
		Hints   string
	}
	s.testData.GetTestCases(c, &input, &output)
	ctx := context.Background()
	for i, test := range input {
		comment := Commentf("case:%v sql:%s", i, test)
		stmt, err := s.ParseOneStmt(test, "", "")
		c.Assert(err, IsNil, comment)

		se.GetSessionVars().StmtCtx.SetWarnings(nil)
		p, err := planner.Optimize(ctx, se, stmt, s.is)
		c.Assert(err, IsNil)
		warnings := se.GetSessionVars().StmtCtx.GetWarnings()

		s.testData.OnRecord(func() {
			output[i].SQL = test
			output[i].Best = core.ToString(p)
			if len(warnings) > 0 {
				output[i].Warning = warnings[0].Err.Error()
			}
			output[i].Hints = core.GenHintsFromPhysicalPlan(p)
		})
		c.Assert(core.ToString(p), Equals, output[i].Best)
		if output[i].Warning == "" {
			c.Assert(len(warnings), Equals, 0)
		} else {
			c.Assert(len(warnings), Equals, 1, Commentf("%v", warnings))
			c.Assert(warnings[0].Level, Equals, stmtctx.WarnLevelWarning)
			c.Assert(warnings[0].Err.Error(), Equals, output[i].Warning)
		}
		c.Assert(core.GenHintsFromPhysicalPlan(p), Equals, output[i].Hints, comment)
	}
}

func (s *testPlanSuite) TestAggregationHints(c *C) {
	defer testleak.AfterTest(c)()
	store, dom, err := newStoreWithBootstrap()
	c.Assert(err, IsNil)
	defer func() {
		dom.Close()
		store.Close()
	}()
	se, err := session.CreateSession4Test(store)
	c.Assert(err, IsNil)
	_, err = se.Execute(context.Background(), "use test")
	c.Assert(err, IsNil)

	sessionVars := se.(sessionctx.Context).GetSessionVars()
	sessionVars.HashAggFinalConcurrency = 1
	sessionVars.HashAggPartialConcurrency = 1

	var input []struct {
		SQL         string
		AggPushDown bool
	}
	var output []struct {
		SQL     string
		Best    string
		Warning string
	}
	s.testData.GetTestCases(c, &input, &output)
	ctx := context.Background()
	for i, test := range input {
		comment := Commentf("case:%v sql:%s", i, test)
		se.GetSessionVars().StmtCtx.SetWarnings(nil)
		se.GetSessionVars().AllowAggPushDown = test.AggPushDown

		stmt, err := s.ParseOneStmt(test.SQL, "", "")
		c.Assert(err, IsNil, comment)

		p, err := planner.Optimize(ctx, se, stmt, s.is)
		c.Assert(err, IsNil)
		warnings := se.GetSessionVars().StmtCtx.GetWarnings()

		s.testData.OnRecord(func() {
			output[i].SQL = test.SQL
			output[i].Best = core.ToString(p)
			if len(warnings) > 0 {
				output[i].Warning = warnings[0].Err.Error()
			}
		})
		c.Assert(core.ToString(p), Equals, output[i].Best, comment)
		if output[i].Warning == "" {
			c.Assert(len(warnings), Equals, 0, comment)
		} else {
			c.Assert(len(warnings), Equals, 1, comment)
			c.Assert(warnings[0].Level, Equals, stmtctx.WarnLevelWarning, comment)
			c.Assert(warnings[0].Err.Error(), Equals, output[i].Warning, comment)
		}
	}
}

func (s *testPlanSuite) TestAggToCopHint(c *C) {
	defer testleak.AfterTest(c)()
	store, dom, err := newStoreWithBootstrap()
	c.Assert(err, IsNil)
	defer func() {
		dom.Close()
		store.Close()
	}()
	se, err := session.CreateSession4Test(store)
	c.Assert(err, IsNil)
	_, err = se.Execute(context.Background(), "use test")
	c.Assert(err, IsNil)
	_, err = se.Execute(context.Background(), "insert into mysql.opt_rule_blacklist values(\"aggregation_eliminate\")")
	c.Assert(err, IsNil)
	_, err = se.Execute(context.Background(), "admin reload opt_rule_blacklist")
	c.Assert(err, IsNil)

	tests := []struct {
		sql     string
		best    string
		warning string
	}{
		{
			sql:  "select /*+ AGG_TO_COP(), HASH_AGG(), USE_INDEX(t) */ sum(a) from t group by a",
			best: "TableReader(Table(t)->HashAgg)->HashAgg",
		},
		{
			sql:  "select /*+ AGG_TO_COP(), USE_INDEX(t) */ sum(b) from t group by b",
			best: "TableReader(Table(t)->HashAgg)->HashAgg",
		},
		{
			sql:     "select /*+ AGG_TO_COP(), HASH_AGG(), USE_INDEX(t) */ distinct a from t group by a",
			best:    "TableReader(Table(t)->HashAgg)->HashAgg->HashAgg",
			warning: "[planner:1815]Optimizer Hint AGG_TO_COP is inapplicable",
		},
		{
			sql:     "select /*+ AGG_TO_COP(), HASH_AGG(), HASH_JOIN(t1), USE_INDEX(t1), USE_INDEX(t2) */ sum(t1.a) from t t1, t t2 where t1.a = t2.b group by t1.a",
			best:    "LeftHashJoin{TableReader(Table(t))->TableReader(Table(t))}(Column#1,Column#14)->Projection->HashAgg",
			warning: "[planner:1815]Optimizer Hint AGG_TO_COP is inapplicable",
		},
	}
	ctx := context.Background()
	for i, test := range tests {
		comment := Commentf("case:%v sql:%s", i, test)
		se.GetSessionVars().StmtCtx.SetWarnings(nil)

		stmt, err := s.ParseOneStmt(test.sql, "", "")
		c.Assert(err, IsNil, comment)

		p, err := planner.Optimize(ctx, se, stmt, s.is)
		c.Assert(err, IsNil)
		c.Assert(core.ToString(p), Equals, test.best, comment)

		warnings := se.GetSessionVars().StmtCtx.GetWarnings()
		if test.warning == "" {
			c.Assert(len(warnings), Equals, 0, comment)
		} else {
			c.Assert(len(warnings), Equals, 1, comment)
			c.Assert(warnings[0].Level, Equals, stmtctx.WarnLevelWarning, comment)
			c.Assert(warnings[0].Err.Error(), Equals, test.warning, comment)
		}
	}
}

func (s *testPlanSuite) TestHintAlias(c *C) {
	defer testleak.AfterTest(c)()
	store, dom, err := newStoreWithBootstrap()
	c.Assert(err, IsNil)
	defer func() {
		dom.Close()
		store.Close()
	}()
	se, err := session.CreateSession4Test(store)
	c.Assert(err, IsNil)
	_, err = se.Execute(context.Background(), "use test")
	c.Assert(err, IsNil)

	tests := []struct {
		sql1 string
		sql2 string
	}{
		{
			sql1: "select /*+ TIDB_SMJ(t1) */ t1.a, t1.b from t t1, (select /*+ TIDB_INLJ(t3) */ t2.a from t t2, t t3 where t2.a = t3.c) s where t1.a=s.a",
			sql2: "select /*+ SM_JOIN(t1) */ t1.a, t1.b from t t1, (select /*+ INL_JOIN(t3) */ t2.a from t t2, t t3 where t2.a = t3.c) s where t1.a=s.a",
		},
		{
			sql1: "select /*+ TIDB_HJ(t1) */ t1.a, t1.b from t t1, (select /*+ TIDB_SMJ(t2) */ t2.a from t t2, t t3 where t2.a = t3.c) s where t1.a=s.a",
			sql2: "select /*+ HASH_JOIN(t1) */ t1.a, t1.b from t t1, (select /*+ SM_JOIN(t2) */ t2.a from t t2, t t3 where t2.a = t3.c) s where t1.a=s.a",
		},
		{
			sql1: "select /*+ TIDB_INLJ(t1) */ t1.a, t1.b from t t1, (select /*+ TIDB_HJ(t2) */ t2.a from t t2, t t3 where t2.a = t3.c) s where t1.a=s.a",
			sql2: "select /*+ INL_JOIN(t1) */ t1.a, t1.b from t t1, (select /*+ HASH_JOIN(t2) */ t2.a from t t2, t t3 where t2.a = t3.c) s where t1.a=s.a",
		},
	}
	ctx := context.TODO()
	for i, tt := range tests {
		comment := Commentf("case:%v sql1:%s sql2:%s", i, tt.sql1, tt.sql2)
		stmt1, err := s.ParseOneStmt(tt.sql1, "", "")
		c.Assert(err, IsNil, comment)
		stmt2, err := s.ParseOneStmt(tt.sql2, "", "")
		c.Assert(err, IsNil, comment)

		p1, err := planner.Optimize(ctx, se, stmt1, s.is)
		c.Assert(err, IsNil)
		p2, err := planner.Optimize(ctx, se, stmt2, s.is)
		c.Assert(err, IsNil)

		c.Assert(core.ToString(p1), Equals, core.ToString(p2))
	}
}

func (s *testPlanSuite) TestIndexHint(c *C) {
	defer testleak.AfterTest(c)()
	store, dom, err := newStoreWithBootstrap()
	c.Assert(err, IsNil)
	defer func() {
		dom.Close()
		store.Close()
	}()
	se, err := session.CreateSession4Test(store)
	c.Assert(err, IsNil)
	_, err = se.Execute(context.Background(), "use test")
	c.Assert(err, IsNil)

	var input []string
	var output []struct {
		SQL     string
		Best    string
		HasWarn bool
		Hints   string
	}
	s.testData.GetTestCases(c, &input, &output)
	ctx := context.Background()
	for i, test := range input {
		comment := Commentf("case:%v sql:%s", i, test)
		se.GetSessionVars().StmtCtx.SetWarnings(nil)

		stmt, err := s.ParseOneStmt(test, "", "")
		c.Assert(err, IsNil, comment)

		p, err := planner.Optimize(ctx, se, stmt, s.is)
		c.Assert(err, IsNil)
		s.testData.OnRecord(func() {
			output[i].SQL = test
			output[i].Best = core.ToString(p)
			output[i].HasWarn = len(se.GetSessionVars().StmtCtx.GetWarnings()) > 0
			output[i].Hints = core.GenHintsFromPhysicalPlan(p)
		})
		c.Assert(core.ToString(p), Equals, output[i].Best, comment)
		warnings := se.GetSessionVars().StmtCtx.GetWarnings()
		if output[i].HasWarn {
			c.Assert(warnings, HasLen, 1, comment)
		} else {
			c.Assert(warnings, HasLen, 0, comment)
		}
		c.Assert(core.GenHintsFromPhysicalPlan(p), Equals, output[i].Hints, comment)
	}
}

func (s *testPlanSuite) TestQueryBlockHint(c *C) {
	defer testleak.AfterTest(c)()
	store, dom, err := newStoreWithBootstrap()
	c.Assert(err, IsNil)
	defer func() {
		dom.Close()
		store.Close()
	}()
	se, err := session.CreateSession4Test(store)
	c.Assert(err, IsNil)
	_, err = se.Execute(context.Background(), "use test")
	c.Assert(err, IsNil)

	var input []string
	var output []struct {
		SQL   string
		Plan  string
		Hints string
	}
	s.testData.GetTestCases(c, &input, &output)
	ctx := context.TODO()
	for i, tt := range input {
		comment := Commentf("case:%v sql: %s", i, tt)
		stmt, err := s.ParseOneStmt(tt, "", "")
		c.Assert(err, IsNil, comment)

		p, err := planner.Optimize(ctx, se, stmt, s.is)
		c.Assert(err, IsNil, comment)
		s.testData.OnRecord(func() {
			output[i].SQL = tt
			output[i].Plan = core.ToString(p)
			output[i].Hints = core.GenHintsFromPhysicalPlan(p)
		})
		c.Assert(core.ToString(p), Equals, output[i].Plan, comment)
		c.Assert(core.GenHintsFromPhysicalPlan(p), Equals, output[i].Hints, comment)
	}
}

func (s *testPlanSuite) TestDAGPlanBuilderSplitAvg(c *C) {
	defer testleak.AfterTest(c)()
	store, dom, err := newStoreWithBootstrap()
	c.Assert(err, IsNil)
	defer func() {
		dom.Close()
		store.Close()
	}()
	se, err := session.CreateSession4Test(store)
	c.Assert(err, IsNil)
	_, err = se.Execute(context.Background(), "use test")
	c.Assert(err, IsNil)
	tests := []struct {
		sql  string
		plan string
	}{
		{
			sql:  "select avg(a),avg(b),avg(c) from t",
			plan: "TableReader(Table(t)->StreamAgg)->StreamAgg",
		},
		{
			sql:  "select /*+ HASH_AGG() */ avg(a),avg(b),avg(c) from t",
			plan: "TableReader(Table(t)->HashAgg)->HashAgg",
		},
	}

	for _, tt := range tests {
		comment := Commentf("for %s", tt.sql)
		stmt, err := s.ParseOneStmt(tt.sql, "", "")
		c.Assert(err, IsNil, comment)

		core.Preprocess(se, stmt, s.is)
		p, err := planner.Optimize(context.TODO(), se, stmt, s.is)
		c.Assert(err, IsNil, comment)

		c.Assert(core.ToString(p), Equals, tt.plan, comment)
		root, ok := p.(core.PhysicalPlan)
		if !ok {
			continue
		}
		testDAGPlanBuilderSplitAvg(c, root)
	}
}

func testDAGPlanBuilderSplitAvg(c *C, root core.PhysicalPlan) {
	if p, ok := root.(*core.PhysicalTableReader); ok {
		if p.TablePlans != nil {
			baseAgg := p.TablePlans[len(p.TablePlans)-1]
			if agg, ok := baseAgg.(*core.PhysicalHashAgg); ok {
				for i, aggfunc := range agg.AggFuncs {
					c.Assert(agg.Schema().Columns[i].RetType, Equals, aggfunc.RetTp)
				}
			}
			if agg, ok := baseAgg.(*core.PhysicalStreamAgg); ok {
				for i, aggfunc := range agg.AggFuncs {
					c.Assert(agg.Schema().Columns[i].RetType, Equals, aggfunc.RetTp)
				}
			}
		}
	}

	childs := root.Children()
	if childs == nil {
		return
	}
	for _, son := range childs {
		testDAGPlanBuilderSplitAvg(c, son)
	}
}

func (s *testPlanSuite) TestSimplifyOuterJoinWithCast(c *C) {
	defer testleak.AfterTest(c)()
	store, dom, err := newStoreWithBootstrap()
	c.Assert(err, IsNil)
	defer func() {
		dom.Close()
		store.Close()
	}()
	se, err := session.CreateSession4Test(store)
	c.Assert(err, IsNil)
	_, err = se.Execute(context.Background(), "use test")
	c.Assert(err, IsNil)

	var input []string
	var output []struct {
		SQL  string
		Best string
	}
	s.testData.GetTestCases(c, &input, &output)
	for i, test := range input {
		comment := Commentf("case:%v sql:%s", i, test)
		stmt, err := s.ParseOneStmt(test, "", "")
		c.Assert(err, IsNil, comment)

		p, err := planner.Optimize(context.Background(), se, stmt, s.is)
		c.Assert(err, IsNil)
		s.testData.OnRecord(func() {
			output[i].SQL = test
			output[i].Best = core.ToString(p)
		})
		c.Assert(core.ToString(p), Equals, output[i].Best)

		warnings := se.GetSessionVars().StmtCtx.GetWarnings()
		c.Assert(warnings, HasLen, 0, comment)
	}
}
