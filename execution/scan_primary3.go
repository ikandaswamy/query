//  Copyright (c) 2017 Couchbase, Inc.
//  Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file
//  except in compliance with the License. You may obtain a copy of the License at
//    http://www.apache.org/licenses/LICENSE-2.0
//  Unless required by applicable law or agreed to in writing, software distributed under the
//  License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
//  either express or implied. See the License for the specific language governing permissions
//  and limitations under the License.

package execution

import (
	"encoding/json"
	"math"

	"github.com/couchbase/query/datastore"
	"github.com/couchbase/query/errors"
	"github.com/couchbase/query/logging"
	"github.com/couchbase/query/plan"
	"github.com/couchbase/query/value"
)

type PrimaryScan3 struct {
	base
	plan *plan.PrimaryScan3
}

func NewPrimaryScan3(plan *plan.PrimaryScan3, context *Context) *PrimaryScan3 {
	rv := &PrimaryScan3{
		plan: plan,
	}

	newBase(&rv.base, context)
	rv.newStopChannel()
	rv.output = rv
	return rv
}

func (this *PrimaryScan3) Accept(visitor Visitor) (interface{}, error) {
	return visitor.VisitPrimaryScan3(this)
}

func (this *PrimaryScan3) Copy() Operator {
	rv := &PrimaryScan3{plan: this.plan}
	this.base.copy(&rv.base)
	return rv
}

func (this *PrimaryScan3) RunOnce(context *Context, parent value.Value) {
	this.once.Do(func() {
		defer context.Recover() // Recover from any panic
		this.active()
		defer this.close(context)
		this.setExecPhase(PRIMARY_SCAN, context)
		defer this.notify() // Notify that I have stopped

		this.scanPrimary(context, parent)
	})
}

func (this *PrimaryScan3) scanPrimary(context *Context, parent value.Value) {
	this.switchPhase(_EXECTIME)
	defer this.switchPhase(_NOTIME)
	conn := datastore.NewIndexConnection(context)
	conn.SetPrimary()
	defer notifyConn(conn.StopChannel()) // Notify index that I have stopped

	offset := evalLimitOffset(this.plan.Offset(), nil, int64(0), false, context)
	limit := evalLimitOffset(this.plan.Limit(), nil, math.MaxInt64, false, context)

	go this.scanEntries(context, conn, offset, limit)

	nitems := uint64(0)

	var docs uint64 = 0
	defer func() {
		if docs > 0 {
			context.AddPhaseCount(PRIMARY_SCAN, docs)
		}
	}()

	var lastEntry *datastore.IndexEntry
	for {
		entry, ok := this.getItemEntry(conn.EntryChannel())
		if ok {
			if entry != nil {
				// current policy is to only count 'in' documents
				// from operators, not kv
				// add this.addInDocs(1) if this changes
				av := this.newEmptyDocumentWithKey(entry.PrimaryKey, parent, context)
				ok = this.sendItem(av)
				lastEntry = entry
				nitems++
				docs++
				if docs > _PHASE_UPDATE_COUNT {
					context.AddPhaseCount(PRIMARY_SCAN, docs)
					docs = 0
				}
			} else {
				break
			}
		} else {
			return
		}
	}

	emsg := "Primary index scan timeout - resorting to chunked scan"
	for conn.Timeout() {
		// Offset, Aggregates, Order needs to be exact.
		// On timeout return error because we cann't stitch the output
		if this.plan.Offset() != nil || len(this.plan.OrderTerms()) > 0 || this.plan.GroupAggs() != nil ||
			lastEntry == nil {
			context.Error(errors.NewCbIndexScanTimeoutError(nil))
			return
		}

		logging.Errorp(emsg, logging.Pair{"chunkSize", nitems},
			logging.Pair{"startingEntry", stringifyIndexEntry(lastEntry)})

		// do chunked scans; lastEntry the starting point
		conn = datastore.NewIndexConnection(context)
		conn.SetPrimary()
		lastEntry, nitems = this.scanPrimaryChunk(context, parent, conn, lastEntry, limit)
		emsg = "Primary index chunked scan"
	}
}

func (this *PrimaryScan3) scanPrimaryChunk(context *Context, parent value.Value, conn *datastore.IndexConnection,
	indexEntry *datastore.IndexEntry, limit int64) (*datastore.IndexEntry, uint64) {
	this.switchPhase(_EXECTIME)
	defer this.switchPhase(_NOTIME)
	defer notifyConn(conn.StopChannel()) // Notify index that I have stopped

	go this.scanChunk(context, conn, limit, indexEntry)

	nitems := uint64(0)
	var docs uint64 = 0
	defer func() {
		if nitems > 0 {
			context.AddPhaseCount(PRIMARY_SCAN, docs)
		}
	}()

	var lastEntry *datastore.IndexEntry
	for {
		entry, ok := this.getItemEntry(conn.EntryChannel())
		if ok {
			if entry != nil {
				av := this.newEmptyDocumentWithKey(entry.PrimaryKey, parent, context)
				ok = this.sendItem(av)
				lastEntry = entry
				nitems++
				docs++
				if docs > _PHASE_UPDATE_COUNT {
					context.AddPhaseCount(PRIMARY_SCAN, docs)
					docs = 0
				}
			} else {
				break
			}
		} else {
			return nil, nitems
		}
	}
	return lastEntry, nitems
}

func (this *PrimaryScan3) scanEntries(context *Context, conn *datastore.IndexConnection, offset, limit int64) {
	defer context.Recover() // Recover from any panic

	index := this.plan.Index()
	keyspace := this.plan.Keyspace()
	scanVector := context.ScanVectorSource().ScanVector(keyspace.NamespaceId(), keyspace.Name())
	indexProjection, indexOrder, indexGroupAggs := planToScanMapping(index, this.plan.Projection(),
		this.plan.OrderTerms(), this.plan.GroupAggs(), nil)

	index.ScanEntries3(context.RequestId(), indexProjection, offset, limit, indexGroupAggs, indexOrder,
		context.ScanConsistency(), scanVector, conn)
}

func (this *PrimaryScan3) scanChunk(context *Context, conn *datastore.IndexConnection, limit int64, indexEntry *datastore.IndexEntry) {
	defer context.Recover() // Recover from any panic
	ds := &datastore.Span{}
	// do the scan starting from, but not including, the given index entry:
	ds.Range = datastore.Range{
		Inclusion: datastore.NEITHER,
		Low:       []value.Value{value.NewValue(indexEntry.PrimaryKey)},
	}
	keyspace := this.plan.Keyspace()
	scanVector := context.ScanVectorSource().ScanVector(keyspace.NamespaceId(), keyspace.Name())
	this.plan.Index().Scan(context.RequestId(), ds, true, limit,
		context.ScanConsistency(), scanVector, conn)
}

func (this *PrimaryScan3) MarshalJSON() ([]byte, error) {
	r := this.plan.MarshalBase(func(r map[string]interface{}) {
		this.marshalTimes(r)
	})
	return json.Marshal(r)
}

// send a stop
func (this *PrimaryScan3) SendStop() {
	this.chanSendStop()
}
