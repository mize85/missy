package messaging

import (
	"context"
	"errors"
	"io"

	"github.com/microdevs/missy/log"
	"github.com/segmentio/kafka-go"
	"strconv"
)

// ReadMessageFunc is a message reading callback function, on error message will not be committed to underlying
type ReadMessageFunc func(msg Message) error

// Reader is used to read messages giving callback function
type Reader interface {
	Read(msgFunc ReadMessageFunc) error
	io.Closer
}

// BrokerReader interface used for underlying broker implementation
//go:generate mockgen -package=messaging -destination broker_reader_mock.go -source reader.go BrokerReader
type BrokerReader interface {
	FetchMessage(ctx context.Context) (Message, error)
	CommitMessages(ctx context.Context, msgs ...Message) error
	ReadMessage(ctx context.Context) (Message, error)
	io.Closer
}

// missyReader used as a default missy Reader implementation
type missyReader struct {
	brokers      []string
	groupID      string
	topic        string
	brokerReader BrokerReader
	readFunc     *ReadMessageFunc
	writer       Writer
	dlqWriter    Writer
	numOfRetries int
}

// readBroker us as a wrapper for kafka.Reader implementation to fulfill BrokerReader interface
type readBroker struct {
	*kafka.Reader
}

// FetchMessages used to fetch messages from the broker
func (rm *readBroker) FetchMessage(ctx context.Context) (Message, error) {
	m, err := rm.Reader.FetchMessage(ctx)

	if err != nil {
		return Message{}, err
	}

	return Message{Topic: m.Topic, Key: m.Key, Value: m.Value, Time: m.Time, Partition: m.Partition, Offset: m.Offset}, nil
}

// ReadMessage used to read and auto commit messages from the broker (currently not used in missy)
func (rm *readBroker) ReadMessage(ctx context.Context) (Message, error) {
	m, err := rm.Reader.ReadMessage(ctx)

	if err != nil {
		return Message{}, err
	}

	return Message{Topic: m.Topic, Key: m.Key, Value: m.Value, Time: m.Time, Partition: m.Partition, Offset: m.Offset}, nil
}

// CommitMessages used to commit red messages for the broker
func (rm *readBroker) CommitMessages(ctx context.Context, msgs ...Message) error {

	kafkaMessages := make([]kafka.Message, len(msgs))

	for i, m := range msgs {
		kafkaMsg := kafka.Message{Topic: m.Topic, Key: m.Key, Value: m.Value, Time: m.Time, Partition: m.Partition, Offset: m.Offset}
		kafkaMessages[i] = kafkaMsg
	}

	return rm.Reader.CommitMessages(ctx, kafkaMessages...)
}

// Close used to close underlying connection with broker
func (rm *readBroker) Close() error {
	return rm.Reader.Close()
}

// NewReader based on brokers hosts, consumerGroup and topic. You need to close it after use. (Close())
// we are leaving using the missy config for now, because we don't know how we want to configure this yet.
func NewReader(brokers []string, groupID string, topic string) Reader {

	kafkaReader := kafka.NewReader(kafka.ReaderConfig{
		Brokers:        brokers,
		GroupID:        groupID,
		Topic:          topic,
		CommitInterval: 0,    // 0 indicates that commits should be done synchronically
		MinBytes:       10e3, // 10KB do we want it from config?
		MaxBytes:       10e6, // 10MB do we want it from config?
	})

	numOfRetries, err := strconv.Atoi("number.of.retries")
	if err != nil {
		log.Debug("number.of.retries was not set, using default value of 5")
		numOfRetries = 5
	}

	return &missyReader{
		brokers:      brokers,
		groupID:      groupID,
		topic:        topic,
		brokerReader: &readBroker{kafkaReader},
		writer:       NewWriter(brokers, topic),//used to write message again in case of error
		dlqWriter:    NewWriter(brokers, topic+".dlq"),//used to write message to DLQ if all retries failed
		numOfRetries: numOfRetries,
	}
}

// Read start reading goroutine that calls msgFunc on new message, you need to close it after use
func (mr *missyReader) Read(msgFunc ReadMessageFunc) error {
	// we've got a read function on this reader, return error
	if mr.readFunc != nil {
		return errors.New("this reader is currently reading from underlying broker")
	}

	// set current read func
	mr.readFunc = &msgFunc

	// start reading goroutine
	go func() {
		for {
			ctx := context.Background()

			message, err := mr.brokerReader.FetchMessage(ctx)
			if err != nil {
				break
			}

			log.Infof("# messaging # new message: [topic] %v; [part] %v; [offset] %v; [retry] %v, %s = %s\n", message.Topic, message.Partition, message.Offset, message.RetryCounter, string(message.Key), string(message.Value))
			if err := msgFunc(message); err != nil {
				log.Errorf("# messaging # cannot commit a message: %v", err)
				retryCounter := message.RetryCounter
				if message.RetryCounter >= mr.numOfRetries {
					log.Error("Writing message to DLQ as all retries failed")
					mr.dlqWriter.Write(message.Key, message.Value)
				} else {
					log.Infof("# messaging # retry number: %s", retryCounter+1)
					mr.writer.WriteWithRetryCounter(message.Key, message.Value, retryCounter+1)
				}
				continue
			}

			// commit message if no error
			if err := mr.brokerReader.CommitMessages(ctx, message); err != nil {
				// should we do something else to just logging not committed message?
				log.Errorf("cannot commit message [%s] %v/%v: %s = %s; with error: %v", message.Topic, message.Partition, message.Offset, string(message.Key), string(message.Value), err)
			}
		}
	}()

	return nil
}

// Close used to close underlying connection with broker
func (mr *missyReader) Close() error {
	return mr.brokerReader.Close()
}
