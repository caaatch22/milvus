package milvus

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/milvus-io/milvus/internal/storage"

	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/milvus-io/milvus/internal/util/logutil"

	"github.com/milvus-io/milvus/internal/util/paramtable"

	pb "github.com/milvus-io/milvus/internal/proto/etcdpb"

	"github.com/milvus-io/milvus/internal/proto/commonpb"
	"github.com/milvus-io/milvus/internal/proto/querypb"

	"github.com/golang/protobuf/proto"
	etcdkv "github.com/milvus-io/milvus/internal/kv/etcd"
	"github.com/milvus-io/milvus/internal/log"
	"github.com/milvus-io/milvus/internal/proto/datapb"
	"github.com/milvus-io/milvus/internal/util/etcd"
	"go.uber.org/zap"
)

const (
	MckCmd       = "mck"
	MckTypeRun   = "run"
	MckTypeClean = "cleanTrash"

	segmentPrefix     = "datacoord-meta/s"
	collectionPrefix  = "snapshots/root-coord/collection"
	triggerTaskPrefix = "queryCoord-triggerTask"
	activeTaskPrefix  = "queryCoord-activeTask"
	taskInfoPrefix    = "queryCoord-taskInfo"
	MckTrash          = "mck-trash"
)

type mck struct {
	params              paramtable.GrpcServerConfig
	taskKeyMap          map[int64]string
	taskNameMap         map[int64]string
	allTaskInfo         map[string]string
	partitionIDToTasks  map[int64][]int64
	segmentIDToTasks    map[int64][]int64
	taskIDToInvalidPath map[int64][]string
	segmentIDMap        map[int64]*datapb.SegmentInfo
	partitionIDMap      map[int64]struct{}
	etcdKV              *etcdkv.EtcdKV
	minioChunkManager   storage.ChunkManager

	etcdIP          string
	ectdRootPath    string
	minioAddress    string
	minioUsername   string
	minioPassword   string
	minioUseSSL     string
	minioBucketName string

	flagStartIndex int
}

func (c *mck) execute(args []string, flags *flag.FlagSet) {
	if len(args) < 3 {
		fmt.Fprintln(os.Stderr, mckLine)
		return
	}
	c.initParam()
	flags.Usage = func() {
		fmt.Fprintln(os.Stderr, mckLine)
	}

	logutil.SetupLogger(&log.Config{
		Level: "info",
		File: log.FileLogConfig{
			Filename: fmt.Sprintf("mck-%s.log", time.Now().Format("20060102150405.99")),
		},
	})

	mckType := args[2]
	c.flagStartIndex = 2
	isClean := mckType == MckTypeClean
	if isClean {
		c.flagStartIndex = 3
	}
	c.formatFlags(args, flags)
	c.connectEctd()

	switch mckType {
	case MckTypeRun:
		c.run()
	case MckTypeClean:
		c.cleanTrash()
		return
	default:
		fmt.Fprintln(os.Stderr, mckLine)
		return
	}
}

func (c *mck) run() {
	c.connectMinio()

	_, values, err := c.etcdKV.LoadWithPrefix(segmentPrefix)
	if err != nil {
		log.Fatal("failed to list the segment info", zap.String("key", segmentPrefix), zap.Error(err))
	}
	for _, value := range values {
		info := &datapb.SegmentInfo{}
		err = proto.Unmarshal([]byte(value), info)
		if err != nil {
			log.Warn("fail to unmarshal the segment info", zap.Error(err))
			continue
		}
		c.segmentIDMap[info.ID] = info
	}

	_, values, err = c.etcdKV.LoadWithPrefix(collectionPrefix)
	if err != nil {
		log.Fatal("failed to list the collection info", zap.String("key", collectionPrefix), zap.Error(err))
	}
	for _, value := range values {
		collInfo := pb.CollectionInfo{}
		err = proto.Unmarshal([]byte(value), &collInfo)
		if err != nil {
			log.Warn("fail to unmarshal the collection info", zap.Error(err))
			continue
		}
		for _, id := range collInfo.PartitionIDs {
			c.partitionIDMap[id] = struct{}{}
		}
	}

	// log Segment ids and partition ids
	var ids []int64
	for id := range c.segmentIDMap {
		ids = append(ids, id)
	}
	log.Info("segment ids", zap.Int64s("ids", ids))

	ids = []int64{}
	for id := range c.partitionIDMap {
		ids = append(ids, id)
	}
	log.Info("partition ids", zap.Int64s("ids", ids))

	keys, values, err := c.etcdKV.LoadWithPrefix(triggerTaskPrefix)
	if err != nil {
		log.Fatal("failed to list the trigger task info", zap.Error(err))
	}
	c.extractTask(triggerTaskPrefix, keys, values)

	keys, values, err = c.etcdKV.LoadWithPrefix(activeTaskPrefix)
	if err != nil {
		log.Fatal("failed to list the active task info", zap.Error(err))
	}
	c.extractTask(activeTaskPrefix, keys, values)

	// log all tasks
	if len(c.taskNameMap) > 0 {
		log.Info("all tasks")
		for taskID, taskName := range c.taskNameMap {
			log.Info("task info", zap.String("name", taskName), zap.Int64("id", taskID))
		}
	}

	// collect invalid tasks
	invalidTasks := c.collectInvalidTask()
	if len(invalidTasks) > 0 {
		line()
		fmt.Println("All invalid tasks")
		for _, invalidTask := range invalidTasks {
			line2()
			fmt.Printf("TaskID: %d\t%s\n", invalidTask, c.taskNameMap[invalidTask])
		}
	}
	c.handleUserEnter(invalidTasks)
}

func (c *mck) initParam() {
	c.taskKeyMap = make(map[int64]string)
	c.taskNameMap = make(map[int64]string)
	c.allTaskInfo = make(map[string]string)
	c.partitionIDToTasks = make(map[int64][]int64)
	c.segmentIDToTasks = make(map[int64][]int64)
	c.taskIDToInvalidPath = make(map[int64][]string)
	c.segmentIDMap = make(map[int64]*datapb.SegmentInfo)
	c.partitionIDMap = make(map[int64]struct{})
}

func (c *mck) formatFlags(args []string, flags *flag.FlagSet) {
	flags.StringVar(&c.etcdIP, "etcdIp", "", "Etcd endpoint to connect")
	flags.StringVar(&c.ectdRootPath, "etcdRootPath", "", "Etcd root path")
	flags.StringVar(&c.minioAddress, "minioAddress", "", "Minio endpoint to connect")
	flags.StringVar(&c.minioUsername, "minioUsername", "", "Minio username")
	flags.StringVar(&c.minioPassword, "minioPassword", "", "Minio password")
	flags.StringVar(&c.minioUseSSL, "minioUseSSL", "", "Minio to use ssl")
	flags.StringVar(&c.minioBucketName, "minioBucketName", "", "Minio bucket name")

	if err := flags.Parse(os.Args[2:]); err != nil {
		log.Fatal("failed to parse flags", zap.Error(err))
	}
	log.Info("args", zap.Strings("args", args))
}

func (c *mck) connectEctd() {
	c.params.Init()
	var etcdCli *clientv3.Client
	var err error
	if c.etcdIP != "" {
		etcdCli, err = etcd.GetRemoteEtcdClient([]string{c.etcdIP})
	} else {
		etcdCli, err = etcd.GetEtcdClient(&c.params.EtcdCfg)
	}
	if err != nil {
		log.Fatal("failed to connect to etcd", zap.Error(err))
	}

	rootPath := getConfigValue(c.ectdRootPath, c.params.EtcdCfg.MetaRootPath, "ectd_root_path")
	c.etcdKV = etcdkv.NewEtcdKV(etcdCli, rootPath)
	log.Info("Etcd root path", zap.String("root_path", rootPath))
}

func (c *mck) connectMinio() {
	useSSL := c.params.MinioCfg.UseSSL
	if c.minioUseSSL == "true" || c.minioUseSSL == "false" {
		minioUseSSL, err := strconv.ParseBool(c.minioUseSSL)
		if err != nil {
			log.Panic("fail to parse the 'minioUseSSL' string to the bool value", zap.String("minioUseSSL", c.minioUseSSL), zap.Error(err))
		}
		useSSL = minioUseSSL
	}
	chunkManagerFactory := storage.NewChunkManagerFactory("local", "minio",
		storage.RootPath(c.params.LocalStorageCfg.Path),
		storage.Address(getConfigValue(c.minioAddress, c.params.MinioCfg.Address, "minio_address")),
		storage.AccessKeyID(getConfigValue(c.minioUsername, c.params.MinioCfg.AccessKeyID, "minio_username")),
		storage.SecretAccessKeyID(getConfigValue(c.minioPassword, c.params.MinioCfg.SecretAccessKey, "minio_password")),
		storage.UseSSL(useSSL),
		storage.BucketName(getConfigValue(c.minioBucketName, c.params.MinioCfg.BucketName, "minio_bucket_name")),
		storage.CreateBucket(true))

	var err error
	c.minioChunkManager, err = chunkManagerFactory.NewVectorStorageChunkManager(context.Background())
	if err != nil {
		log.Fatal("failed to connect to etcd", zap.Error(err))
	}
}

func getConfigValue(a string, b string, name string) string {
	if a != "" {
		return a
	}
	if b != "" {
		return b
	}
	log.Panic(fmt.Sprintf("the config '%s' is empty", name))
	return ""
}

func (c *mck) cleanTrash() {
	keys, _, err := c.etcdKV.LoadWithPrefix(MckTrash)
	if err != nil {
		log.Error("failed to load backup info", zap.Error(err))
		return
	}
	if len(keys) == 0 {
		fmt.Println("Empty backup task info")
		return
	}
	fmt.Println(strings.Join(keys, "\n"))
	fmt.Print("Delete all backup infos, [Y/n]:")
	deleteAll := ""
	fmt.Scanln(&deleteAll)
	if deleteAll == "Y" {
		err = c.etcdKV.RemoveWithPrefix(MckTrash)
		if err != nil {
			log.Error("failed to remove backup infos", zap.String("key", MckTrash), zap.Error(err))
			return
		}
	}
}

func (c *mck) collectInvalidTask() []int64 {
	var invalidTasks []int64
	invalidTasks = append(invalidTasks, c.collectInvalidTaskForPartition()...)
	invalidTasks = append(invalidTasks, c.collectInvalidTaskForSegment()...)
	invalidTasks = append(invalidTasks, c.collectInvalidTaskForMinioPath()...)
	invalidTasks = removeRepeatElement(invalidTasks)
	return invalidTasks
}

// collect invalid tasks related with invalid partitions
func (c *mck) collectInvalidTaskForPartition() []int64 {
	var invalidTasksOfPartition []int64
	var buffer bytes.Buffer
	for id, tasks := range c.partitionIDToTasks {
		if _, ok := c.partitionIDMap[id]; !ok {
			invalidTasksOfPartition = append(invalidTasksOfPartition, tasks...)
			buffer.WriteString(fmt.Sprintf("Partition ID: %d\n", id))
			buffer.WriteString(fmt.Sprintf("Tasks: %v\n", tasks))
		}
	}
	invalidTasksOfPartition = removeRepeatElement(invalidTasksOfPartition)
	if len(invalidTasksOfPartition) > 0 {
		line()
		fmt.Println("Invalid Partitions")
		fmt.Println(buffer.String())
	}
	return invalidTasksOfPartition
}

// collect invalid tasks related with invalid segments
func (c *mck) collectInvalidTaskForSegment() []int64 {
	var invalidTasksOfSegment []int64
	var buffer bytes.Buffer
	if len(c.segmentIDToTasks) > 0 {
		for id, tasks := range c.segmentIDToTasks {
			if _, ok := c.segmentIDMap[id]; !ok {
				invalidTasksOfSegment = append(invalidTasksOfSegment, tasks...)
				buffer.WriteString(fmt.Sprintf("Segment ID: %d\n", id))
				buffer.WriteString(fmt.Sprintf("Tasks: %v\n", tasks))
			}
		}
		invalidTasksOfSegment = removeRepeatElement(invalidTasksOfSegment)
	}
	if len(invalidTasksOfSegment) > 0 {
		line()
		fmt.Println("Invalid Segments")
		fmt.Println(buffer.String())
	}
	return invalidTasksOfSegment
}

// collect invalid tasks related with incorrect file paths in minio
func (c *mck) collectInvalidTaskForMinioPath() []int64 {
	var invalidTasksOfPath []int64
	if len(c.taskIDToInvalidPath) > 0 {
		line()
		fmt.Println("Invalid Paths")
		for id, paths := range c.taskIDToInvalidPath {
			fmt.Printf("Task ID: %d\n", id)
			for _, path := range paths {
				fmt.Printf("\t%s\n", path)
			}
			invalidTasksOfPath = append(invalidTasksOfPath, id)
		}
		invalidTasksOfPath = removeRepeatElement(invalidTasksOfPath)
	}
	return invalidTasksOfPath
}

func redPrint(msg string) {
	fmt.Printf("\033[0;31;40m%s\033[0m", msg)
}

func line() {
	fmt.Println("================================================================================")
}

func line2() {
	fmt.Println("--------------------------------------------------------------------------------")
}

func getTrashKey(taskType, key string) string {
	return fmt.Sprintf("%s/%s/%s", MckTrash, taskType, key)
}

func (c *mck) extractTask(prefix string, keys []string, values []string) {

	for i := range keys {
		taskID, err := strconv.ParseInt(filepath.Base(keys[i]), 10, 64)
		if err != nil {
			log.Warn("failed to parse int", zap.String("key", filepath.Base(keys[i])), zap.String("tasks", filepath.Base(keys[i])))
			continue
		}

		taskName, pids, sids, err := c.unmarshalTask(taskID, values[i])
		if err != nil {
			log.Warn("failed to unmarshal task", zap.Int64("task_id", taskID))
			continue
		}
		for _, pid := range pids {
			c.partitionIDToTasks[pid] = append(c.partitionIDToTasks[pid], taskID)
		}
		for _, sid := range sids {
			c.segmentIDToTasks[sid] = append(c.segmentIDToTasks[sid], taskID)
		}

		c.taskKeyMap[taskID] = fmt.Sprintf("%s/%d", prefix, taskID)
		c.allTaskInfo[c.taskKeyMap[taskID]] = values[i]
		c.taskNameMap[taskID] = taskName
	}
}

func (c *mck) removeTask(invalidTask int64) bool {
	taskType := c.taskNameMap[invalidTask]
	key := c.taskKeyMap[invalidTask]
	err := c.etcdKV.Save(getTrashKey(taskType, key), c.allTaskInfo[key])
	if err != nil {
		log.Warn("failed to backup task", zap.String("key", getTrashKey(taskType, key)), zap.Int64("task_id", invalidTask), zap.Error(err))
		return false
	}
	fmt.Printf("Back up task successfully, back path: %s\n", getTrashKey(taskType, key))
	err = c.etcdKV.Remove(key)
	if err != nil {
		log.Warn("failed to remove task", zap.Int64("task_id", invalidTask), zap.Error(err))
		return false
	}

	key = fmt.Sprintf("%s/%d", taskInfoPrefix, invalidTask)
	taskInfo, err := c.etcdKV.Load(key)
	if err != nil {
		log.Warn("failed to load task info", zap.Int64("task_id", invalidTask), zap.Error(err))
		return false
	}
	err = c.etcdKV.Save(getTrashKey(taskType, key), taskInfo)
	if err != nil {
		log.Warn("failed to backup task info", zap.Int64("task_id", invalidTask), zap.Error(err))
		return false
	}
	fmt.Printf("Back up task info successfully, back path: %s\n", getTrashKey(taskType, key))
	err = c.etcdKV.Remove(key)
	if err != nil {
		log.Warn("failed to remove task info", zap.Int64("task_id", invalidTask), zap.Error(err))
	}
	return true
}

func emptyInt64() []int64 {
	return []int64{}
}

func errReturn(taskID int64, pbName string, err error) (string, []int64, []int64, error) {
	return "", emptyInt64(), emptyInt64(), fmt.Errorf("task id: %d, failed to unmarshal %s, err %s ", taskID, pbName, err.Error())
}

func int64Map(ids []int64) map[int64]interface{} {
	idMap := make(map[int64]interface{})
	for _, id := range ids {
		idMap[id] = nil
	}
	return idMap
}

func removeRepeatElement(ids []int64) []int64 {
	idMap := int64Map(ids)
	elements := make([]int64, 0, len(idMap))
	for k := range idMap {
		elements = append(elements, k)
	}
	return elements
}

func (c *mck) extractDataSegmentInfos(taskID int64, infos []*datapb.SegmentInfo) ([]int64, []int64) {
	var partitionIDs []int64
	var segmentIDs []int64
	for _, info := range infos {
		partitionIDs = append(partitionIDs, info.PartitionID)
		segmentIDs = append(segmentIDs, info.ID)
		c.extractFieldBinlog(taskID, info.Binlogs)
		c.extractFieldBinlog(taskID, info.Statslogs)
		c.extractFieldBinlog(taskID, info.Deltalogs)
	}

	return partitionIDs, segmentIDs
}

func (c *mck) extractQuerySegmentInfos(taskID int64, infos []*querypb.SegmentInfo) ([]int64, []int64) {
	var partitionIDs []int64
	var segmentIDs []int64
	for _, info := range infos {
		partitionIDs = append(partitionIDs, info.PartitionID)
		segmentIDs = append(segmentIDs, info.SegmentID)
		c.extractVecFieldIndexInfo(taskID, info.IndexInfos)
	}

	return partitionIDs, segmentIDs
}

func (c *mck) extractVchannelInfo(taskID int64, infos []*datapb.VchannelInfo) ([]int64, []int64) {
	var partitionIDs []int64
	var segmentIDs []int64
	for _, info := range infos {
		pids, sids := c.extractDataSegmentInfos(taskID, info.DroppedSegments)
		partitionIDs = append(partitionIDs, pids...)
		segmentIDs = append(segmentIDs, sids...)

		pids, sids = c.extractDataSegmentInfos(taskID, info.FlushedSegments)
		partitionIDs = append(partitionIDs, pids...)
		segmentIDs = append(segmentIDs, sids...)

		pids, sids = c.extractDataSegmentInfos(taskID, info.UnflushedSegments)
		partitionIDs = append(partitionIDs, pids...)
		segmentIDs = append(segmentIDs, sids...)
	}
	return partitionIDs, segmentIDs
}

func (c *mck) extractFieldBinlog(taskID int64, fieldBinlogList []*datapb.FieldBinlog) {
	for _, fieldBinlog := range fieldBinlogList {
		for _, binlog := range fieldBinlog.Binlogs {
			ok, _ := c.minioChunkManager.Exist(binlog.LogPath)
			if !ok {
				c.taskIDToInvalidPath[taskID] = append(c.taskIDToInvalidPath[taskID], binlog.LogPath)
			}
		}
	}
}

func (c *mck) extractVecFieldIndexInfo(taskID int64, infos []*querypb.FieldIndexInfo) {
	for _, info := range infos {
		for _, indexPath := range info.IndexFilePaths {
			ok, _ := c.minioChunkManager.Exist(indexPath)
			if !ok {
				c.taskIDToInvalidPath[taskID] = append(c.taskIDToInvalidPath[taskID], indexPath)
			}
		}
	}
}

// return partitionIDs,segmentIDs,error
func (c *mck) unmarshalTask(taskID int64, t string) (string, []int64, []int64, error) {
	header := commonpb.MsgHeader{}
	err := proto.Unmarshal([]byte(t), &header)

	if err != nil {
		return errReturn(taskID, "MsgHeader", err)
	}

	switch header.Base.MsgType {
	case commonpb.MsgType_LoadCollection:
		loadReq := querypb.LoadCollectionRequest{}
		err = proto.Unmarshal([]byte(t), &loadReq)
		if err != nil {
			return errReturn(taskID, "LoadCollectionRequest", err)
		}
		log.Info("LoadCollection", zap.String("detail", fmt.Sprintf("+%v", loadReq)))
		return "LoadCollection", emptyInt64(), emptyInt64(), nil
	case commonpb.MsgType_LoadPartitions:
		loadReq := querypb.LoadPartitionsRequest{}
		err = proto.Unmarshal([]byte(t), &loadReq)
		if err != nil {
			return errReturn(taskID, "LoadPartitionsRequest", err)
		}
		log.Info("LoadPartitions", zap.String("detail", fmt.Sprintf("+%v", loadReq)))
		return "LoadPartitions", loadReq.PartitionIDs, emptyInt64(), nil
	case commonpb.MsgType_ReleaseCollection:
		loadReq := querypb.ReleaseCollectionRequest{}
		err = proto.Unmarshal([]byte(t), &loadReq)
		if err != nil {
			return errReturn(taskID, "ReleaseCollectionRequest", err)
		}
		log.Info("ReleaseCollection", zap.String("detail", fmt.Sprintf("+%v", loadReq)))
		return "ReleaseCollection", emptyInt64(), emptyInt64(), nil
	case commonpb.MsgType_ReleasePartitions:
		loadReq := querypb.ReleasePartitionsRequest{}
		err = proto.Unmarshal([]byte(t), &loadReq)
		if err != nil {
			return errReturn(taskID, "ReleasePartitionsRequest", err)
		}
		log.Info("ReleasePartitions", zap.String("detail", fmt.Sprintf("+%v", loadReq)))
		return "ReleasePartitions", loadReq.PartitionIDs, emptyInt64(), nil
	case commonpb.MsgType_LoadSegments:
		loadReq := querypb.LoadSegmentsRequest{}
		err = proto.Unmarshal([]byte(t), &loadReq)
		if err != nil {
			return errReturn(taskID, "LoadSegmentsRequest", err)
		}

		fmt.Printf("LoadSegments, task-id: %d, %+v\n", taskID, loadReq)

		var partitionIDs []int64
		var segmentIDs []int64
		if loadReq.LoadMeta != nil {
			partitionIDs = append(partitionIDs, loadReq.LoadMeta.PartitionIDs...)
		}
		for _, info := range loadReq.Infos {
			partitionIDs = append(partitionIDs, info.PartitionID)
			segmentIDs = append(segmentIDs, info.SegmentID)
			c.extractFieldBinlog(taskID, info.BinlogPaths)
			c.extractFieldBinlog(taskID, info.Statslogs)
			c.extractFieldBinlog(taskID, info.Deltalogs)
			c.extractVecFieldIndexInfo(taskID, info.IndexInfos)
		}
		log.Info("LoadSegments", zap.String("detail", fmt.Sprintf("+%v", loadReq)))
		return "LoadSegments", removeRepeatElement(partitionIDs), removeRepeatElement(segmentIDs), nil
	case commonpb.MsgType_ReleaseSegments:
		loadReq := querypb.ReleaseSegmentsRequest{}
		err = proto.Unmarshal([]byte(t), &loadReq)
		if err != nil {
			return errReturn(taskID, "ReleaseSegmentsRequest", err)
		}
		log.Info("ReleaseSegments", zap.String("detail", fmt.Sprintf("+%v", loadReq)))
		return "ReleaseSegments", loadReq.PartitionIDs, loadReq.SegmentIDs, nil
	case commonpb.MsgType_WatchDmChannels:
		loadReq := querypb.WatchDmChannelsRequest{}
		err = proto.Unmarshal([]byte(t), &loadReq)
		if err != nil {
			return errReturn(taskID, "WatchDmChannelsRequest", err)
		}

		var partitionIDs []int64
		var segmentIDs []int64
		if loadReq.LoadMeta != nil {
			partitionIDs = append(partitionIDs, loadReq.LoadMeta.PartitionIDs...)
		}

		pids, sids := c.extractVchannelInfo(taskID, loadReq.Infos)
		partitionIDs = append(partitionIDs, pids...)
		segmentIDs = append(segmentIDs, sids...)
		pids, sids = c.extractDataSegmentInfos(taskID, loadReq.ExcludeInfos)
		partitionIDs = append(partitionIDs, pids...)
		segmentIDs = append(segmentIDs, sids...)
		log.Info("WatchDmChannels", zap.String("detail", fmt.Sprintf("+%v", loadReq)))
		return "WatchDmChannels", removeRepeatElement(partitionIDs), removeRepeatElement(segmentIDs), nil
	case commonpb.MsgType_WatchDeltaChannels:
		loadReq := querypb.WatchDeltaChannelsRequest{}
		err = proto.Unmarshal([]byte(t), &loadReq)
		if err != nil {
			return errReturn(taskID, "WatchDeltaChannelsRequest", err)
		}
		var partitionIDs []int64
		var segmentIDs []int64
		if loadReq.LoadMeta != nil {
			partitionIDs = append(partitionIDs, loadReq.LoadMeta.PartitionIDs...)
		}
		pids, sids := c.extractVchannelInfo(taskID, loadReq.Infos)
		partitionIDs = append(partitionIDs, pids...)
		segmentIDs = append(segmentIDs, sids...)
		log.Info("WatchDeltaChannels", zap.String("detail", fmt.Sprintf("+%v", loadReq)))
		return "WatchDeltaChannels", removeRepeatElement(partitionIDs), removeRepeatElement(segmentIDs), nil
	case commonpb.MsgType_WatchQueryChannels:
		log.Warn("legacy WatchQueryChannels type found, ignore")
		return "WatchQueryChannels", emptyInt64(), emptyInt64(), nil
	case commonpb.MsgType_LoadBalanceSegments:
		loadReq := querypb.LoadBalanceRequest{}
		err = proto.Unmarshal([]byte(t), &loadReq)
		if err != nil {
			return errReturn(taskID, "LoadBalanceRequest", err)
		}
		log.Info("LoadBalanceSegments", zap.String("detail", fmt.Sprintf("+%v", loadReq)))
		return "LoadBalanceSegments", emptyInt64(), loadReq.SealedSegmentIDs, nil
	case commonpb.MsgType_HandoffSegments:
		handoffReq := querypb.HandoffSegmentsRequest{}
		err = proto.Unmarshal([]byte(t), &handoffReq)
		if err != nil {
			return errReturn(taskID, "HandoffSegmentsRequest", err)
		}
		pids, sids := c.extractQuerySegmentInfos(taskID, handoffReq.SegmentInfos)
		log.Info("HandoffSegments", zap.String("detail", fmt.Sprintf("+%v", handoffReq)))
		return "HandoffSegments", pids, sids, nil
	default:
		err = errors.New("inValid msg type when unMarshal task")
		log.Error("invalid message task", zap.Int("type", int(header.Base.MsgType)), zap.Error(err))
		return "", emptyInt64(), emptyInt64(), err
	}
}

func (c *mck) handleUserEnter(invalidTasks []int64) {
	if len(invalidTasks) > 0 {
		fmt.Print("Delete all invalid tasks, [Y/n]:")
		deleteAll := ""
		fmt.Scanln(&deleteAll)
		if deleteAll == "Y" {
			for _, invalidTask := range invalidTasks {
				if !c.removeTask(invalidTask) {
					continue
				}
				fmt.Println("Delete task id: ", invalidTask)
			}
		} else {
			deleteTask := ""
			idMap := int64Map(invalidTasks)
			fmt.Println("Enter 'Exit' to end")
			for len(idMap) > 0 {
				fmt.Print("Enter a invalid task id to delete, [Exit]:")
				fmt.Scanln(&deleteTask)
				if deleteTask == "Exit" {
					return
				}
				invalidTask, err := strconv.ParseInt(deleteTask, 10, 64)
				if err != nil {
					fmt.Println("Invalid task id.")
					continue
				}
				if _, ok := idMap[invalidTask]; !ok {
					fmt.Println("This is not a invalid task id.")
					continue
				}
				if !c.removeTask(invalidTask) {
					continue
				}
				delete(idMap, invalidTask)
				fmt.Println("Delete task id: ", invalidTask)
			}
		}
	}
}
