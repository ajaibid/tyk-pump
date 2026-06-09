package pumps

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/url"
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
	kafkaWriter *kafka.Writer
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
	Key       string `json:"sasl_key" mapstructure:"sasl_key"`
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

	k.kafkaWriter = kafka.NewWriter(k.writerConfig)

	k.log.Info(k.GetName() + " Initialized")

	return nil
}

func (k *KafkaPump) WriteData(ctx context.Context, data []interface{}) error {
	startTime := time.Now()
	k.log.Debug("Attempting to write ", len(data), " records...")
	kafkaMessages := make([]kafka.Message, len(data))
	for i, v := range data {
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

		if val, ok := k.kafkaConf.MetaData["detailed_log_for_status"]; ok {
			if strings.Contains(val, strconv.Itoa(decoded.ResponseCode)) {
				filteredRequestB, err := base64.StdEncoding.DecodeString(decoded.RawRequest)
				if err != nil { // CHANGED: don't swallow — blank request otherwise
					k.log.WithError(err).Warn("failed to base64-decode RawRequest; skipping detail")
				} else {
					filteredRequest := string(filteredRequestB)

					if hideHeader, ok2 := k.kafkaConf.MetaData["hide_request_header"]; ok2 {
						hideHeaderArr := strings.Split(hideHeader, ",")
						hideBodyArr := strings.Split(k.kafkaConf.MetaData["hide_request_body_key"], ",")
						hashBodyArr := strings.Split(k.kafkaConf.MetaData["hash_request_body_key"], ",") // CHANGED: new field

						// CHANGED: pass hash list + pepper; fail closed on error
						rawDecodedData, derr := decodeRawLogData(filteredRequest, hideHeaderArr, hideBodyArr, hashBodyArr, []byte(k.kafkaConf.MetaData["hash_key"]))
						if derr != nil {
							k.log.WithError(derr).Warn("decodeRawLogData failed; dropping raw_request")
							filteredRequest = "" // never publish unredacted on parse failure
						} else {
							filteredRequestByte, _ := json.Marshal(rawDecodedData)
							filteredRequest = string(filteredRequestByte)
						}
					}

					rawResponseDecodedB, rerr := base64.StdEncoding.DecodeString(decoded.RawResponse)
					if rerr != nil { // CHANGED
						k.log.WithError(rerr).Warn("failed to base64-decode RawResponse")
					}
					message["raw_request"] = filteredRequest
					message["raw_response"] = string(rawResponseDecodedB)
				}
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

		for key, value := range k.kafkaConf.MetaData {
			message[key] = value
		}

		messageBytes, jsonError := json.Marshal(message) // CHANGED: was `json, ...` which shadowed the json package
		if jsonError != nil {
			k.log.WithError(jsonError).Error("unable to marshal message")
		}

		kafkaMessages[i] = kafka.Message{
			Time:  time.Now(),
			Value: messageBytes,
		}
	}

	if kafkaError := k.write(ctx, kafkaMessages); kafkaError != nil {
		k.log.WithError(kafkaError).Error("unable to write message")
	}
	k.log.Info("ElapsedTime in ms for ", len(data), " records:", time.Since(startTime).Milliseconds())
	return nil
}

type DecodedRequest struct {
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Proto   string            `json:"proto"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
}

func buildKeySet(arr []string, lower bool) map[string]struct{} {
	set := make(map[string]struct{}, len(arr))
	for _, k := range arr {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		if lower {
			k = strings.ToLower(k)
		}
		set[k] = struct{}{}
	}
	return set
}

// HMAC-SHA256 with secret pepper: deterministic (correlation works) but not
// brute-forceable without the key. Bare SHA256 of a password is reversible.
func hashSecret(value string, key []byte) string {
	if len(value) == 0 {
		return value
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(value))
	return "h:" + hex.EncodeToString(mac.Sum(nil))
}

func decodeRawLogData(raw string, hideHeaders, hideBody, hashBody []string, key []byte) (*DecodedRequest, error) {
	req, err := http.ReadRequest(bufio.NewReader(strings.NewReader(raw)))
	if err != nil {
		return nil, err
	}
	defer req.Body.Close()

	bodyBytes, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}

	hideHdr := buildKeySet(hideHeaders, true) // headers: case-insensitive
	hide := buildKeySet(hideBody, false)      // body keys: case-sensitive per spec
	hash := buildKeySet(hashBody, false)

	headers := make(map[string]string, len(req.Header))
	for name, vals := range req.Header {
		if _, ok := hideHdr[strings.ToLower(name)]; ok {
			headers[name] = "***"
			continue
		}
		headers[name] = strings.Join(vals, ", ")
	}

	body := processBody(bodyBytes, req.Header.Get("Content-Type"), hide, hash, key)

	return &DecodedRequest{
		Method:  req.Method,
		URL:     req.URL.String(),
		Proto:   req.Proto,
		Headers: headers,
		Body:    string(body),
	}, nil
}

func processBody(body []byte, contentType string, hide, hash map[string]struct{}, key []byte) []byte {
	if len(body) == 0 || (len(hide) == 0 && len(hash) == 0) {
		return body
	}
	mediaType, _, _ := mime.ParseMediaType(contentType) // strips "; charset=UTF-8"
	switch mediaType {
	case "application/x-www-form-urlencoded":
		return processForm(body, hide, hash, key)
	case "application/json":
		return processJSON(body, hide, hash, key)
	default:
		return body
	}
}

func processForm(body []byte, hide, hash map[string]struct{}, key []byte) []byte {
	pairs := strings.Split(string(body), "&")
	for i, pair := range pairs {
		eq := strings.IndexByte(pair, '=')
		if eq < 0 {
			continue
		}
		name, err := url.QueryUnescape(pair[:eq])
		if err != nil {
			continue
		}
		if _, ok := hide[name]; ok {
			pairs[i] = pair[:eq+1] + "%2A%2A%2A" // ***
			continue
		}
		if _, ok := hash[name]; ok {
			val, err := url.QueryUnescape(pair[eq+1:])
			if err != nil {
				continue
			}
			pairs[i] = pair[:eq+1] + url.QueryEscape(hashSecret(val, key))
		}
	}
	return []byte(strings.Join(pairs, "&"))
}

func processJSON(body []byte, hide, hash map[string]struct{}, key []byte) []byte {
	var parsed interface{}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return body // unparseable JSON: see note below
	}
	parsed = redactJSONValue(parsed, hide, hash, key)
	out, err := json.Marshal(parsed)
	if err != nil {
		return body
	}
	return out
}

// recursive so nested secrets (e.g. {"auth":{"password":"x"}}) are caught
func redactJSONValue(v interface{}, hide, hash map[string]struct{}, key []byte) interface{} {
	switch t := v.(type) {
	case map[string]interface{}:
		for k, val := range t {
			if _, ok := hide[k]; ok {
				t[k] = "***"
				continue
			}
			if _, ok := hash[k]; ok {
				if s, isStr := val.(string); isStr {
					t[k] = hashSecret(s, key)
				} else {
					t[k] = "***" // non-string secret: mask, don't hash
				}
				continue
			}
			t[k] = redactJSONValue(val, hide, hash, key)
		}
		return t
	case []interface{}:
		for i, item := range t {
			t[i] = redactJSONValue(item, hide, hash, key)
		}
		return t
	default:
		return v
	}
}

func (k *KafkaPump) write(ctx context.Context, messages []kafka.Message) error {
	return k.kafkaWriter.WriteMessages(ctx, messages...)
}
