package collector

import (
	"errors"
	"fmt"
	"path"
	"sort"
	"sync/atomic"
	"time"

	"mongoshake/collector/configure"
	"mongoshake/collector/filter"
	"mongoshake/common"
	"mongoshake/oplog"
	"mongoshake/quorum"

	"github.com/gugemichael/nimo4go"
	LOG "github.com/vinllen/log4go"
	"github.com/vinllen/mgo/bson"
)

const (
	// bson deserialize workload is CPU-intensive task
	PipelineQueueMaxNr = 4
	PipelineQueueMinNr = 1
	PipelineQueueLen   = 64

	DurationTime        = 6000 // unit: ms.
	DDLCheckpointGap    = 5    // unit: seconds.
	FilterCheckpointGap = 60   // unit: seconds. no checkpoint update, flush checkpoint mandatory

	DiskQueueName = "dqName"
)

type OplogHandler interface {
	// invocation on every oplog consumed
	Handle(log *oplog.PartialLog)
}

// OplogSyncer poll oplogs from original source MongoDB.
type OplogSyncer struct {
	OplogHandler

	// global replicate coordinator
	coordinator *ReplicationCoordinator
	// source mongodb replica set name
	replset string
	// full sync finish position, used to check DDL between full sync and incr sync
	docEndTs bson.MongoTimestamp

	ckptManager *CheckpointManager
	mvckManager *MoveChunkManager
	ddlManager  *DDLManager

	// oplog hash strategy
	hasher oplog.Hasher

	// pending queue. used by rawlog parsing. we buffered the
	// target raw oplogs in buffer and push them to pending queue
	// when buffer is filled in. and transfer to log queue
	buffer            []*bson.Raw
	pendingQueue      []chan []*bson.Raw
	logsQueue         []chan []*oplog.GenericOplog
	nextQueuePosition uint64

	// source mongo oplog reader
	reader *OplogReader
	// journal log that records all oplogs
	journal *utils.Journal
	// oplogs dispatcher
	batcher *Batcher

	replMetric *utils.ReplicationMetric
}

/*
 * Syncer is used to fetch oplog from source MongoDB and then send to different workers which can be seen as
 * a network sender. There are several syncer coexist to improve the fetching performance.
 * The data flow in syncer is:
 * source mongodb --> reader --> pending queue(raw data) --> logs queue(parsed data) --> worker
 * The reason we split pending queue and logs queue is to improve the performance.
 */
func NewOplogSyncer(
	coordinator *ReplicationCoordinator,
	replset string,
	docEndTsMap map[string]bson.MongoTimestamp,
	mongoUrl string,
	gids []string,
	ckptManager *CheckpointManager,
	mvckManager *MoveChunkManager,
	ddlManager *DDLManager) *OplogSyncer {

	var docEndTs bson.MongoTimestamp
	if docEndTsMap != nil {
		if ts, ok := docEndTsMap[replset]; !ok {
			LOG.Crashf("new oplog syncer %v has no docEndTs. docEndTsMap %v", replset, docEndTsMap)
		} else {
			docEndTs = ts
		}
	}

	syncer := &OplogSyncer{
		coordinator: coordinator,
		replset:     replset,
		docEndTs:    docEndTs,
		journal: utils.NewJournal(utils.JournalFileName(
			fmt.Sprintf("%s.%s", conf.Options.CollectorId, replset))),
		ckptManager: ckptManager,
		mvckManager: mvckManager,
		ddlManager:  ddlManager,
	}
	syncer.reader = NewOplogReader(mongoUrl, syncer)

	// concurrent level hasher
	switch conf.Options.ShardKey {
	case oplog.ShardByNamespace:
		syncer.hasher = &oplog.TableHasher{}
	case oplog.ShardByID:
		syncer.hasher = &oplog.PrimaryKeyHasher{}
	}

	filterList := filter.OplogFilterChain{filter.NewAutologousFilter(), filter.NewGidFilter(gids)}

	// DDL filter
	if conf.Options.ReplayerDMLOnly {
		filterList = append(filterList, new(filter.DDLFilter))
	}
	// namespace filter, heavy operation
	if len(conf.Options.FilterNamespaceWhite) != 0 || len(conf.Options.FilterNamespaceBlack) != 0 {
		namespaceFilter := filter.NewNamespaceFilter(conf.Options.FilterNamespaceWhite,
			conf.Options.FilterNamespaceBlack)
		filterList = append(filterList, namespaceFilter)
	}

	// oplog filters. drop the oplog if any of the filter
	// list returns true. The order of all filters is not significant.
	// workerGroup is assigned later by syncer.bind()
	syncer.batcher = NewBatcher(syncer, filterList, syncer, []*Worker{})
	return syncer
}

func (sync *OplogSyncer) init() {
	sync.replMetric = utils.NewMetric(sync.replset, utils.METRIC_CKPT_TIMES|
		utils.METRIC_TUNNEL_TRAFFIC|utils.METRIC_LSN_CKPT|utils.METRIC_SUCCESS|
		utils.METRIC_TPS|utils.METRIC_RETRANSIMISSION)
	sync.replMetric.ReplStatus.Update(utils.WorkGood)

	sync.RestAPI()
}

// bind different worker
func (sync *OplogSyncer) bind(w *Worker) {
	sync.batcher.workerGroup = append(sync.batcher.workerGroup, w)
}

func (sync *OplogSyncer) startDiskApply(docEndTs bson.MongoTimestamp) {
	sync.docEndTs = docEndTs
	sync.reader.UpdateFetchStatus(FetchStatusStoreDiskApply)
}

// start to polling oplog
func (sync *OplogSyncer) start() {
	LOG.Info("Poll oplog syncer start. ckpt_interval[%dms], gid[%s], shard_key[%s]",
		conf.Options.CheckpointInterval, conf.Options.OplogGIDS, conf.Options.ShardKey)

	// process about the checkpoint :
	//
	// 1. create checkpoint manager
	// 2. load existing ckpt from remote storage
	// 3. start checkpoint persist routine

	// start deserializer: parse data from pending queue, and then push into logs queue.
	sync.startDeserializer()
	// start batcher: pull oplog from logs queue and then batch together before adding into worker.
	sync.startBatcher()

	// forever fetching oplog from mongodb into oplog_reader
	for {
		sync.poll()
		// error or exception occur
		LOG.Warn("oplog syncer polling yield. master:%t, yield:%dms", quorum.IsMaster(), DurationTime)
		utils.DelayFor(DurationTime)
	}
}

// fetch all oplog from logs queue, batched together and then send to different workers.
func (sync *OplogSyncer) startBatcher() {
	var batcher = sync.batcher
	barrier := false
	nimo.GoRoutineInLoop(func() {
		// As much as we can batch more from logs queue. batcher can merge
		// a sort of oplogs from different logs queue one by one. the max number
		// of oplogs in batch is limited by AdaptiveBatchingMaxSize
		nextBatch := batcher.Next()

		// avoid to do checkpoint when syncer update ackTs or syncTs
		sync.ckptManager.mutex.RLock()
		filteredNextBatch, nextBarrier, flushCheckpoint, lastOplog := batcher.filterAndBlockMoveChunk(nextBatch, barrier)
		barrier = nextBarrier

		if lastOplog != nil {
			needDispatch := true
			needUnBlock := false
			if ddlFilter.Filter(lastOplog) {
				logRaw := filteredNextBatch[0].Raw
				if DDLSupportForSharding() {
					// DDL operate for mongodb sharding
					needDispatch = sync.ddlManager.BlockDDL(sync.replset, lastOplog)
					if needDispatch {
						// ddl need to run, when not all but majority oplog syncer received ddl oplog
						LOG.Info("oplog syncer %v prepare to dispatch ddl log %v", sync.replset, lastOplog)
						// transform ddl to run at mongos of dest sharding
						// number of worker of sharding instance and number of ddl command must be 1
						shardColSpec := utils.GetShardCollectionSpec(sync.ddlManager.FromCsConn.Session, lastOplog)
						var transOplogs []*oplog.PartialLog
						if shardColSpec != nil {
							transOplogs = TransformShardingDDL(sync.replset, lastOplog, shardColSpec, sync.ddlManager.ToIsSharding)
						} else {
							transOplogs = TransformDbDDL(sync.replset, lastOplog)
						}
						filteredNextBatch = []*oplog.GenericOplog{}
						for _, tlog := range transOplogs {
							filteredNextBatch = append(filteredNextBatch, &oplog.GenericOplog{Raw: logRaw, Parsed: tlog})
						}
						needUnBlock = true
					}
				} else if !conf.Options.ReplayerDMLOnly {
					// DDL operate for normal mongodb
					transOplogs := TransformDbDDL(sync.replset, lastOplog)
					filteredNextBatch = []*oplog.GenericOplog{}
					for _, tlog := range transOplogs {
						filteredNextBatch = append(filteredNextBatch, &oplog.GenericOplog{Raw: logRaw, Parsed: tlog})
					}
				}
			}
			if needDispatch {
				// push to worker to run
				if worked := batcher.dispatchBatch(filteredNextBatch); worked {
					sync.replMetric.SetLSN(int64(lastOplog.Timestamp))
					// update latest fetched timestamp in memory
					sync.reader.UpdateQueryTimestamp(lastOplog.Timestamp)
				}
				if barrier {
					// wait for ddl operation finish, and flush checkpoint value
					sync.batcher.WaitAllAck()
					if flushCheckpoint {
						sync.ckptManager.mutex.RUnlock()
						sync.ckptManager.FlushChan <- true
						sync.ckptManager.mutex.RLock()
					}
					if needUnBlock {
						LOG.Info("oplog syncer %v Unblock at ddl log %v", sync.replset, lastOplog)
						// unblock other shard nodes when sharding ddl has finished
						sync.ddlManager.UnBlockDDL(sync.replset, lastOplog)
					}
				}
			}
		} else {
			// sync.batcher.unsyncTs >= sync.batcher.syncTs, because oplog have been filtered
			syncTs := sync.batcher.unsyncTs
			readerQueryTs := int64(sync.reader.GetQueryTimestamp())
			if utils.ExtractTs32(syncTs)-readerQueryTs >= FilterCheckpointGap {
				sync.batcher.WaitAllAck()
				LOG.Info("oplog syncer %v batcher update ackTs from %v to %v",
					sync.replset, utils.TimestampToLog(readerQueryTs), utils.TimestampToLog(syncTs))
				// update latest fetched timestamp in memory
				sync.reader.UpdateQueryTimestamp(syncTs)
				sync.batcher.UpdateAckTs(syncTs)
			}
		}

		// update syncTs of batcher
		sync.batcher.syncTs = sync.batcher.unsyncTs
		sync.ckptManager.mutex.RUnlock()
	})
}

func (sync *OplogSyncer) WaitAckTsUntil(logData []byte) {
	log := new(oplog.PartialLog)
	if err := bson.Unmarshal(logData, log); err != nil {
		LOG.Crashf("unmarshal oplog[%v] failed[%v]", logData, err)
	}
	LOG.Info("oplog syncer %v wait to ackTs[%v]", sync.replset, utils.TimestampToLog(log.Timestamp))
	for {
		syncTs := sync.batcher.syncTs
		if syncTs >= log.Timestamp {
			sync.batcher.WaitAllAck()
			sync.batcher.UpdateAckTs(syncTs)
			break
		}
		time.Sleep(AckUpdateInterval * time.Millisecond)
	}
	LOG.Info("oplog syncer %v finish wait to ackTs[%v]", sync.replset, utils.TimestampToLog(log.Timestamp))
}

// how many pending queue we create
func calculatePendingQueueConcurrency() int {
	// single {pending|logs}queue while it'is multi source shard
	if conf.Options.IsShardCluster() {
		return PipelineQueueMinNr
	}
	return PipelineQueueMaxNr
}

// deserializer: fetch oplog from pending queue, parsed and then add into logs queue.
func (sync *OplogSyncer) startDeserializer() {
	parallel := calculatePendingQueueConcurrency()
	sync.pendingQueue = make([]chan []*bson.Raw, parallel, parallel)
	sync.logsQueue = make([]chan []*oplog.GenericOplog, parallel, parallel)
	for index := 0; index != len(sync.pendingQueue); index++ {
		sync.pendingQueue[index] = make(chan []*bson.Raw, PipelineQueueLen)
		sync.logsQueue[index] = make(chan []*oplog.GenericOplog, PipelineQueueLen)
		go sync.deserializer(index)
	}
}

func (sync *OplogSyncer) deserializer(index int) {
	for {
		batchRawLogs := <-sync.pendingQueue[index]
		nimo.AssertTrue(len(batchRawLogs) != 0, "pending queue batch logs has zero length")
		var deserializeLogs = make([]*oplog.GenericOplog, 0, len(batchRawLogs))

		for _, rawLog := range batchRawLogs {
			log := new(oplog.PartialLog)
			bson.Unmarshal(rawLog.Data, log)
			log.RawSize = len(rawLog.Data)
			deserializeLogs = append(deserializeLogs, &oplog.GenericOplog{Raw: rawLog.Data, Parsed: log})
		}
		sync.logsQueue[index] <- deserializeLogs
	}
}

// only master(maybe several mongo-shake starts) can poll oplog.
func (sync *OplogSyncer) poll() {
	sync.reader.StartFetcher() // start reader fetcher if not exist
	// every syncer should under the control of global rate limiter
	rc := sync.coordinator.rateController

	for quorum.IsMaster() {
		// SimpleRateController is too simple. the TPS flow may represent
		// low -> high -> low.... and centralize to point time in somewhere
		// However. not smooth is make sense in stream processing. This was
		// more effected in request processing programing
		//
		//				    _             _
		//		    	   / |           / |             <- peak
		//			     /   |         /   |
		//   _____/    |____/    |___    <-  controlled
		//
		//
		// WARNING : in current version. we throttle the replicate tps in Receiver
		// rather than limiting in Collector. since the real replication traffic happened
		// in Receiver executor. Apparently it tends to change {SentinelOptions} in
		// Receiver. The follows were kept for compatibility
		if utils.SentinelOptions.TPS != 0 && rc.Control(utils.SentinelOptions.TPS, 1) {
			utils.DelayFor(100)
			continue
		}
		// only get one
		sync.next()
	}
}

// fetch oplog from reader.
func (sync *OplogSyncer) next() bool {
	var log *bson.Raw
	var err error
	if log, err = sync.reader.Next(); log != nil {
		payload := int64(len(log.Data))
		sync.replMetric.AddGet(1)
		sync.replMetric.SetOplogMax(payload)
		sync.replMetric.SetOplogAvg(payload)
		sync.replMetric.ReplStatus.Clear(utils.FetchBad)
		//test := bson.M{}
		//if err1 := bson.UnmarshalJSON(log.Data, test); err1 != nil {
		//	fmt.Printf("Unmarshal fail: %v", err1)
		//}

		//if errin := log.Unmarshal(test); errin != nil {
		//	fmt.Printf("Unmarshal fail: %v", errin)
		//}
		// map[h:0 ns:youshu.test o:map[$set:map[name:testttt] $v:1] o2:map[_id:4] op:u t:1
		// ts:6805825681415471105 ui:{4 [173 96 220 206 71 236 67 31 162 111 233 122 124 218 28 148]}
		// v:2 wall:2020-03-19 07:59:34.074 +0000 UTC]
		//if namespace := test["ns"]; strings.HasPrefix(namespace.(string), "youshu") {
		//	fmt.Println(test)
		//fmt.Println(log.Data)
		//	fmt.Println(test["ns"])
		//	fmt.Println(reflect.TypeOf(test["o2"]))
		//	fmt.Println(test["o2"])
		//}
		//return false
	} else if err != nil && err != TimeoutError {
		LOG.Error("oplog syncer internal error: %v", err)
		// error is nil indicate that only timeout incur syncer.next()
		// return false. so we regardless that
		sync.replMetric.ReplStatus.Update(utils.FetchBad)
		utils.DelayFor(DurationTime)
		// alarm
	}
	// buffered oplog or trigger to flush. log is nil
	// means that we need to flush buffer right now
	return sync.transfer(log)
}

func (sync *OplogSyncer) transfer(log *bson.Raw) bool {
	flush := false
	if log != nil {
		sync.buffer = append(sync.buffer, log)
	} else {
		flush = true
	}

	if len(sync.buffer) >= conf.Options.FetcherBufferCapacity || (flush && len(sync.buffer) != 0) {
		// we could simply ++syncer.resolverIndex. The max uint64 is 9223372036854774807
		// and discard the skip situation. we assume nextQueueCursor couldn't be overflow
		selected := int(sync.nextQueuePosition % uint64(len(sync.pendingQueue)))
		sync.pendingQueue[selected] <- sync.buffer
		sync.buffer = make([]*bson.Raw, 0, conf.Options.FetcherBufferCapacity)
		sync.nextQueuePosition++
		return true
	}
	return false
}

func (sync *OplogSyncer) LoadByDoc(ckptDoc map[string]interface{}, ts time.Time) error {
	ackTs, ok1 := ckptDoc[utils.CheckpointAckTs].(bson.MongoTimestamp)
	syncTs, ok2 := ckptDoc[utils.CheckpointSyncTs].(bson.MongoTimestamp)
	if !ok1 || !ok2 {
		return LOG.Critical("oplog syncer %v load checkpoint illegal record %v. ok1[%v] ok2[%v]",
			sync.replset, ckptDoc, ok1, ok2)
	}

	if ackTs != 0 {
		// the oldest oplog is lost
		if oldTs, err := utils.GetOldestTimestampByUrl(sync.reader.src); err != nil {
			LOG.Crashf("oplog syncer %v load checkpoint connect to %v failed. %v", sync.replset, err)
		} else if oldTs > ackTs {
			LOG.Crashf("oplog syncer %v load checkpoint queryTs[%v] is less than oldTs[%v], "+
				"this error means user's oplog collection size is too small or document replication continues too long",
				sync.replset, utils.TimestampToLog(ackTs), utils.TimestampToLog(oldTs))
		}
	} else {
		// oplog replication with context.start_position = 1970-01-01T00:00:00Z
		syncTs = bson.MongoTimestamp(1 << 32)
		ackTs = bson.MongoTimestamp(1 << 32)
	}

	sync.batcher.syncTs = syncTs
	sync.batcher.unsyncTs = syncTs
	for _, worker := range sync.batcher.workerGroup {
		worker.unack = int64(ackTs)
		worker.ack = int64(ackTs)
	}
	dqName, ok3 := ckptDoc[DiskQueueName].(string)
	dqPath := fmt.Sprintf(path.Join(conf.Options.LogDirectory, "%s.meta.dat"), dqName)
	if sync.docEndTs == 0 {
		// parallel run document and oplog replication
		sync.reader.UpdateFetchStatus(FetchStatusStoreDiskNoApply)
		sync.reader.InitDiskQueue(fmt.Sprintf("diskqueue-%v-%v", sync.replset, ts.Format("20060102-150405")))
		sync.reader.UpdateQueryTimestamp(ackTs)
	} else if ok3 && utils.FileExist(dqPath) {
		// oplog replication with disk queue remained
		sync.reader.UpdateFetchStatus(FetchStatusStoreDiskApply)
		sync.reader.InitDiskQueue(dqName)
		queryTs := sync.reader.GetQueryTsFromDiskQueue()
		if queryTs == 0 {
			// disk queue has finished and deleted
			LOG.Warn("oplog syncer %v load checkpoint disk queue[%v] has deleted", sync.replset, dqName)
			sync.reader.UpdateQueryTimestamp(ackTs)
		} else {
			sync.reader.UpdateQueryTimestamp(queryTs)
		}
	} else {
		// serially run document and oplog replication
		sync.reader.UpdateFetchStatus(FetchStatusStoreMemoryApply)
		sync.reader.UpdateQueryTimestamp(ackTs)
	}

	LOG.Info("oplog syncer %v load checkpoint set checkpoint to ackTs[%v] syncTs[%v] fetchStatus[%v] dqName[%v]",
		sync.replset, utils.TimestampToLog(ackTs), utils.TimestampToLog(syncTs),
		logFetchStatus(sync.reader.fetchStatus), dqName)
	return nil
}

func (sync *OplogSyncer) FlushByDoc() map[string]interface{} {
	ackTs, err := sync.calculateSyncerAckTs()
	if err != nil {
		LOG.Crashf("OplogSyncer flush checkpoint get ackTs of syncer %v failed. %v", sync.replset, err)
	}

	syncTs := sync.batcher.syncTs
	unsyncTs := sync.batcher.unsyncTs
	nimo.AssertTrue(syncTs == unsyncTs, "OplogSyncer flush checkpoint panic when syncTs != unsyncTs")
	for _, worker := range sync.batcher.workerGroup {
		ack := bson.MongoTimestamp(atomic.LoadInt64(&worker.ack))
		unack := bson.MongoTimestamp(atomic.LoadInt64(&worker.unack))
		LOG.Info("OplogSyncer flush checkpoint syncer %v ack[%v] unack[%v] syncTs[%v]", sync.replset,
			utils.TimestampToLog(ack), utils.TimestampToLog(unack), utils.TimestampToLog(syncTs))
	}
	sync.replMetric.AddCheckpoint(1)
	sync.replMetric.SetLSNCheckpoint(int64(ackTs))

	ckptDoc := map[string]interface{}{
		utils.CheckpointName:   sync.replset,
		utils.CheckpointAckTs:  ackTs,
		utils.CheckpointSyncTs: syncTs,
	}
	fetchStatus := atomic.LoadInt32(&sync.reader.fetchStatus)
	if fetchStatus == FetchStatusStoreDiskNoApply || fetchStatus == FetchStatusStoreDiskApply {
		ckptDoc[DiskQueueName] = sync.reader.GetDiskQueueName()
	}
	return ckptDoc
}

func (sync *OplogSyncer) calculateSyncerAckTs() (v bson.MongoTimestamp, err error) {
	// no need to lock and eventually consistence is acceptable
	allAcked := true
	candidates := make([]int64, 0, len(sync.batcher.workerGroup))
	allAckValues := make([]int64, 0, len(sync.batcher.workerGroup))
	for _, worker := range sync.batcher.workerGroup {
		// read ack value first because of we don't wanna
		// a result of ack > unack. There wouldn't be cpu
		// reorder under atomic !
		ack := atomic.LoadInt64(&worker.ack)
		unack := atomic.LoadInt64(&worker.unack)
		if ack == 0 && unack == 0 {
			// have no oplogs synced in this worker. skip
		} else if ack == unack || worker.IsAllAcked() {
			// all oplogs have been acked for right now or previous status
			worker.AllAcked(true)
			allAckValues = append(allAckValues, ack)
		} else if unack > ack {
			// most likely. partial oplogs acked (0 is possible)
			candidates = append(candidates, ack)
			allAcked = false
		} else if unack < ack && unack == 0 {
			// collector restarts. receiver unack value if from buffer
			// this is rarely happened. However we have delayed for
			// a bit log time. so we could use it
			allAcked = false
		} else if unack < ack && unack != 0 {
			// we should wait the bigger unack follows up the ack
			// they (unack and ack) will be equivalent soon !
			return 0, fmt.Errorf("candidates should follow up unack[%d] ack[%d]", unack, ack)
		}
	}
	if allAcked && len(allAckValues) != 0 {
		// free to choose the maximum value. ascend order
		// the last one is the biggest
		sort.Sort(utils.Int64Slice(allAckValues))
		return bson.MongoTimestamp(allAckValues[len(allAckValues)-1]), nil
	}

	if len(candidates) == 0 {
		return 0, errors.New("no candidates ack values found")
	}
	// ascend order. first is the smallest
	sort.Sort(utils.Int64Slice(candidates))

	if candidates[0] == 0 {
		return 0, errors.New("smallest candidates is zero")
	}
	LOG.Info("calculateSyncerAckTs worker offset %v use lowest %d", candidates, candidates[0])
	return bson.MongoTimestamp(candidates[0]), nil
}

func (sync *OplogSyncer) Handle(log *oplog.PartialLog) {
	// 1. records audit log if need
	sync.journal.WriteRecord(log)
}

func (sync *OplogSyncer) RestAPI() {
	type Time struct {
		TimestampUnix int64  `json:"unix"`
		TimestampTime string `json:"time"`
	}
	type MongoTime struct {
		Time
		TimestampMongo string `json:"ts"`
	}

	type Info struct {
		Who         string     `json:"who"`
		Tag         string     `json:"tag"`
		ReplicaSet  string     `json:"replset"`
		Logs        uint64     `json:"logs_get"`
		LogsRepl    uint64     `json:"logs_repl"`
		LogsSuccess uint64     `json:"logs_success"`
		Tps         uint64     `json:"tps"`
		Lsn         *MongoTime `json:"lsn"`
		LsnAck      *MongoTime `json:"lsn_ack"`
		LsnCkpt     *MongoTime `json:"lsn_ckpt"`
		Now         *Time      `json:"now"`
	}

	utils.HttpApi.RegisterAPI("/repl", nimo.HttpGet, func([]byte) interface{} {
		return &Info{
			Who:         conf.Options.CollectorId,
			Tag:         utils.BRANCH,
			ReplicaSet:  sync.replset,
			Logs:        sync.replMetric.Get(),
			LogsRepl:    sync.replMetric.Apply(),
			LogsSuccess: sync.replMetric.Success(),
			Tps:         sync.replMetric.Tps(),
			Lsn: &MongoTime{TimestampMongo: utils.Int64ToString(sync.replMetric.LSN),
				Time: Time{TimestampUnix: utils.ExtractTs32(sync.replMetric.LSN),
					TimestampTime: utils.TimestampToString(utils.ExtractTs32(sync.replMetric.LSN))}},
			LsnCkpt: &MongoTime{TimestampMongo: utils.Int64ToString(sync.replMetric.LSNCheckpoint),
				Time: Time{TimestampUnix: utils.ExtractTs32(sync.replMetric.LSNCheckpoint),
					TimestampTime: utils.TimestampToString(utils.ExtractTs32(sync.replMetric.LSNCheckpoint))}},
			LsnAck: &MongoTime{TimestampMongo: utils.Int64ToString(sync.replMetric.LSNAck),
				Time: Time{TimestampUnix: utils.ExtractTs32(sync.replMetric.LSNAck),
					TimestampTime: utils.TimestampToString(utils.ExtractTs32(sync.replMetric.LSNAck))}},
			Now: &Time{TimestampUnix: time.Now().Unix(), TimestampTime: utils.TimestampToString(time.Now().Unix())},
		}
	})
}
