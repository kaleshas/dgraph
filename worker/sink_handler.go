/*
 * Copyright 2021 Dgraph Labs, Inc. and Contributors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package worker

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"github.com/Shopify/sarama"

	"github.com/dgraph-io/dgraph/x"
)

type SinkMessage struct {
	Meta  SinkMeta
	Key   []byte
	Value []byte
}

type SinkMeta struct {
	Topic string
}

type Sink interface {
	// send in bulk to the sink
	Send(messages []SinkMessage) error
	// close sink
	Close() error
}

const (
	defaultSinkFileName = "sink.log"
)

func GetSink(conf *x.SuperFlag) (Sink, error) {
	switch {
	case conf.GetString("kafka") != "":
		return newKafkaSink(conf)
	case conf.GetString("file") != "":
		return newFileSink(conf)
	}
	return nil, errors.New("sink config is not provided")
}

// Kafka client is not concurrency safe.
// Its the responsibility of callee to manage the concurrency.
type kafkaSinkClient struct {
	client sarama.Client
	writer sarama.SyncProducer
}

func newKafkaSink(config *x.SuperFlag) (Sink, error) {
	if config.GetString("kafka") == "" {
		return nil, errors.New("brokers are not provided for the kafka config")
	}

	saramaConf := sarama.NewConfig()
	saramaConf.ClientID = "Dgraph"
	saramaConf.Producer.Partitioner = sarama.NewHashPartitioner
	saramaConf.Producer.Return.Successes = true
	saramaConf.Producer.Return.Errors = true

	if config.GetString("ca-cert") != "" {
		tlsCfg := &tls.Config{}
		var pool *x509.CertPool
		var err error
		if pool, err = x509.SystemCertPool(); err != nil {
			return nil, err
		}
		caFile, err := ioutil.ReadFile(config.GetString("ca-cert"))
		if err != nil {
			return nil, err
		}
		if !pool.AppendCertsFromPEM(caFile) {
			return nil, errors.New("not able to append certificates")
		}
		tlsCfg.RootCAs = pool
		cert := config.GetString("client-cert")
		key := config.GetString("client-key")
		if cert != "" && key != "" {
			cert, err := tls.LoadX509KeyPair(cert, key)
			if err != nil {
				return nil, err
			}
			tlsCfg.Certificates = []tls.Certificate{cert}
		}
		saramaConf.Net.TLS.Enable = true
		saramaConf.Net.TLS.Config = tlsCfg
	}

	if config.GetString("sasl-user") != "" && config.GetString("sasl-password") != "" {
		saramaConf.Net.SASL.Enable = true
		saramaConf.Net.SASL.User = config.GetString("sasl-user")
		saramaConf.Net.SASL.Password = config.GetString("sasl-password")
	}
	brokers := strings.Split(config.GetString("kafka"), ",")
	client, err := sarama.NewClient(brokers, saramaConf)
	if err != nil {
		return nil, err
	}
	producer, err := sarama.NewSyncProducerFromClient(client)
	if err != nil {
		return nil, err
	}
	return &kafkaSinkClient{
		client: client,
		writer: producer,
	}, nil
}

func (k *kafkaSinkClient) Send(messages []SinkMessage) error {
	if len(messages) == 0 {
		return nil
	}
	msgs := make([]*sarama.ProducerMessage, len(messages))
	for i, m := range messages {
		msgs[i] = &sarama.ProducerMessage{
			Topic: m.Meta.Topic,
			Key:   sarama.ByteEncoder(m.Key),
			Value: sarama.ByteEncoder(m.Value),
		}
	}
	return k.writer.SendMessages(msgs)
}

func (k *kafkaSinkClient) Close() error {
	_ = k.writer.Close()
	return k.client.Close()
}

// this is only for testing purposes. Ideally client wouldn't want file based sink
type fileSink struct {
	// log writer is buffered. Do take care of that while testing
	fileWriter *x.LogWriter
}

func (f *fileSink) Send(messages []SinkMessage) error {
	for _, m := range messages {
		_, err := f.fileWriter.Write([]byte(fmt.Sprintf("{ \"key\": \"%s\", \"value\": %s}\n",
			string(m.Key), string(m.Value))))
		if err != nil {
			return err
		}
	}
	return nil
}

func (f *fileSink) Close() error {
	return f.fileWriter.Close()
}

func newFileSink(path *x.SuperFlag) (Sink, error) {
	dir := path.GetString("file")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}

	fp, err := filepath.Abs(filepath.Join(dir, defaultSinkFileName))
	if err != nil {
		return nil, err
	}

	w := &x.LogWriter{
		FilePath: fp,
		MaxSize:  100,
		MaxAge:   10,
	}
	if w, err = w.Init(); err != nil {
		return nil, err
	}
	return &fileSink{
		fileWriter: w,
	}, nil
}
