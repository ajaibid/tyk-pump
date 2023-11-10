package pumps

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"encoding/base64"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/TykTechnologies/tyk-pump/analytics"
	"github.com/mitchellh/mapstructure"
	"github.com/segmentio/kafka-go"
	"github.com/segmentio/kafka-go/sasl"
	"github.com/segmentio/kafka-go/sasl/plain"
	"github.com/segmentio/kafka-go/sasl/scram"
	"github.com/sirupsen/logrus"

	"github.com/segmentio/kafka-go/snappy"
)

type KafkaPump struct {
	kafkaConf    *KafkaConf
	writerConfig kafka.WriterConfig
	log          *logrus.Entry
	CommonPumpConfig
}

type Json map[string]interface{}

var kafkaPrefix = "kafka-pump"
var kafkaDefaultENV = PUMPS_ENV_PREFIX + "_KAFKA" + PUMPS_ENV_META_PREFIX

// @PumpConf Kafka
type KafkaConf struct {
	EnvPrefix string `mapstructure:"meta_env_prefix"`
	// The list of brokers used to discover the partitions available on the kafka cluster. E.g.
	// "localhost:9092".
	Broker []string `json:"broker" mapstructure:"broker"`
	// Unique identifier for client connections established with Kafka.
	ClientId string `json:"client_id" mapstructure:"client_id"`
	// The topic that the writer will produce messages to.
	Topic string `json:"topic" mapstructure:"topic"`
	// Timeout is the maximum amount of seconds to wait for a connect or write to complete.
	Timeout interface{} `json:"timeout" mapstructure:"timeout"`
	// Enable "github.com/golang/snappy" codec to be used to compress Kafka messages. By default
	// is `false`.
	Compressed bool `json:"compressed" mapstructure:"compressed"`
	// Can be used to set custom metadata inside the kafka message.
	MetaData map[string]string `json:"meta_data" mapstructure:"meta_data"`
	// Enables SSL connection.
	UseSSL bool `json:"use_ssl" mapstructure:"use_ssl"`
	// Controls whether the pump client verifies the kafka server's certificate chain and host
	// name.
	SSLInsecureSkipVerify bool `json:"ssl_insecure_skip_verify" mapstructure:"ssl_insecure_skip_verify"`
	// Can be used to set custom certificate file for authentication with kafka.
	SSLCertFile string `json:"ssl_cert_file" mapstructure:"ssl_cert_file"`
	// Can be used to set custom key file for authentication with kafka.
	SSLKeyFile string `json:"ssl_key_file" mapstructure:"ssl_key_file"`
	// SASL mechanism configuration. Only "plain" and "scram" are supported.
	SASLMechanism string `json:"sasl_mechanism" mapstructure:"sasl_mechanism"`
	// SASL username.
	Username string `json:"sasl_username" mapstructure:"sasl_username"`
	// SASL password.
	Password string `json:"sasl_password" mapstructure:"sasl_password"`
	// SASL algorithm. It's the algorithm specified for scram mechanism. It could be sha-512 or sha-256.
	// Defaults to "sha-256".
	Algorithm string `json:"sasl_algorithm" mapstructure:"sasl_algorithm"`
}

func (k *KafkaPump) New() Pump {
	newPump := KafkaPump{}
	return &newPump
}

func (k *KafkaPump) GetName() string {
	return "Kafka Pump"
}

func (k *KafkaPump) GetEnvPrefix() string {
	return k.kafkaConf.EnvPrefix
}

func (k *KafkaPump) Init(config interface{}) error {
	k.log = log.WithField("prefix", kafkaPrefix)

	//Read configuration file
	k.kafkaConf = &KafkaConf{}
	err := mapstructure.Decode(config, &k.kafkaConf)
	if err != nil {
		k.log.Fatal("Failed to decode configuration: ", err)
	}

	processPumpEnvVars(k, k.log, k.kafkaConf, kafkaDefaultENV)
	// This interface field is not reached by envconfig library, that's why we manually check it
	if os.Getenv("TYK_PMP_PUMPS_KAFKA_META_TIMEOUT") != "" {
		k.kafkaConf.Timeout = os.Getenv("TYK_PMP_PUMPS_KAFKA_META_TIMEOUT")
	}

	var tlsConfig *tls.Config
	if k.kafkaConf.UseSSL {
		if k.kafkaConf.SSLCertFile != "" && k.kafkaConf.SSLKeyFile != "" {
			var cert tls.Certificate
			k.log.Debug("Loading certificates for mTLS.")
			cert, err = tls.LoadX509KeyPair(k.kafkaConf.SSLCertFile, k.kafkaConf.SSLKeyFile)
			if err != nil {
				k.log.Debug("Error loading mTLS certificates:", err)
				return err
			}
			tlsConfig = &tls.Config{
				Certificates:       []tls.Certificate{cert},
				InsecureSkipVerify: k.kafkaConf.SSLInsecureSkipVerify,
			}
		} else if k.kafkaConf.SSLCertFile != "" || k.kafkaConf.SSLKeyFile != "" {
			k.log.Error("Only one of ssl_cert_file and ssl_cert_key configuration option is setted, you should set both to enable mTLS.")
		} else {
			tlsConfig = &tls.Config{
				InsecureSkipVerify: k.kafkaConf.SSLInsecureSkipVerify,
			}
		}
	} else if k.kafkaConf.SASLMechanism != "" {
		k.log.WithField("SASL-Mechanism", k.kafkaConf.SASLMechanism).Warn("SASL-Mechanism is setted but use_ssl is false.")
	}

	var mechanism sasl.Mechanism

	switch k.kafkaConf.SASLMechanism {
	case "":
		break
	case "PLAIN", "plain":
		mechanism = plain.Mechanism{Username: k.kafkaConf.Username, Password: k.kafkaConf.Password}
	case "SCRAM", "scram":
		algorithm := scram.SHA256
		if k.kafkaConf.Algorithm == "sha-512" || k.kafkaConf.Algorithm == "SHA-512" {
			algorithm = scram.SHA512
		}
		var mechErr error
		mechanism, mechErr = scram.Mechanism(algorithm, k.kafkaConf.Username, k.kafkaConf.Password)
		if mechErr != nil {
			k.log.Fatal("Failed initialize kafka mechanism  : ", mechErr)
		}
	default:
		k.log.WithField("SASL-Mechanism", k.kafkaConf.SASLMechanism).Warn("Tyk pump doesn't support this SASL mechanism.")
	}

	// Timeout is an interface type to allow both time.Duration and float values
	var timeout time.Duration
	switch v := k.kafkaConf.Timeout.(type) {
	case string:
		timeout, err = time.ParseDuration(v) // i.e: when timeout is '1s'
		if err != nil {
			floatValue, floatErr := strconv.ParseFloat(v, 64) // i.e: when timeout is '1'
			if floatErr != nil {
				k.log.Fatal("Failed to parse timeout: ", floatErr)
			} else {
				timeout = time.Duration(floatValue * float64(time.Second))
			}
		}
	case float64:
		timeout = time.Duration(v) * time.Second // i.e: when timeout is 1
	}

	//Kafka writer connection config
	dialer := &kafka.Dialer{
		Timeout:       timeout,
		ClientID:      k.kafkaConf.ClientId,
		TLS:           tlsConfig,
		SASLMechanism: mechanism,
	}

	k.writerConfig.Brokers = k.kafkaConf.Broker
	k.writerConfig.Topic = k.kafkaConf.Topic
	k.writerConfig.Balancer = &kafka.LeastBytes{}
	k.writerConfig.Dialer = dialer
	k.writerConfig.WriteTimeout = timeout
	k.writerConfig.ReadTimeout = timeout
	if k.kafkaConf.Compressed {
		k.writerConfig.CompressionCodec = snappy.NewCompressionCodec()
	}

	k.log.Info(k.GetName() + " Initialized")

	return nil
}

func (k *KafkaPump) WriteData(ctx context.Context, data []interface{}) error {
	startTime := time.Now()
	k.log.Debug("Attempting to write ", len(data), " records...")
	kafkaMessages := make([]kafka.Message, len(data))
	for i, v := range data {
		//Build message format
		decoded := v.(analytics.AnalyticsRecord)
		message := Json{
			"timestamp":       decoded.TimeStamp,
			"method":          decoded.Method,
			"path":            decoded.Path,
			"raw_path":        decoded.RawPath,
			"response_code":   decoded.ResponseCode,
			"alias":           decoded.Alias,
			"api_key":         decoded.APIKey,
			"api_name":        decoded.APIName,
			"api_id":          decoded.APIID,
			"request_time_ms": decoded.RequestTime,
			"ip_address":      decoded.IPAddress,
			"host":            decoded.Host,
			"content_length":  decoded.ContentLength,
			"user_agent":      decoded.UserAgent,
		}
		// Filter only expose raw request response for certain status
		if val, ok := k.kafkaConf.MetaData["detailed_log_for_status"]; ok {
                if strings.Contains(val, strconv.Itoa(decoded.ResponseCode)) {
                    filteredRequestB, _ := base64.StdEncoding.DecodeString(decoded.RawRequest)
                    filteredRequest := string(filteredRequestB)
                    if hideHeader, ok2 := k.kafkaConf.MetaData["hide_request_header"]; ok2 {
                        hideHeaderArr := strings.Split(hideHeader, ",")

                        hideBody, _ := k.kafkaConf.MetaData["hide_request_body_key"]
                        hideBodyArr := strings.Split(hideBody, ",")

                        rawDecodedData, _ := decodeRawData(filteredRequest, hideHeaderArr, hideBodyArr, false)
                        filteredRequestByte, _ := json.Marshal(rawDecodedData)
                        filteredRequest = string(filteredRequestByte)
                    }

                    rawResponseDecodedB, _ := base64.StdEncoding.DecodeString(decoded.RawResponse)
                    rawResponseDecoded := string(rawResponseDecodedB)
                    message["raw_request"] = filteredRequest
                    message["raw_response"] = rawResponseDecoded
                }
            }

            if val, ok := k.kafkaConf.MetaData["include_tag"]; ok {
                prefixes := strings.Split(val, ",")
                for _, prefix := range prefixes {
                    for _, tagContent := range decoded.Tags {
                        if strings.HasPrefix(tagContent, prefix) {
                            message[prefix] = strings.TrimPrefix(tagContent, prefix)[1:]
                        }
                    }
                }
            }

		//Add static metadata to json
		for key, value := range k.kafkaConf.MetaData {
			message[key] = value
		}

		//Transform object to json string
		json, jsonError := json.Marshal(message)
		if jsonError != nil {
			k.log.WithError(jsonError).Error("unable to marshal message")
		}

		//Kafka message structure
		kafkaMessages[i] = kafka.Message{
			Time:  time.Now(),
			Value: json,
		}
	}
	//Send kafka message
	kafkaError := k.write(ctx, kafkaMessages)
	if kafkaError != nil {
		k.log.WithError(kafkaError).Error("unable to write message")
	}
	k.log.Info("ElapsedTime in ms for ", len(data), " records:", time.Since(startTime).Milliseconds())
	return nil
}

func (k *KafkaPump) write(ctx context.Context, messages []kafka.Message) error {
	kafkaWriter := kafka.NewWriter(k.writerConfig)
	defer kafkaWriter.Close()
	return kafkaWriter.WriteMessages(ctx, messages...)
}
