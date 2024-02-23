// Copyright 2022 PingCAP, Inc.
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

package sinkmanager

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pingcap/errors"
	"github.com/pingcap/log"
	"github.com/pingcap/tiflow/cdc/model"
	"github.com/pingcap/tiflow/cdc/processor/sourcemanager/sorter"
	"github.com/pingcap/tiflow/cdc/processor/tablepb"
	"github.com/pingcap/tiflow/cdc/sink/tablesink"
	cerrors "github.com/pingcap/tiflow/pkg/errors"
	"github.com/pingcap/tiflow/pkg/retry"
	"github.com/tikv/client-go/v2/oracle"
	pd "github.com/tikv/pd/client"
	"go.uber.org/zap"
)

var tableSinkWrapperIDCounter uint64 = 0

// tableSinkWrapper is a wrapper of TableSink, it is used in SinkManager to manage TableSink.
// Because in the SinkManager, we write data to TableSink and RedoManager concurrently,
// so current sink node can not be reused.
type tableSinkWrapper struct {
	id uint64

	// changefeed used for logging.
	changefeed model.ChangeFeedID
	// tableSpan used for logging.
	span tablepb.Span

	tableSinkCreator func() (tablesink.TableSink, uint64)

	// tableSink is the underlying sink.
	tableSink struct {
		sync.RWMutex
		s       tablesink.TableSink
		version uint64 // it's generated by `tableSinkCreater`.

		state struct {
			sync.RWMutex
			advanced     time.Time
			resolvedTs   model.ResolvedTs
			checkpointTs model.ResolvedTs
			lastSyncedTs model.Ts
		}
	}

	// state used to control the lifecycle of the table.
	state *tablepb.TableState

	// startTs is the start ts of the table.
	startTs model.Ts

	// barrierTs is the barrier bound of the table sink.
	barrierTs atomic.Uint64
	// receivedSorterResolvedTs is the resolved ts received from the sorter.
	// We use this to advance the redo log.
	receivedSorterResolvedTs atomic.Uint64

	// replicateTs is the ts that the table sink has started to replicate.
	replicateTs    model.Ts
	genReplicateTs func(ctx context.Context) (model.Ts, error)

	// lastCleanTime indicates the last time the table has been cleaned.
	lastCleanTime time.Time

	// rangeEventCounts is for clean the table sorter.
	// If rangeEventCounts[i].events is greater than 0, it means there must be
	// events in the range (rangeEventCounts[i-1].lastPos, rangeEventCounts[i].lastPos].
	rangeEventCounts struct {
		sync.Mutex
		c []rangeEventCount
	}
}

type rangeEventCount struct {
	// firstPos and lastPos are used to merge many rangeEventCount into one.
	firstPos sorter.Position
	lastPos  sorter.Position
	events   int
}

func newRangeEventCount(pos sorter.Position, events int) rangeEventCount {
	return rangeEventCount{
		firstPos: pos,
		lastPos:  pos,
		events:   events,
	}
}

func newTableSinkWrapper(
	changefeed model.ChangeFeedID,
	span tablepb.Span,
	tableSinkCreater func() (tablesink.TableSink, uint64),
	state tablepb.TableState,
	startTs model.Ts,
	genReplicateTs func(ctx context.Context) (model.Ts, error),
) *tableSinkWrapper {
	res := &tableSinkWrapper{
		id:               atomic.AddUint64(&tableSinkWrapperIDCounter, 1),
		changefeed:       changefeed,
		span:             span,
		tableSinkCreator: tableSinkCreater,
		state:            &state,
		startTs:          startTs,
		genReplicateTs:   genReplicateTs,
	}

	res.tableSink.version = 0
	res.tableSink.state.checkpointTs = model.NewResolvedTs(startTs)
	res.tableSink.state.resolvedTs = model.NewResolvedTs(startTs)
	res.tableSink.state.advanced = time.Now()

	res.receivedSorterResolvedTs.Store(startTs)
	res.barrierTs.Store(startTs)
	return res
}

func (t *tableSinkWrapper) start(ctx context.Context, startTs model.Ts) (err error) {
	if t.replicateTs != 0 {
		log.Panic("The table sink has already started",
			zap.String("namespace", t.changefeed.Namespace),
			zap.String("changefeed", t.changefeed.ID),
			zap.Stringer("span", &t.span),
			zap.Uint64("startTs", startTs),
			zap.Uint64("oldReplicateTs", t.replicateTs),
		)
	}

	// FIXME(qupeng): it can be re-fetched later instead of fails.
	if t.replicateTs, err = t.genReplicateTs(ctx); err != nil {
		return errors.Trace(err)
	}

	log.Info("Sink is started",
		zap.String("namespace", t.changefeed.Namespace),
		zap.String("changefeed", t.changefeed.ID),
		zap.Stringer("span", &t.span),
		zap.Uint64("startTs", startTs),
		zap.Uint64("replicateTs", t.replicateTs),
	)

	// This start ts maybe greater than the initial start ts of the table sink.
	// Because in two phase scheduling, the table sink may be advanced to a later ts.
	// And we can just continue to replicate the table sink from the new start ts.
	for {
		old := t.receivedSorterResolvedTs.Load()
		if startTs <= old || t.receivedSorterResolvedTs.CompareAndSwap(old, startTs) {
			break
		}
	}

	t.tableSink.state.Lock()
	defer t.tableSink.state.Unlock()
	if model.NewResolvedTs(startTs).Greater(t.tableSink.state.checkpointTs) {
		t.tableSink.state.checkpointTs = model.NewResolvedTs(startTs)
		t.tableSink.state.resolvedTs = model.NewResolvedTs(startTs)
		t.tableSink.state.advanced = time.Now()
	}
	t.state.Store(tablepb.TableStateReplicating)
	return nil
}

func (t *tableSinkWrapper) appendRowChangedEvents(events ...*model.RowChangedEvent) error {
	t.tableSink.RLock()
	defer t.tableSink.RUnlock()
	if t.tableSink.s == nil {
		// If it's nil it means it's closed.
		return tablesink.NewSinkInternalError(errors.New("table sink cleared"))
	}
	t.tableSink.s.AppendRowChangedEvents(events...)
	return nil
}

func (t *tableSinkWrapper) updateBarrierTs(ts model.Ts) {
	for {
		old := t.barrierTs.Load()
		if ts <= old || t.barrierTs.CompareAndSwap(old, ts) {
			break
		}
	}
}

func (t *tableSinkWrapper) updateReceivedSorterResolvedTs(ts model.Ts) {
	for {
		old := t.receivedSorterResolvedTs.Load()
		if ts <= old {
			return
		}
		if t.receivedSorterResolvedTs.CompareAndSwap(old, ts) {
			if t.state.Load() == tablepb.TableStatePreparing {
				t.state.Store(tablepb.TableStatePrepared)
			}
			return
		}
	}
}

func (t *tableSinkWrapper) updateResolvedTs(ts model.ResolvedTs) error {
	t.tableSink.RLock()
	defer t.tableSink.RUnlock()
	if t.tableSink.s == nil {
		// If it's nil it means it's closed.
		return tablesink.NewSinkInternalError(errors.New("table sink cleared"))
	}
	t.tableSink.state.Lock()
	defer t.tableSink.state.Unlock()
	t.tableSink.state.resolvedTs = ts
	return t.tableSink.s.UpdateResolvedTs(ts)
}

func (t *tableSinkWrapper) getLastSyncedTs() uint64 {
	t.tableSink.RLock()
	defer t.tableSink.RUnlock()
	if t.tableSink.s != nil {
		return t.tableSink.s.GetLastSyncedTs()
	}

	t.tableSink.state.RLock()
	defer t.tableSink.state.RUnlock()
	return t.tableSink.state.lastSyncedTs
}

func (t *tableSinkWrapper) getCheckpointTs() model.ResolvedTs {
	t.tableSink.RLock()
	defer t.tableSink.RUnlock()

	t.tableSink.state.Lock()
	defer t.tableSink.state.Unlock()

	if t.tableSink.s != nil {
		checkpointTs := t.tableSink.s.GetCheckpointTs()
		if t.tableSink.state.checkpointTs.Less(checkpointTs) {
			t.tableSink.state.checkpointTs = checkpointTs
			t.tableSink.state.advanced = time.Now()
		} else if !checkpointTs.Less(t.tableSink.state.resolvedTs) {
			t.tableSink.state.advanced = time.Now()
		}
	}

	return t.tableSink.state.checkpointTs
}

func (t *tableSinkWrapper) getReceivedSorterResolvedTs() model.Ts {
	return t.receivedSorterResolvedTs.Load()
}

func (t *tableSinkWrapper) getState() tablepb.TableState {
	return t.state.Load()
}

// getUpperBoundTs returns the upper bound of the table sink.
// It is used by sinkManager to generate sink task.
// upperBoundTs should be the minimum of the following two values:
// 1. the resolved ts of the sorter
// 2. the barrier ts of the table
func (t *tableSinkWrapper) getUpperBoundTs() model.Ts {
	resolvedTs := t.getReceivedSorterResolvedTs()
	barrierTs := t.barrierTs.Load()
	if resolvedTs > barrierTs {
		resolvedTs = barrierTs
	}
	return resolvedTs
}

func (t *tableSinkWrapper) markAsClosing() {
	for {
		currentState := t.state.Load()
		if currentState == tablepb.TableStateStopped {
			return
		}
		// Use CompareAndSwap to prevent state from being changed by other
		// goroutines at the same time.
		// Because we don't want to change the state if it's already stopped.
		if t.state.CompareAndSwap(currentState, tablepb.TableStateStopping) {
			log.Info("Sink is closing",
				zap.String("namespace", t.changefeed.Namespace),
				zap.String("changefeed", t.changefeed.ID),
				zap.Stringer("span", &t.span))
			return
		}
	}
}

// asyncStop will try to close the table sink asynchronously.
func (t *tableSinkWrapper) asyncStop() bool {
	t.markAsClosing()
	if t.asyncClose() {
		t.state.Store(tablepb.TableStateStopped)
		log.Info("Table sink is closed",
			zap.String("namespace", t.changefeed.Namespace),
			zap.String("changefeed", t.changefeed.ID),
			zap.Stringer("span", &t.span))
		return true
	}
	return false
}

// Return true means the underlying table sink has been initialized.
// So we can use it to write data.
func (t *tableSinkWrapper) isReady() bool {
	t.tableSink.Lock()
	defer t.tableSink.Unlock()
	t.tableSink.state.Lock()
	defer t.tableSink.state.Unlock()

	if t.tableSink.s == nil {
		t.tableSink.s, t.tableSink.version = t.tableSinkCreator()
		if t.tableSink.s != nil {
			t.tableSink.state.advanced = time.Now()
			return true
		}
		return false
	}

	return true
}

func (t *tableSinkWrapper) asyncClose() bool {
	t.tableSink.RLock()
	if t.tableSink.s == nil {
		return true
	}
	closed := t.tableSink.s.AsyncClose()
	t.tableSink.RUnlock()

	if closed {
		t.clear()
	}

	return closed
}

// closeAndClear will close the table sink synchronously and clear the table sink.
// It is only when we need to restart sink factory in sink manager.
// Because it uses a write lock, which may cause the closing to be blocked a long time.
func (t *tableSinkWrapper) closeAndClear() {
	t.Close()
	t.clear()
}

// Close will Close the table sink synchronously.
// It just use a read lock to avoid blocking the main loop.
func (t *tableSinkWrapper) Close() {
	t.tableSink.RLock()
	defer t.tableSink.RUnlock()
	if t.tableSink.s == nil {
		return
	}
	t.tableSink.s.Close()
}

// clear will update the tableSinkWrapper's state and
// set the table sink to nil.
func (t *tableSinkWrapper) clear() {
	t.tableSink.Lock()
	defer t.tableSink.Unlock()
	t.tableSink.state.Lock()
	defer t.tableSink.state.Unlock()

	if t.tableSink.s == nil {
		return
	}

	checkpointTs := t.tableSink.s.GetCheckpointTs()
	if t.tableSink.state.checkpointTs.Less(checkpointTs) {
		t.tableSink.state.checkpointTs = checkpointTs
	}
	t.tableSink.state.resolvedTs = checkpointTs
	t.tableSink.state.lastSyncedTs = t.tableSink.s.GetLastSyncedTs()
	t.tableSink.state.advanced = time.Now()

	// clear the table sink
	t.tableSink.s = nil
	t.tableSink.version = 0
}

func (t *tableSinkWrapper) checkTableSinkHealth() (err error) {
	t.tableSink.RLock()
	defer t.tableSink.RUnlock()
	if t.tableSink.s != nil {
		err = t.tableSink.s.CheckHealth()
	}
	return
}

// When the attached sink fail, there can be some events that have already been
// committed at downstream but we don't know. So we need to update `replicateTs`
// of the table so that we can re-send those events later.
func (t *tableSinkWrapper) restart(ctx context.Context) (err error) {
	if t.replicateTs, err = t.genReplicateTs(ctx); err != nil {
		return errors.Trace(err)
	}
	log.Info("Sink is restarted",
		zap.String("namespace", t.changefeed.Namespace),
		zap.String("changefeed", t.changefeed.ID),
		zap.Stringer("span", &t.span),
		zap.Uint64("replicateTs", t.replicateTs))
	return nil
}

func (t *tableSinkWrapper) updateRangeEventCounts(eventCount rangeEventCount) {
	t.rangeEventCounts.Lock()
	defer t.rangeEventCounts.Unlock()

	countsLen := len(t.rangeEventCounts.c)
	if countsLen == 0 {
		t.rangeEventCounts.c = append(t.rangeEventCounts.c, eventCount)
		return
	}
	if t.rangeEventCounts.c[countsLen-1].lastPos.Compare(eventCount.lastPos) < 0 {
		// If two rangeEventCounts are close enough, we can merge them into one record
		// to save memory usage. When merging B into A, A.lastPos will be updated but
		// A.firstPos will be kept so that we can determine whether to continue to merge
		// more events or not based on timeDiff(C.lastPos, A.firstPos).
		lastPhy := oracle.ExtractPhysical(t.rangeEventCounts.c[countsLen-1].firstPos.CommitTs)
		currPhy := oracle.ExtractPhysical(eventCount.lastPos.CommitTs)
		if (currPhy - lastPhy) >= 1000 { // 1000 means 1000ms.
			t.rangeEventCounts.c = append(t.rangeEventCounts.c, eventCount)
		} else {
			t.rangeEventCounts.c[countsLen-1].lastPos = eventCount.lastPos
			t.rangeEventCounts.c[countsLen-1].events += eventCount.events
		}
	}
}

func (t *tableSinkWrapper) cleanRangeEventCounts(upperBound sorter.Position, minEvents int) bool {
	t.rangeEventCounts.Lock()
	defer t.rangeEventCounts.Unlock()

	idx := sort.Search(len(t.rangeEventCounts.c), func(i int) bool {
		return t.rangeEventCounts.c[i].lastPos.Compare(upperBound) > 0
	})
	if len(t.rangeEventCounts.c) == 0 || idx == 0 {
		return false
	}

	count := 0
	for _, events := range t.rangeEventCounts.c[0:idx] {
		count += events.events
	}
	shouldClean := count >= minEvents

	if !shouldClean {
		// To reduce sorter.CleanByTable calls.
		t.rangeEventCounts.c[idx-1].events = count
		t.rangeEventCounts.c = t.rangeEventCounts.c[idx-1:]
	} else {
		t.rangeEventCounts.c = t.rangeEventCounts.c[idx:]
	}
	return shouldClean
}

func (t *tableSinkWrapper) sinkMaybeStuck(stuckCheck time.Duration) (bool, uint64) {
	t.getCheckpointTs()

	t.tableSink.RLock()
	defer t.tableSink.RUnlock()
	t.tableSink.state.RLock()
	defer t.tableSink.state.RUnlock()
	// What these conditions mean:
	// 1. the table sink has been associated with a valid sink;
	// 2. its checkpoint hasn't been advanced for a while;
	version := t.tableSink.version
	advanced := t.tableSink.state.advanced
	if version > 0 && time.Since(advanced) > stuckCheck {
		return true, version
	}
	return false, uint64(0)
}

func handleRowChangedEvents(
	changefeed model.ChangeFeedID, span tablepb.Span,
	events ...*model.PolymorphicEvent,
) ([]*model.RowChangedEvent, uint64) {
	size := 0
	rowChangedEvents := make([]*model.RowChangedEvent, 0, len(events))
	for _, e := range events {
		if e == nil || e.Row == nil {
			log.Warn("skip emit nil event",
				zap.String("namespace", changefeed.Namespace),
				zap.String("changefeed", changefeed.ID),
				zap.Stringer("span", &span),
				zap.Any("event", e))
			continue
		}

		rowEvent := e.Row
		// Some transactions could generate empty row change event, such as
		// begin; insert into t (id) values (1); delete from t where id=1; commit;
		// Just ignore these row changed events.
		if len(rowEvent.Columns) == 0 && len(rowEvent.PreColumns) == 0 {
			log.Warn("skip emit empty row event",
				zap.Stringer("span", &span),
				zap.String("namespace", changefeed.Namespace),
				zap.String("changefeed", changefeed.ID),
				zap.Any("event", e))
			continue
		}

		size += rowEvent.ApproximateBytes()
		rowChangedEvents = append(rowChangedEvents, rowEvent)
	}
	return rowChangedEvents, uint64(size)
}

func genReplicateTs(ctx context.Context, pdClient pd.Client) (model.Ts, error) {
	backoffBaseDelayInMs := int64(100)
	totalRetryDuration := 10 * time.Second
	var replicateTs model.Ts
	err := retry.Do(ctx, func() error {
		phy, logic, err := pdClient.GetTS(ctx)
		if err != nil {
			return errors.Trace(err)
		}
		replicateTs = oracle.ComposeTS(phy, logic)
		return nil
	}, retry.WithBackoffBaseDelay(backoffBaseDelayInMs),
		retry.WithTotalRetryDuratoin(totalRetryDuration),
		retry.WithIsRetryableErr(cerrors.IsRetryableError))
	if err != nil {
		return model.Ts(0), errors.Trace(err)
	}
	return replicateTs, nil
}
