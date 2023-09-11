// Copyright 2020 PingCAP, Inc.
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

package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"math"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/apache/pulsar-client-go/pulsar"
	"github.com/pingcap/errors"
	"github.com/pingcap/log"
	"github.com/pingcap/tiflow/cdc/model"
	"github.com/pingcap/tiflow/cdc/sink/ddlsink"
	ddlsinkfactory "github.com/pingcap/tiflow/cdc/sink/ddlsink/factory"
	eventsinkfactory "github.com/pingcap/tiflow/cdc/sink/dmlsink/factory"
	"github.com/pingcap/tiflow/cdc/sink/dmlsink/mq/dispatcher"
	"github.com/pingcap/tiflow/cdc/sink/tablesink"
	sutil "github.com/pingcap/tiflow/cdc/sink/util"
	cmdUtil "github.com/pingcap/tiflow/pkg/cmd/util"
	"github.com/pingcap/tiflow/pkg/config"
	"github.com/pingcap/tiflow/pkg/filter"
	"github.com/pingcap/tiflow/pkg/logutil"
	"github.com/pingcap/tiflow/pkg/quotes"
	"github.com/pingcap/tiflow/pkg/sink/codec"
	"github.com/pingcap/tiflow/pkg/sink/codec/canal"
	"github.com/pingcap/tiflow/pkg/sink/codec/common"
	"github.com/pingcap/tiflow/pkg/spanz"
	"github.com/pingcap/tiflow/pkg/util"
	"github.com/pingcap/tiflow/pkg/version"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

type consumerOption struct {
	address []string
	topic   string

	protocol            config.Protocol
	enableTiDBExtension bool

	// the replicaConfig of the changefeed which produce data to the kafka topic
	replicaConfig *config.ReplicaConfig

	logPath       string
	logLevel      string
	timezone      string
	ca, cert, key string

	downstreamURI string
	partitionNum  int
	// upstreamTiDBDSN is the dsn of the upstream TiDB cluster
	upstreamTiDBDSN string
}

func newConsumerOption() *consumerOption {
	return &consumerOption{
		protocol: config.ProtocolDefault,
	}
}

// Adjust the consumer option by the upstream uri passed in parameters.
func (o *consumerOption) Adjust(upstreamURI *url.URL, configFile string) error {
	// the default value of partitionNum is 1
	o.partitionNum = 1

	s := upstreamURI.Query().Get("version")

	o.topic = strings.TrimFunc(upstreamURI.Path, func(r rune) bool {
		return r == '/'
	})

	o.address = strings.Split(upstreamURI.Host, ",")

	s = upstreamURI.Query().Get("protocol")
	if s != "" {
		protocol, err := config.ParseSinkProtocolFromString(s)
		if err != nil {
			log.Panic("invalid protocol", zap.Error(err), zap.String("protocol", s))
		}
		if !sutil.IsPulsarSupportedProtocols(protocol) {
			log.Panic("unsupported protocol, pulsar sink currently only support these protocols: [canal-json, canal, maxwell]",
				zap.String("protocol", s))
		}
		o.protocol = protocol
	}

	s = upstreamURI.Query().Get("enable-tidb-extension")
	if s != "" {
		enableTiDBExtension, err := strconv.ParseBool(s)
		if err != nil {
			log.Panic("invalid enable-tidb-extension of upstream-uri")
		}
		if enableTiDBExtension {
			if o.protocol != config.ProtocolCanalJSON && o.protocol != config.ProtocolAvro {
				log.Panic("enable-tidb-extension only work with canal-json / avro")
			}
		}
		o.enableTiDBExtension = enableTiDBExtension
	}

	if configFile != "" {
		replicaConfig := config.GetDefaultReplicaConfig()
		replicaConfig.Sink.Protocol = util.AddressOf(o.protocol.String())
		err := cmdUtil.StrictDecodeFile(configFile, "pulsar consumer", replicaConfig)
		if err != nil {
			return errors.Trace(err)
		}
		if _, err := filter.VerifyTableRules(replicaConfig.Filter); err != nil {
			return errors.Trace(err)
		}
		o.replicaConfig = replicaConfig
	}

	log.Info("consumer option adjusted",
		zap.String("configFile", configFile),
		zap.String("address", strings.Join(o.address, ",")),
		zap.String("topic", o.topic),
		zap.Any("protocol", o.protocol),
		zap.Bool("enableTiDBExtension", o.enableTiDBExtension))
	return nil
}

func main() {
	consumerOption := newConsumerOption()

	var (
		upstreamURIStr string
		configFile     string
	)

	flag.StringVar(&configFile, "config", "", "config file for changefeed")

	flag.StringVar(&upstreamURIStr, "upstream-uri", "", "Kafka uri")
	flag.StringVar(&consumerOption.downstreamURI, "downstream-uri", "", "downstream sink uri")
	flag.StringVar(&consumerOption.upstreamTiDBDSN, "upstream-tidb-dsn", "", "upstream TiDB DSN")

	flag.StringVar(&consumerOption.logPath, "log-file", "cdc_kafka_consumer.log", "log file path")
	flag.StringVar(&consumerOption.logLevel, "log-level", "info", "log file path")
	flag.StringVar(&consumerOption.timezone, "tz", "System", "Specify time zone of Kafka consumer")
	flag.StringVar(&consumerOption.ca, "ca", "", "CA certificate path for Kafka SSL connection")
	flag.StringVar(&consumerOption.cert, "cert", "", "Certificate path for Kafka SSL connection")
	flag.StringVar(&consumerOption.key, "key", "", "Private key path for Kafka SSL connection")
	flag.Parse()

	err := logutil.InitLogger(&logutil.Config{
		Level: consumerOption.logLevel,
		File:  consumerOption.logPath,
	},
		logutil.WithInitGRPCLogger(),
		logutil.WithInitSaramaLogger(),
	)
	if err != nil {
		log.Error("init logger failed", zap.Error(err))
		return
	}

	version.LogVersionInfo("pulsar consumer")

	upstreamURI, err := url.Parse(upstreamURIStr)
	if err != nil {
		log.Panic("invalid upstream-uri", zap.Error(err))
	}
	scheme := strings.ToLower(upstreamURI.Scheme)
	if scheme != "pulsar" {
		log.Panic("invalid upstream-uri scheme, the scheme of upstream-uri must be `pulsar`",
			zap.String("upstreamURI", upstreamURIStr))
	}

	err = consumerOption.Adjust(upstreamURI, configFile)
	if err != nil {
		log.Panic("adjust consumer option failed", zap.Error(err))
	}

	ctx, cancel := context.WithCancel(context.Background())
	consumer, err := NewConsumer(ctx, consumerOption)
	if err != nil {
		log.Panic("Error creating pulsar consumer", zap.Error(err))
	}

	pulsarConsumer, client := NewPulsarConsumer(consumerOption)
	defer client.Close()
	defer pulsarConsumer.Close()

	wg := &sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		select {
		case <-ctx.Done():
			return
		case msg := <-pulsarConsumer.Chan():
			fmt.Printf("Received message msgId: %#v -- content: '%s'\n", msg.ID(), string(msg.Payload()))
			pulsarConsumer.Ack(msg)
			consumer.ready = make(chan bool)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := consumer.Run(ctx); err != nil {
			if err != context.Canceled {
				log.Panic("Error running consumer", zap.Error(err))
			}
		}
	}()

	<-consumer.ready // wait till the consumer has been set up
	log.Info("TiCDC consumer up and running!...")
	sigterm := make(chan os.Signal, 1)
	signal.Notify(sigterm, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-ctx.Done():
		log.Info("terminating: context cancelled")
	case <-sigterm:
		log.Info("terminating: via signal")
	}
	cancel()
	wg.Wait()
}

func NewPulsarConsumer(option *consumerOption) (pulsar.Consumer, pulsar.Client) {
	pulsarURL := strings.Join(option.address, ",")
	topicName := option.topic
	subscriptionName := "pulsar-test-subscription"

	client, err := pulsar.NewClient(pulsar.ClientOptions{
		URL: pulsarURL,
	})
	if err != nil {
		log.Fatal("can't create pulsar client: %v", zap.Error(err))
	}

	consumerConfig := pulsar.ConsumerOptions{
		Topic:            topicName,
		SubscriptionName: subscriptionName,
		Type:             pulsar.Exclusive,
	}

	consumer, err := client.Subscribe(consumerConfig)
	if err != nil {
		log.Fatal("can't create pulsar consumer: %v", zap.Error(err))
	}
	return consumer, client
}

// partitionSinks maintained for each partition, it may sync data for multiple tables.
type partitionSinks struct {
	tablesCommitTsMap sync.Map
	tableSinksMap     sync.Map
	// resolvedTs record the maximum timestamp of the received event
	resolvedTs uint64
}

// Consumer represents a Sarama consumer group consumer
type Consumer struct {
	ready chan bool

	ddlList              []*model.DDLEvent
	ddlListMu            sync.Mutex
	ddlWithMaxCommitTs   *model.DDLEvent
	ddlSink              ddlsink.Sink
	fakeTableIDGenerator *fakeTableIDGenerator

	// sinkFactory is used to create table sink for each table.
	sinkFactory *eventsinkfactory.SinkFactory
	sinks       []*partitionSinks
	sinksMu     sync.Mutex

	// initialize to 0 by default
	globalResolvedTs uint64

	eventRouter *dispatcher.EventRouter

	tz *time.Location

	codecConfig *common.Config

	option *consumerOption
}

// NewConsumer creates a new cdc pulsar consumer
// the consumer is responsible for consuming the data from the kafka topic
// and write the data to the downstream.
func NewConsumer(ctx context.Context, o *consumerOption) (*Consumer, error) {
	c := new(Consumer)
	c.option = o

	tz, err := util.GetTimezone(o.timezone)
	if err != nil {
		return nil, errors.Annotate(err, "can not load timezone")
	}
	config.GetGlobalServerConfig().TZ = o.timezone
	c.tz = tz

	c.fakeTableIDGenerator = &fakeTableIDGenerator{
		tableIDs: make(map[string]int64),
	}

	c.codecConfig = common.NewConfig(o.protocol)
	c.codecConfig.EnableTiDBExtension = o.enableTiDBExtension
	if c.codecConfig.Protocol == config.ProtocolAvro {
		c.codecConfig.AvroEnableWatermark = true
	}

	if o.replicaConfig != nil && o.replicaConfig.Sink != nil && o.replicaConfig.Sink.KafkaConfig != nil {
		c.codecConfig.LargeMessageHandle = o.replicaConfig.Sink.KafkaConfig.LargeMessageHandle
	}

	if o.replicaConfig != nil {
		eventRouter, err := dispatcher.NewEventRouter(o.replicaConfig, o.topic, "kafka")
		if err != nil {
			return nil, errors.Trace(err)
		}
		c.eventRouter = eventRouter
	}

	c.sinks = make([]*partitionSinks, o.partitionNum)
	ctx, cancel := context.WithCancel(ctx)
	errChan := make(chan error, 1)
	for i := 0; i < int(o.partitionNum); i++ {
		c.sinks[i] = &partitionSinks{}
	}

	changefeedID := model.DefaultChangeFeedID("pulsar-consumer")
	f, err := eventsinkfactory.New(ctx, changefeedID, o.downstreamURI, config.GetDefaultReplicaConfig(), errChan)
	if err != nil {
		cancel()
		return nil, errors.Trace(err)
	}
	c.sinkFactory = f

	go func() {
		err := <-errChan
		if errors.Cause(err) != context.Canceled {
			log.Error("error on running consumer", zap.Error(err))
		} else {
			log.Info("consumer exited")
		}
		cancel()
	}()

	ddlSink, err := ddlsinkfactory.New(ctx, changefeedID, o.downstreamURI, config.GetDefaultReplicaConfig())
	if err != nil {
		cancel()
		return nil, errors.Trace(err)
	}
	c.ddlSink = ddlSink
	c.ready = make(chan bool)
	return c, nil
}

type eventsGroup struct {
	events []*model.RowChangedEvent
}

func newEventsGroup() *eventsGroup {
	return &eventsGroup{
		events: make([]*model.RowChangedEvent, 0),
	}
}

func (g *eventsGroup) Append(e *model.RowChangedEvent) {
	g.events = append(g.events, e)
}

func (g *eventsGroup) Resolve(resolveTs uint64) []*model.RowChangedEvent {
	sort.Slice(g.events, func(i, j int) bool {
		return g.events[i].CommitTs < g.events[j].CommitTs
	})

	i := sort.Search(len(g.events), func(i int) bool {
		return g.events[i].CommitTs > resolveTs
	})
	result := g.events[:i]
	g.events = g.events[i:]

	return result
}

// ConsumeClaim must start a consumer loop of ConsumerGroupClaim's Messages().
func (c *Consumer) ConsumeMsg(msg pulsar.Message) error {
	c.sinksMu.Lock()
	sink := c.sinks[0]
	c.sinksMu.Unlock()
	if sink == nil {
		panic("sink should initialized")
	}

	ctx := context.Background()
	var (
		decoder codec.RowEventDecoder
		err     error
	)

	switch c.codecConfig.Protocol {
	case config.ProtocolCanalJSON:
		decoder, err = canal.NewBatchDecoder(ctx, c.codecConfig, nil)
		if err != nil {
			return err
		}
	default:
		log.Panic("Protocol not supported", zap.Any("Protocol", c.codecConfig.Protocol))
	}
	if err != nil {
		return errors.Trace(err)
	}

	log.Info("start consume claim",
		zap.String("topic", msg.Topic()),
		zap.Int64("initialOffset", claim.InitialOffset()), zap.Int64("highWaterMarkOffset", claim.HighWaterMarkOffset()))

	eventGroups := make(map[int64]*eventsGroup)
	for message := range claim.Messages() {
		if err := decoder.AddKeyValue(message.Key, message.Value); err != nil {
			log.Error("add key value to the decoder failed", zap.Error(err))
			return errors.Trace(err)
		}

		counter := 0
		for {
			tp, hasNext, err := decoder.HasNext()
			if err != nil {
				log.Panic("decode message key failed", zap.Error(err))
			}
			if !hasNext {
				break
			}

			counter++
			// If the message containing only one event exceeds the length limit, CDC will allow it and issue a warning.
			if len(message.Key)+len(message.Value) > c.option.maxMessageBytes && counter > 1 {
				log.Panic("kafka max-messages-bytes exceeded",
					zap.Int("max-message-bytes", c.option.maxMessageBytes),
					zap.Int("receivedBytes", len(message.Key)+len(message.Value)))
			}

			switch tp {
			case model.MessageTypeDDL:
				// for some protocol, DDL would be dispatched to all partitions,
				// Consider that DDL a, b, c received from partition-0, the latest DDL is c,
				// if we receive `a` from partition-1, which would be seemed as DDL regression,
				// then cause the consumer panic, but it was a duplicate one.
				// so we only handle DDL received from partition-0 should be enough.
				// but all DDL event messages should be consumed.
				ddl, err := decoder.NextDDLEvent()
				if err != nil {
					log.Panic("decode message value failed",
						zap.ByteString("value", message.Value),
						zap.Error(err))
				}
				if partition == 0 {
					c.appendDDL(ddl)
				}
				// todo: mark the offset after the DDL is fully synced to the downstream mysql.
				session.MarkMessage(message, "")
			case model.MessageTypeRow:
				row, err := decoder.NextRowChangedEvent()
				if err != nil {
					log.Panic("decode message value failed",
						zap.ByteString("value", message.Value),
						zap.Error(err))
				}

				if c.eventRouter != nil {
					target := c.eventRouter.GetPartitionForRowChange(row, c.option.partitionNum)
					if partition != target {
						log.Panic("RowChangedEvent dispatched to wrong partition",
							zap.Int32("obtained", partition),
							zap.Int32("expected", target),
							zap.Int32("partitionNum", c.option.partitionNum),
							zap.Any("row", row),
						)
					}
				}

				globalResolvedTs := atomic.LoadUint64(&c.globalResolvedTs)
				partitionResolvedTs := atomic.LoadUint64(&sink.resolvedTs)
				if row.CommitTs <= globalResolvedTs || row.CommitTs <= partitionResolvedTs {
					log.Warn("RowChangedEvent fallback row, ignore it",
						zap.Uint64("commitTs", row.CommitTs),
						zap.Uint64("globalResolvedTs", globalResolvedTs),
						zap.Uint64("partitionResolvedTs", partitionResolvedTs),
						zap.Int32("partition", partition),
						zap.Any("row", row))
					// todo: mark the offset after the DDL is fully synced to the downstream mysql.
					session.MarkMessage(message, "")
					continue
				}
				var partitionID int64
				if row.Table.IsPartition {
					partitionID = row.Table.TableID
				}
				tableID := c.fakeTableIDGenerator.
					generateFakeTableID(row.Table.Schema, row.Table.Table, partitionID)
				row.Table.TableID = tableID

				group, ok := eventGroups[tableID]
				if !ok {
					group = newEventsGroup()
					eventGroups[tableID] = group
				}

				group.Append(row)
				// todo: mark the offset after the DDL is fully synced to the downstream mysql.
				session.MarkMessage(message, "")
			case model.MessageTypeResolved:
				ts, err := decoder.NextResolvedEvent()
				if err != nil {
					log.Panic("decode message value failed",
						zap.ByteString("value", message.Value),
						zap.Error(err))
				}

				globalResolvedTs := atomic.LoadUint64(&c.globalResolvedTs)
				partitionResolvedTs := atomic.LoadUint64(&sink.resolvedTs)
				if ts < globalResolvedTs || ts < partitionResolvedTs {
					log.Warn("partition resolved ts fallback, skip it",
						zap.Uint64("ts", ts),
						zap.Uint64("partitionResolvedTs", partitionResolvedTs),
						zap.Uint64("globalResolvedTs", globalResolvedTs),
						zap.Int32("partition", partition))
					session.MarkMessage(message, "")
					continue
				}

				for tableID, group := range eventGroups {
					events := group.Resolve(ts)
					if len(events) == 0 {
						continue
					}
					if _, ok := sink.tableSinksMap.Load(tableID); !ok {
						sink.tableSinksMap.Store(tableID, c.sinkFactory.CreateTableSinkForConsumer(
							model.DefaultChangeFeedID("kafka-consumer"),
							spanz.TableIDToComparableSpan(tableID),
							events[0].CommitTs,
							prometheus.NewCounter(prometheus.CounterOpts{}),
						))
					}
					s, _ := sink.tableSinksMap.Load(tableID)
					s.(tablesink.TableSink).AppendRowChangedEvents(events...)
					commitTs := events[len(events)-1].CommitTs
					lastCommitTs, ok := sink.tablesCommitTsMap.Load(tableID)
					if !ok || lastCommitTs.(uint64) < commitTs {
						sink.tablesCommitTsMap.Store(tableID, commitTs)
					}
				}
				log.Debug("update partition resolved ts",
					zap.Uint64("ts", ts), zap.Int32("partition", partition))
				atomic.StoreUint64(&sink.resolvedTs, ts)
				// todo: mark the offset after the DDL is fully synced to the downstream mysql.
				session.MarkMessage(message, "")

			}

		}

		if counter > c.option.maxBatchSize {
			log.Panic("Open Protocol max-batch-size exceeded", zap.Int("max-batch-size", c.option.maxBatchSize),
				zap.Int("actual-batch-size", counter))
		}
	}

	return nil
}

// append DDL wait to be handled, only consider the constraint among DDLs.
// for DDL a / b received in the order, a.CommitTs < b.CommitTs should be true.
func (c *Consumer) appendDDL(ddl *model.DDLEvent) {
	c.ddlListMu.Lock()
	defer c.ddlListMu.Unlock()
	// DDL CommitTs fallback, just crash it to indicate the bug.
	if c.ddlWithMaxCommitTs != nil && ddl.CommitTs < c.ddlWithMaxCommitTs.CommitTs {
		log.Panic("DDL CommitTs < maxCommitTsDDL.CommitTs",
			zap.Uint64("commitTs", ddl.CommitTs),
			zap.Uint64("maxCommitTs", c.ddlWithMaxCommitTs.CommitTs),
			zap.Any("DDL", ddl))
	}

	// A rename tables DDL job contains multiple DDL events with same CommitTs.
	// So to tell if a DDL is redundant or not, we must check the equivalence of
	// the current DDL and the DDL with max CommitTs.
	if ddl == c.ddlWithMaxCommitTs {
		log.Info("ignore redundant DDL, the DDL is equal to ddlWithMaxCommitTs",
			zap.Any("DDL", ddl))
		return
	}

	c.ddlList = append(c.ddlList, ddl)
	log.Info("DDL event received", zap.Any("DDL", ddl))
	c.ddlWithMaxCommitTs = ddl
}

func (c *Consumer) getFrontDDL() *model.DDLEvent {
	c.ddlListMu.Lock()
	defer c.ddlListMu.Unlock()
	if len(c.ddlList) > 0 {
		return c.ddlList[0]
	}
	return nil
}

func (c *Consumer) popDDL() *model.DDLEvent {
	c.ddlListMu.Lock()
	defer c.ddlListMu.Unlock()
	if len(c.ddlList) > 0 {
		ddl := c.ddlList[0]
		c.ddlList = c.ddlList[1:]
		return ddl
	}
	return nil
}

func (c *Consumer) forEachSink(fn func(sink *partitionSinks) error) error {
	c.sinksMu.Lock()
	defer c.sinksMu.Unlock()
	for _, sink := range c.sinks {
		if err := fn(sink); err != nil {
			return errors.Trace(err)
		}
	}
	return nil
}

func (c *Consumer) getMinPartitionResolvedTs() (result uint64, err error) {
	result = uint64(math.MaxUint64)
	err = c.forEachSink(func(sink *partitionSinks) error {
		a := atomic.LoadUint64(&sink.resolvedTs)
		if a < result {
			result = a
		}
		return nil
	})
	return result, err
}

// Run the Consumer
func (c *Consumer) Run(ctx context.Context) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}

		minPartitionResolvedTs, err := c.getMinPartitionResolvedTs()
		if err != nil {
			return errors.Trace(err)
		}

		// handle DDL
		todoDDL := c.getFrontDDL()
		if todoDDL != nil && todoDDL.CommitTs <= minPartitionResolvedTs {
			// flush DMLs
			if err := c.forEachSink(func(sink *partitionSinks) error {
				return syncFlushRowChangedEvents(ctx, sink, todoDDL.CommitTs)
			}); err != nil {
				return errors.Trace(err)
			}

			// DDL can be executed, do it first.
			if err := c.ddlSink.WriteDDLEvent(ctx, todoDDL); err != nil {
				return errors.Trace(err)
			}
			c.popDDL()

			if todoDDL.CommitTs < minPartitionResolvedTs {
				log.Info("update minPartitionResolvedTs by DDL",
					zap.Uint64("minPartitionResolvedTs", minPartitionResolvedTs),
					zap.Any("DDL", todoDDL))
			}
			minPartitionResolvedTs = todoDDL.CommitTs
		}

		// update global resolved ts
		if c.globalResolvedTs > minPartitionResolvedTs {
			log.Panic("global ResolvedTs fallback",
				zap.Uint64("globalResolvedTs", c.globalResolvedTs),
				zap.Uint64("minPartitionResolvedTs", minPartitionResolvedTs))
		}

		if c.globalResolvedTs < minPartitionResolvedTs {
			c.globalResolvedTs = minPartitionResolvedTs
		}

		if err := c.forEachSink(func(sink *partitionSinks) error {
			return syncFlushRowChangedEvents(ctx, sink, c.globalResolvedTs)
		}); err != nil {
			return errors.Trace(err)
		}
	}
}

func syncFlushRowChangedEvents(ctx context.Context, sink *partitionSinks, resolvedTs uint64) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		flushedResolvedTs := true
		sink.tablesCommitTsMap.Range(func(key, value interface{}) bool {
			tableID := key.(int64)
			resolvedTs := model.NewResolvedTs(resolvedTs)
			tableSink, ok := sink.tableSinksMap.Load(tableID)
			if !ok {
				log.Panic("Table sink not found", zap.Int64("tableID", tableID))
			}
			if err := tableSink.(tablesink.TableSink).UpdateResolvedTs(resolvedTs); err != nil {
				log.Error("Failed to update resolved ts", zap.Error(err))
				return false
			}
			if !tableSink.(tablesink.TableSink).GetCheckpointTs().EqualOrGreater(resolvedTs) {
				flushedResolvedTs = false
			}
			return true
		})
		if flushedResolvedTs {
			return nil
		}
	}
}

type fakeTableIDGenerator struct {
	tableIDs       map[string]int64
	currentTableID int64
	mu             sync.Mutex
}

func (g *fakeTableIDGenerator) generateFakeTableID(schema, table string, partition int64) int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	key := quotes.QuoteSchema(schema, table)
	if partition != 0 {
		key = fmt.Sprintf("%s.`%d`", key, partition)
	}
	if tableID, ok := g.tableIDs[key]; ok {
		return tableID
	}
	g.currentTableID++
	g.tableIDs[key] = g.currentTableID
	return g.currentTableID
}

func openDB(ctx context.Context, dsn string) (*sql.DB, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		log.Error("open db failed", zap.String("dsn", dsn), zap.Error(err))
		return nil, errors.Trace(err)
	}

	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(10)
	db.SetConnMaxLifetime(10 * time.Minute)

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err = db.PingContext(ctx); err != nil {
		log.Error("ping db failed", zap.String("dsn", dsn), zap.Error(err))
		return nil, errors.Trace(err)
	}
	log.Info("open db success", zap.String("dsn", dsn))
	return db, nil
}