package msgtransfer

import (
	"context"
	"sync"
	"time"

	"github.com/OpenIMSDK/Open-IM-Server/pkg/common/config"
	"github.com/OpenIMSDK/Open-IM-Server/pkg/common/constant"
	"github.com/OpenIMSDK/Open-IM-Server/pkg/common/db/controller"
	"github.com/OpenIMSDK/Open-IM-Server/pkg/common/kafka"
	"github.com/OpenIMSDK/Open-IM-Server/pkg/common/log"
	"github.com/OpenIMSDK/Open-IM-Server/pkg/common/mcontext"
	pbMsg "github.com/OpenIMSDK/Open-IM-Server/pkg/proto/msg"
	"github.com/OpenIMSDK/Open-IM-Server/pkg/utils"
	"github.com/Shopify/sarama"
	"github.com/golang/protobuf/proto"
)

const ConsumerMsgs = 3
const AggregationMessages = 4
const MongoMessages = 5
const ChannelNum = 100

type MsgChannelValue struct {
	aggregationID string //maybe userID or super groupID
	ctx           context.Context
	ctxMsgList    []*ContextMsg
}

type TriggerChannelValue struct {
	ctx      context.Context
	cMsgList []*sarama.ConsumerMessage
}

type Cmd2Value struct {
	Cmd   int
	Value interface{}
}
type ContextMsg struct {
	message *pbMsg.MsgDataToMQ
	ctx     context.Context
}

type OnlineHistoryRedisConsumerHandler struct {
	historyConsumerGroup *kafka.MConsumerGroup
	chArrays             [ChannelNum]chan Cmd2Value
	msgDistributionCh    chan Cmd2Value

	singleMsgSuccessCount      uint64
	singleMsgFailedCount       uint64
	singleMsgSuccessCountMutex sync.Mutex
	singleMsgFailedCountMutex  sync.Mutex

	msgDatabase controller.MsgDatabase
}

func NewOnlineHistoryRedisConsumerHandler(database controller.MsgDatabase) *OnlineHistoryRedisConsumerHandler {
	var och OnlineHistoryRedisConsumerHandler
	och.msgDatabase = database
	och.msgDistributionCh = make(chan Cmd2Value) //no buffer channel
	go och.MessagesDistributionHandle()
	for i := 0; i < ChannelNum; i++ {
		och.chArrays[i] = make(chan Cmd2Value, 50)
		go och.Run(i)
	}
	och.historyConsumerGroup = kafka.NewMConsumerGroup(&kafka.MConsumerGroupConfig{KafkaVersion: sarama.V2_0_0_0,
		OffsetsInitial: sarama.OffsetNewest, IsReturnErr: false}, []string{config.Config.Kafka.Ws2mschat.Topic},
		config.Config.Kafka.Ws2mschat.Addr, config.Config.Kafka.ConsumerGroupID.MsgToRedis)
	//statistics.NewStatistics(&och.singleMsgSuccessCount, config.Config.ModuleName.MsgTransferName, fmt.Sprintf("%d second singleMsgCount insert to mongo", constant.StatisticsTimeInterval), constant.StatisticsTimeInterval)
	return &och
}

func (och *OnlineHistoryRedisConsumerHandler) Run(channelID int) {
	for {
		select {
		case cmd := <-och.chArrays[channelID]:
			switch cmd.Cmd {
			case AggregationMessages:
				msgChannelValue := cmd.Value.(MsgChannelValue)
				ctxMsgList := msgChannelValue.ctxMsgList
				ctx := msgChannelValue.ctx
				storageMsgList := make([]*pbMsg.MsgDataToMQ, 0, 80)
				notStorageMsgList := make([]*pbMsg.MsgDataToMQ, 0, 80)
				storageNotificationList := make([]*pbMsg.MsgDataToMQ, 0, 80)
				notStorageNotificationList := make([]*pbMsg.MsgDataToMQ, 0, 80)
				modifyMsgList := make([]*pbMsg.MsgDataToMQ, 0, 80)
				log.ZDebug(ctx, "msg arrived channel", "channel id", channelID, "msgList length", len(ctxMsgList), "aggregationID", msgChannelValue.aggregationID)
				storageMsgList, notStorageMsgList, storageNotificationList, notStorageNotificationList, modifyMsgList = och.getPushStorageMsgList(msgChannelValue.aggregationID, ctxMsgList)
				och.handleMsg(ctx, msgChannelValue.aggregationID, storageMsgList, notStorageMsgList)
				och.handleNotification(ctx, msgChannelValue.aggregationID, storageNotificationList, notStorageNotificationList)
				if err := och.msgDatabase.MsgToModifyMQ(ctx, msgChannelValue.aggregationID, modifyMsgList); err != nil {
					log.ZError(ctx, "msg to modify mq error", err, "aggregationID", msgChannelValue.aggregationID, "modifyMsgList", modifyMsgList)
				}
			}
		}
	}
}

// 获取消息/通知 存储的消息列表， 不存储并且推送的消息列表，
func (och *OnlineHistoryRedisConsumerHandler) getPushStorageMsgList(aggregationID string, totalMsgs []*ContextMsg) (storageMsgList, notStorageMsgList, storageNotificatoinList, notStorageNotificationList, modifyMsgList []*pbMsg.MsgDataToMQ) {
	isStorage := func(msg *pbMsg.MsgDataToMQ) bool {
		options2 := utils.Options(msg.MsgData.Options)
		if options2.IsHistory() {
			return true
		} else {
			if !(!options2.IsSenderSync() && aggregationID == msg.MsgData.SendID) {
				return false
			}
		}
		return false
	}
	for _, v := range totalMsgs {
		options := utils.Options(v.message.MsgData.Options)
		if options.IsNotification() {
			// 原通知
			notificationMsg := proto.Clone(v.message).(*pbMsg.MsgDataToMQ)
			if options.IsSendMsg() {
				// 消息
				v.message.MsgData.Options = utils.WithOptions(utils.Options(v.message.MsgData.Options), utils.WithNotification(false), utils.WithSendMsg(false))
				storageMsgList = append(storageMsgList, v.message)
			}
			if isStorage(notificationMsg) {
				storageNotificatoinList = append(storageNotificatoinList, notificationMsg)
			} else {
				notStorageNotificationList = append(notStorageNotificationList, notificationMsg)
			}
		} else {
			if isStorage(v.message) {
				storageMsgList = append(storageMsgList, v.message)
			} else {
				notStorageMsgList = append(notStorageMsgList, v.message)
			}
		}
		if v.message.MsgData.ContentType == constant.ReactionMessageModifier || v.message.MsgData.ContentType == constant.ReactionMessageDeleter {
			modifyMsgList = append(modifyMsgList, v.message)
		}
	}
	return
}

func (och *OnlineHistoryRedisConsumerHandler) handleMsg(ctx context.Context, aggregationID string, storageList, notStorageList []*pbMsg.MsgDataToMQ) {
	och.handle(ctx, aggregationID, storageList, notStorageList, och.msgDatabase.BatchInsertChat2Cache)
}

func (och *OnlineHistoryRedisConsumerHandler) handleNotification(ctx context.Context, aggregationID string, storageList, notStorageList []*pbMsg.MsgDataToMQ) {
	och.handle(ctx, aggregationID, storageList, notStorageList, och.msgDatabase.NotificationBatchInsertChat2Cache)
}

func (och *OnlineHistoryRedisConsumerHandler) handle(ctx context.Context, aggregationID string, storageList, notStorageList []*pbMsg.MsgDataToMQ, cacheAndIncr func(ctx context.Context, sourceID string, msgList []*pbMsg.MsgDataToMQ) (int64, error)) {
	if len(storageList) > 0 {
		lastSeq, err := cacheAndIncr(ctx, aggregationID, storageList)
		if err != nil {
			log.ZError(ctx, "batch data insert to redis err", err, "storageMsgList", storageList)
			och.singleMsgFailedCountMutex.Lock()
			och.singleMsgFailedCount += uint64(len(storageList))
			och.singleMsgFailedCountMutex.Unlock()
		} else {
			och.singleMsgSuccessCountMutex.Lock()
			och.singleMsgSuccessCount += uint64(len(storageList))
			och.singleMsgSuccessCountMutex.Unlock()
			och.msgDatabase.MsgToMongoMQ(ctx, aggregationID, storageList, lastSeq)
			for _, v := range storageList {
				och.msgDatabase.MsgToPushMQ(ctx, aggregationID, v)
			}
		}
	}
	if len(notStorageList) > 0 {
		for _, v := range notStorageList {
			och.msgDatabase.MsgToPushMQ(ctx, aggregationID, v)
		}
	}
}

func (och *OnlineHistoryRedisConsumerHandler) MessagesDistributionHandle() {
	for {
		aggregationMsgs := make(map[string][]*ContextMsg, ChannelNum)
		select {
		case cmd := <-och.msgDistributionCh:
			switch cmd.Cmd {
			case ConsumerMsgs:
				triggerChannelValue := cmd.Value.(TriggerChannelValue)
				ctx := triggerChannelValue.ctx
				consumerMessages := triggerChannelValue.cMsgList
				//Aggregation map[userid]message list
				log.ZDebug(ctx, "batch messages come to distribution center", "length", len(consumerMessages))
				for i := 0; i < len(consumerMessages); i++ {
					ctxMsg := &ContextMsg{}
					msgFromMQ := pbMsg.MsgDataToMQ{}
					err := proto.Unmarshal(consumerMessages[i].Value, &msgFromMQ)
					if err != nil {
						log.ZError(ctx, "msg_transfer Unmarshal msg err", err, string(consumerMessages[i].Value))
						return
					}
					ctxMsg.ctx = kafka.GetContextWithMQHeader(consumerMessages[i].Headers)
					ctxMsg.message = &msgFromMQ
					log.ZDebug(ctx, "single msg come to distribution center", msgFromMQ.String(), string(consumerMessages[i].Key))
					//aggregationMsgs[string(consumerMessages[i].Key)] = append(aggregationMsgs[string(consumerMessages[i].Key)], ctxMsg)
					if oldM, ok := aggregationMsgs[string(consumerMessages[i].Key)]; ok {
						oldM = append(oldM, ctxMsg)
						aggregationMsgs[string(consumerMessages[i].Key)] = oldM
					} else {
						m := make([]*ContextMsg, 0, 100)
						m = append(m, ctxMsg)
						aggregationMsgs[string(consumerMessages[i].Key)] = m
					}
				}
				log.ZDebug(ctx, "generate map list users len", "length", len(aggregationMsgs))
				for aggregationID, v := range aggregationMsgs {
					if len(v) >= 0 {
						hashCode := utils.GetHashCode(aggregationID)
						channelID := hashCode % ChannelNum
						log.ZDebug(ctx, "generate channelID", "hashCode", hashCode, "channelID", channelID, "aggregationID", aggregationID)
						och.chArrays[channelID] <- Cmd2Value{Cmd: AggregationMessages, Value: MsgChannelValue{aggregationID: aggregationID, ctxMsgList: v, ctx: ctx}}
					}
				}
			}
		}
	}
}

func (och *OnlineHistoryRedisConsumerHandler) Setup(_ sarama.ConsumerGroupSession) error { return nil }
func (och *OnlineHistoryRedisConsumerHandler) Cleanup(_ sarama.ConsumerGroupSession) error {
	return nil
}

func (och *OnlineHistoryRedisConsumerHandler) ConsumeClaim(sess sarama.ConsumerGroupSession, claim sarama.ConsumerGroupClaim) error { // a instance in the consumer group
	for {
		if sess == nil {
			log.ZWarn(context.Background(), "sess == nil, waiting", nil)
			time.Sleep(100 * time.Millisecond)
		} else {
			break
		}
	}
	rwLock := new(sync.RWMutex)
	log.ZDebug(context.Background(), "online new session msg come", "highWaterMarkOffset",
		claim.HighWaterMarkOffset(), "topic", claim.Topic(), "partition", claim.Partition())
	cMsg := make([]*sarama.ConsumerMessage, 0, 1000)
	t := time.NewTicker(time.Millisecond * 100)
	go func() {
		for {
			select {
			case <-t.C:
				if len(cMsg) > 0 {
					rwLock.Lock()
					ccMsg := make([]*sarama.ConsumerMessage, 0, 1000)
					for _, v := range cMsg {
						ccMsg = append(ccMsg, v)
					}
					cMsg = make([]*sarama.ConsumerMessage, 0, 1000)
					rwLock.Unlock()
					split := 1000
					ctx := mcontext.WithTriggerIDContext(context.Background(), utils.OperationIDGenerator())
					log.ZDebug(ctx, "timer trigger msg consumer start", "length", len(ccMsg))
					for i := 0; i < len(ccMsg)/split; i++ {
						//log.Debug()
						och.msgDistributionCh <- Cmd2Value{Cmd: ConsumerMsgs, Value: TriggerChannelValue{
							ctx: ctx, cMsgList: ccMsg[i*split : (i+1)*split]}}
					}
					if (len(ccMsg) % split) > 0 {
						och.msgDistributionCh <- Cmd2Value{Cmd: ConsumerMsgs, Value: TriggerChannelValue{
							ctx: ctx, cMsgList: ccMsg[split*(len(ccMsg)/split):]}}
					}
					log.ZDebug(ctx, "timer trigger msg consumer end", "length", len(ccMsg))
				}
			}
		}
	}()
	for msg := range claim.Messages() {
		rwLock.Lock()
		if len(msg.Value) != 0 {
			cMsg = append(cMsg, msg)
		}
		rwLock.Unlock()
		sess.MarkMessage(msg, "")
	}
	return nil
}
