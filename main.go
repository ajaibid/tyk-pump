package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"math/rand"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/TykTechnologies/tyk-pump/analytics"
	logger "github.com/TykTechnologies/tyk-pump/logger"
	"github.com/TykTechnologies/tyk-pump/pumps"
	"github.com/TykTechnologies/tyk-pump/serializer"
	"github.com/TykTechnologies/tyk-pump/server"
	"github.com/TykTechnologies/tyk-pump/storage"
	"github.com/gocraft/health"
	"github.com/sirupsen/logrus"
	kingpin "gopkg.in/alecthomas/kingpin.v2"
)

var SystemConfig TykPumpConfiguration
var AnalyticsStore storage.AnalyticsStorage
var UptimeStorage storage.AnalyticsStorage
var Pumps []pumps.Pump
var UptimePump pumps.UptimePump
var AnalyticsSerializers []serializer.AnalyticsSerializer

var log = logger.GetLogger()

var mainPrefix = "main"

var (
	help               = kingpin.CommandLine.HelpFlag.Short('h')
	conf               = kingpin.Flag("conf", "path to the config file").Short('c').Default("pump.conf").String()
	demoMode           = kingpin.Flag("demo", "pass orgID string to generate demo data").Default("").String()
	demoApiMode        = kingpin.Flag("demo-api", "pass apiID string to generate demo data").Default("").String()
	demoApiVersionMode = kingpin.Flag("demo-api-version", "pass apiID string to generate demo data").Default("").String()
	demoTrackPath      = kingpin.Flag("demo-track-path", "enable track path in analytics records").Default("false").Bool()
	demoDays           = kingpin.Flag("demo-days", "flag that determines the number of days for the analytics records").Default("30").Int()
	demoRecordsPerHour = kingpin.Flag("demo-records-per-hour", "flag that determines the number of records per hour for the analytics records").Default("0").Int()
	demoFutureData     = kingpin.Flag("demo-future-data", "flag that determines if the demo data should be in the future").Default("false").Bool()
	debugMode          = kingpin.Flag("debug", "enable debug mode").Bool()
	version            = kingpin.Version(pumps.VERSION)
)

func Init() {
	SystemConfig = TykPumpConfiguration{}

	kingpin.Parse()
	LoadConfig(conf, &SystemConfig)

	if SystemConfig.LogFormat == "json" {
		log.Formatter = &logrus.JSONFormatter{}
	}

	envDemo := os.Getenv("TYK_PMP_BUILDDEMODATA")
	if envDemo != "" {
		log.Warning("Demo mode active via environemnt variable")
		demoMode = &envDemo
	}

	//Serializer init
	AnalyticsSerializers = []serializer.AnalyticsSerializer{serializer.NewAnalyticsSerializer(serializer.MSGP_SERIALIZER), serializer.NewAnalyticsSerializer(serializer.PROTOBUF_SERIALIZER)}

	log.WithFields(logrus.Fields{
		"prefix": mainPrefix,
	}).Info("## Tyk Pump, ", pumps.VERSION, " ##")

	// If no environment variable is set, check the configuration file:
	if os.Getenv("TYK_LOGLEVEL") == "" {
		level := strings.ToLower(SystemConfig.LogLevel)
		switch level {
		case "", "info":
			// default, do nothing
		case "error":
			log.Level = logrus.ErrorLevel
		case "warn":
			log.Level = logrus.WarnLevel
		case "debug":
			log.Level = logrus.DebugLevel
		default:
			log.WithFields(logrus.Fields{
				"prefix": "main",
			}).Fatalf("Invalid log level %q specified in config, must be error, warn, debug or info. ", level)
		}
	}

	// If debug mode flag is set, override previous log level parameter:
	if *debugMode {
		log.Level = logrus.DebugLevel
	}

}

func randomDelay(minMs, maxMs int) {
	delayMs := minMs + rand.Intn(maxMs-minMs)
	log.Infof("Starting with delay of %d ms...", delayMs)

	randomDuration := time.Duration(delayMs) * time.Millisecond
	time.Sleep(randomDuration)
}

func setupAnalyticsStore() {
	switch SystemConfig.AnalyticsStorageType {
	case "redis":
		AnalyticsStore = &storage.RedisClusterStorageManager{}
		UptimeStorage = &storage.RedisClusterStorageManager{}
	default:
		AnalyticsStore = &storage.RedisClusterStorageManager{}
		UptimeStorage = &storage.RedisClusterStorageManager{}
	}

	AnalyticsStore.Init(SystemConfig.AnalyticsStorageConfig)

	// Copy across the redis configuration
	uptimeConf := SystemConfig.AnalyticsStorageConfig

	// Swap key prefixes for uptime purger
	uptimeConf.RedisKeyPrefix = "host-checker:"
	UptimeStorage.Init(uptimeConf)
}

func storeVersion() {
	var versionStore = &storage.RedisClusterStorageManager{}
	versionConf := SystemConfig.AnalyticsStorageConfig
	versionStore.KeyPrefix = "version-check-"
	versionStore.Config = versionConf
	versionStore.Connect()
	versionStore.SetKey("pump", pumps.VERSION, 0)
}

func initialisePumps() {
	Pumps = []pumps.Pump{}

	for key, pmp := range SystemConfig.Pumps {
		pumpTypeName := pmp.Type
		if pumpTypeName == "" {
			pumpTypeName = key
		}

		pmpType, err := pumps.GetPumpByName(pumpTypeName)
		if err != nil {
			log.WithFields(logrus.Fields{
				"prefix": mainPrefix,
			}).Error("Pump load error (skipping): ", err)
		} else {
			thisPmp := pmpType.New()
			thisPmp.SetFilters(pmp.Filters)
			thisPmp.SetTimeout(pmp.Timeout)
			thisPmp.SetOmitDetailedRecording(pmp.OmitDetailedRecording)
			thisPmp.SetMaxRecordSize(pmp.MaxRecordSize)
			thisPmp.SetIgnoreFields(pmp.IgnoreFields)
			thisPmp.SetDecodingRequest(pmp.DecodeRawRequest)
			thisPmp.SetDecodingResponse(pmp.DecodeRawResponse)
			initErr := thisPmp.Init(pmp.Meta)
			if initErr != nil {
				log.WithField("pump", thisPmp.GetName()).Error("Pump init error (skipping): ", initErr)
			} else {
				log.WithFields(logrus.Fields{
					"prefix": mainPrefix,
				}).Info("Init Pump: ", key)
				Pumps = append(Pumps, thisPmp)
			}
		}
	}

	if len(Pumps) == 0 {
		log.WithFields(logrus.Fields{
			"prefix": mainPrefix,
		}).Fatal("No pumps configured")
	}

	if !SystemConfig.DontPurgeUptimeData {
		initialiseUptimePump()
	}

}

func initialiseUptimePump() {
	log.WithFields(logrus.Fields{
		"prefix": mainPrefix,
	}).Info("'dont_purge_uptime_data' set to false, attempting to start Uptime pump! ")

	switch SystemConfig.UptimePumpConfig.UptimeType {
	case "sql":
		UptimePump = &pumps.SQLPump{IsUptime: true}
		UptimePump.Init(SystemConfig.UptimePumpConfig.SQLConf)

	default:
		UptimePump = &pumps.MongoPump{IsUptime: true}
		UptimePump.Init(SystemConfig.UptimePumpConfig.MongoConf)
	}

	log.WithFields(logrus.Fields{
		"prefix": mainPrefix,
		"type":   SystemConfig.UptimePumpConfig.Type,
	}).Info("Init Uptime Pump: ", UptimePump.GetName())
}

func StartPurgeLoop(wg *sync.WaitGroup, ctx context.Context, secInterval int, chunkSize int64, expire time.Duration, omitDetails bool) {

	analyticsItemsCh := make(chan analytics.AnalyticsItems)
	writeToPumps(analyticsItemsCh)

	for range time.Tick(time.Duration(secInterval) * time.Second) {

		job := instrument.NewJob("PumpRecordsPurge")
		startTime := time.Now()

		for i := -1; i < 10; i++ {
			var analyticsKeyName string
			if i == -1 {
				//if it's the first iteration, we look for tyk-system-analytics to maintain backwards compatibility or if analytics_config.enable_multiple_analytics_keys is disabled in the gateway
				analyticsKeyName = storage.ANALYTICS_KEYNAME
			} else {
				analyticsKeyName = fmt.Sprintf("%v_%v", storage.ANALYTICS_KEYNAME, i)
			}

			for _, serializerMethod := range AnalyticsSerializers {
				analyticsKeyName += serializerMethod.GetSuffix()
				AnalyticsValues := AnalyticsStore.GetAndDeleteSet(analyticsKeyName, chunkSize, expire)
				if len(AnalyticsValues) > 0 {
					analyticsItemsCh <- PreprocessAnalyticsValues(AnalyticsValues, serializerMethod, analyticsKeyName, omitDetails, job, startTime, secInterval)
				}
			}

		}

		job.Timing("purge_time_all", time.Since(startTime).Nanoseconds())

		if !SystemConfig.DontPurgeUptimeData {
			UptimeValues := UptimeStorage.GetAndDeleteSet(storage.UptimeAnalytics_KEYNAME, chunkSize, expire)
			UptimePump.WriteUptimeData(UptimeValues)
		}

		if checkShutdown(ctx, wg) {
			return
		}
	}
}

func PreprocessAnalyticsValues(AnalyticsValues []interface{}, serializerMethod serializer.AnalyticsSerializer, analyticsKeyName string, omitDetails bool, job *health.Job, startTime time.Time, secInterval int) analytics.AnalyticsItems {
	keys := make([]interface{}, len(AnalyticsValues))

	for i, v := range AnalyticsValues {
		decoded := analytics.AnalyticsRecord{}
		err := serializerMethod.Decode([]byte(v.(string)), &decoded)

		log.WithFields(logrus.Fields{
			"prefix": mainPrefix,
		}).Debug("Decoded Record: ", decoded)
		if err != nil {
			log.WithFields(logrus.Fields{
				"prefix":       mainPrefix,
				"analytic_key": analyticsKeyName,
			}).Error("Couldn't unmarshal analytics data:", err)
			continue
		}
		keys[i] = interface{}(decoded)
		job.Event("record")
	}

	return analytics.AnalyticsItems{
		Keys:        keys,
		Job:         job,
		StartTime:   startTime,
		SecInterval: secInterval,
	}
}

func checkShutdown(ctx context.Context, wg *sync.WaitGroup) bool {
	shutdown := false
	select {
	case <-ctx.Done():
		log.WithFields(logrus.Fields{
			"prefix": mainPrefix,
		}).Info("Shutting down ", len(Pumps), " pumps...")
		for _, pmp := range Pumps {
			if err := pmp.Shutdown(); err != nil {
				log.WithFields(logrus.Fields{
					"prefix": mainPrefix,
				}).Error("Error trying to gracefully shutdown  "+pmp.GetName()+":", err)
			} else {
				log.WithFields(logrus.Fields{
					"prefix": mainPrefix,
				}).Info(pmp.GetName() + " gracefully stopped.")
			}
		}
		wg.Done()
		shutdown = true
	default:
	}
	return shutdown
}

func writeToPumps(analyticsItemsCh chan analytics.AnalyticsItems) {
	// Send to pumps
	if Pumps == nil {
		log.WithFields(logrus.Fields{
			"prefix": mainPrefix,
		}).Warning("No pumps defined!")
		return
	}

	go func() {
		log.WithFields(logrus.Fields{
			"prefix": mainPrefix,
		}).Info("Establish channel execution for ", len(Pumps), " pumps")
		for item := range analyticsItemsCh {
			for _, pmp := range Pumps {
				execPumpWriting(pmp, &item.Keys, item.SecInterval, item.StartTime, item.Job)
			}
		}
	}()
}

func filterData(pump pumps.Pump, keys []interface{}) []interface{} {

	shouldTrim := SystemConfig.MaxRecordSize != 0 || pump.GetMaxRecordSize() != 0
	filters := pump.GetFilters()
	ignoreFields := pump.GetIgnoreFields()
	getDecodingResponse := pump.GetDecodedResponse()
	getDecodingRequest := pump.GetDecodedRequest()
	// Checking to see if all the config options are empty/false
	if !getDecodingRequest && !getDecodingResponse && !filters.HasFilter() && !pump.GetOmitDetailedRecording() && !shouldTrim && len(ignoreFields) == 0 {
		return keys
	}

	filteredKeys := make([]interface{}, len(keys))
	copy(filteredKeys, keys)

	newLenght := 0

	for _, key := range keys {
		decoded, ok := key.(analytics.AnalyticsRecord)
		if !ok {
			log.WithFields(logrus.Fields{
				"prefix": mainPrefix,
			}).Warningf("Pump  %s is unable to decode to analytics record: %+v", pump.GetName(), key)
			continue
		}
		if pump.GetOmitDetailedRecording() {
			decoded.RawRequest = ""
			decoded.RawResponse = ""
		} else {
			if shouldTrim {
				if pump.GetMaxRecordSize() != 0 {
					decoded.TrimRawData(pump.GetMaxRecordSize())
				} else {
					decoded.TrimRawData(SystemConfig.MaxRecordSize)
				}
			}
		}
		if filters.ShouldFilter(decoded) {
			continue
		}
		if len(ignoreFields) > 0 {
			decoded.RemoveIgnoredFields(ignoreFields)
		}
		// DECODING RAW REQUEST AND RESPONSE FROM BASE 64
		if getDecodingRequest {
			rawRequest, err := base64.StdEncoding.DecodeString(decoded.RawRequest)
			if err == nil {
				decoded.RawRequest = string(rawRequest)
			}
		}
		if getDecodingResponse {
			rawResponse, err := base64.StdEncoding.DecodeString(decoded.RawResponse)
			if err == nil {
				decoded.RawResponse = string(rawResponse)
			}
		}
		filteredKeys[newLenght] = decoded
		newLenght++
	}
	filteredKeys = filteredKeys[:newLenght]
	return filteredKeys
}

func execPumpWriting(pmp pumps.Pump, keys *[]interface{}, purgeDelay int, startTime time.Time, job *health.Job) {
	timer := time.AfterFunc(time.Duration(purgeDelay)*time.Second, func() {
		if pmp.GetTimeout() == 0 {
			log.WithFields(logrus.Fields{
				"prefix": mainPrefix,
			}).Warning("Pump  ", pmp.GetName(), " is taking more time than the value configured of purge_delay. You should try to set a timeout for this pump.")
		} else if pmp.GetTimeout() > purgeDelay {
			log.WithFields(logrus.Fields{
				"prefix": mainPrefix,
			}).Warning("Pump  ", pmp.GetName(), " is taking more time than the value configured of purge_delay. You should try lowering the timeout configured for this pump.")
		}
	})
	defer timer.Stop()

	log.WithFields(logrus.Fields{
		"prefix": mainPrefix,
	}).Debug("Writing to: ", pmp.GetName())

	ch := make(chan error, 1)
	//Load pump timeout
	timeout := pmp.GetTimeout()
	var ctx context.Context
	var cancel context.CancelFunc
	//Initialize context depending if the pump has a configured timeout
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
	} else {
		ctx, cancel = context.WithCancel(context.Background())
	}

	defer cancel()

	go func(ch chan error, ctx context.Context, pmp pumps.Pump, keys *[]interface{}) {
		filteredKeys := filterData(pmp, *keys)
		ch <- pmp.WriteData(ctx, filteredKeys)
	}(ch, ctx, pmp, keys)

	select {
	case err := <-ch:
		if err != nil {
			log.WithFields(logrus.Fields{
				"prefix": mainPrefix,
			}).Warning("Error Writing to: ", pmp.GetName(), " - Error:", err)
		}
	case <-ctx.Done():
		switch ctx.Err() {
		case context.Canceled:
			log.WithFields(logrus.Fields{
				"prefix": mainPrefix,
			}).Warning("The writing to ", pmp.GetName(), " have got canceled.")
		case context.DeadlineExceeded:
			log.WithFields(logrus.Fields{
				"prefix": mainPrefix,
			}).Warning("Timeout Writing to: ", pmp.GetName())
		}
	}
	if job != nil {
		job.Timing("purge_time_"+pmp.GetName(), time.Since(startTime).Nanoseconds())
	}
}

func main() {
	Init()
	SetupInstrumentation()

	randomDelay(500, 5000)

	go server.ServeHealthCheck(SystemConfig.HealthCheckEndpointName, SystemConfig.HealthCheckEndpointPort, SystemConfig.HTTPProfile)

	// Store version which will be read by dashboard and sent to
	// vclu(version check and licecnse utilisation) service
	storeVersion()

	// Create the store
	setupAnalyticsStore()

	// prime the pumps
	initialisePumps()

	if SystemConfig.PurgeChunk > 0 {
		log.WithField("PurgeChunk", SystemConfig.PurgeChunk).Info("PurgeChunk enabled")
		if SystemConfig.StorageExpirationTime == 0 {
			SystemConfig.StorageExpirationTime = 60
			log.WithField("StorageExpirationTime", 60).Warn("StorageExpirationTime not set, but PurgeChunk enabled, overriding to 60s")
		}
	}

	// start the worker loop
	log.WithFields(logrus.Fields{
		"prefix": mainPrefix,
	}).Infof("Starting purge loop @%d, chunk size %d", SystemConfig.PurgeDelay, SystemConfig.PurgeChunk)

	wg := sync.WaitGroup{}
	wg.Add(1)
	ctx, cancel := context.WithCancel(context.Background())
	go StartPurgeLoop(&wg, ctx, SystemConfig.PurgeDelay, SystemConfig.PurgeChunk, time.Duration(SystemConfig.StorageExpirationTime)*time.Second, SystemConfig.OmitDetailedRecording)

	go func() {
		for {
			logMemoryUsage()
			time.Sleep(1 * time.Minute)
		}
	}()

	termChan := make(chan os.Signal, 1)
	signal.Notify(termChan, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)
	<-termChan // Blocks here until either SIGINT or SIGTERM is received.
	cancel()   // cancel the context
	wg.Wait()  // wait till all the pumps finish
	log.WithFields(logrus.Fields{
		"prefix": mainPrefix,
	}).Info("Tyk-pump stopped.")
}

func logMemoryUsage() {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	log.Printf("Memory stats: Alloc = %vMiB, Sys = %vMiB, NumGC = %v", bToMb(m.Alloc), bToMb(m.Sys), m.NumGC)
}

func bToMb(b uint64) uint64 {
	return b / 1024 / 1024
}
