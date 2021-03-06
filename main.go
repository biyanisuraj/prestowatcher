package main

import (
	"github.com/jessevdk/go-flags"
	"github.com/op/go-logging"
	"github.com/ashwanthkumar/slack-go-webhook"
	"os"
	"fmt"
	"time"
	"net/http"
	"strconv"
	"bytes"
	"encoding/json"
	"github.com/bluele/gcache"
	"strings"
	"github.com/armon/go-metrics/datadog"
	"github.com/armon/go-metrics"
)

/*
	This simple application's purpose in life is to ping Presto on an interval and check if any queries
	exceed a given partition limit and if so, ping a Slack channel to notify the users about the problem.
 */

const APP_NAME = "prestowatcher"
const APP_VERSION = "0.0.1"

var log = logging.MustGetLogger(APP_NAME)
var format = logging.MustStringFormatter(
	`%{color}%{level:-7s}: %{time} %{shortfile} %{longfunc} %{id:03x}%{color:reset} %{message}`,
)

var opts struct {
	Verbose bool `short:"v" long:"verbose" description:"Enable DEBUG logging"`
	DoVersion bool `short:"V" long:"version" description:"Print version and exit"`
	PrestoURL string `short:"u" long:"url" description:"presto URL (including scheme and port)" default:"" env:"PRESTO_URL"`
	PrestoConnector string `short:"c" long:"connector" description:"presto connector name for partitioned tables" default:"hive" env:"PRESTO_CONNECTOR"`
	MaxPartitions string `short:"m" long:"maxpart" description:"Alert when Presto queries scan more than X partitions" default:"30" env:"MAX_PARTITIONS"`
	UpdateInterval string `short:"i" long:"interval" description:"Update interval in seconds" default:"20" env:"UPDATE_INTERVAL"`
	SlackURL string `short:"s" long:"slack" description:"Slack Webhook URL" default:"" env:"SLACK_URL"`
	HealthHTTPPort string `short:"p" long:"port" description:"Health check HTTP server port" default:"8080" env:"PORT"`
	StatsdHost string `long:"statsd" description:"StatsD ( host:port )" default:"127.0.0.1" env:"STATSD_HOST"`

}

// This struct is used twice - once for the low-detail version on the overview page of all queries, and again in the full-detail version
// we simply parse the query again to get the additional detail we need.
type PrestoQuery struct {
	Query string `json:"query"`
	QueryID string `json:"queryId"`
	State string `json:"state"`
	Session struct {
		User string `json:"user"`
	} `json:"session"`
	Inputs []PrestoInput `json:"inputs"`
}
type PrestoInput struct {
	ConnectorID string `json:"connectorId"`
	Schema string `json:"schema"`
	Table string `json:"table"`
	ConnectorInfo ConnectorInfo `json:"connectorInfo"`
}
type ConnectorInfo struct {
	PartitionIds []string `json:"partitionIds"`
	Truncated bool `json:"truncated"`
}

type ModeQueryInfo struct {
	User string `json:"user"`
	URL string `json:"url"`
	Scheduled bool `json:"scheduled"`
}

// Metrics sink
var metricsSink *datadog.DogStatsdSink
// Internal stat to track last time we polled Presto
var lastUpdate int64
// Converted version of the UpdateInterval
var delay time.Duration
// Maximum partitions
var maxParts int
// We need to store the queries we've seen before so we don't spam Slack. Maybe that'd be a good thing?
var queryCache gcache.Cache

func healthCheckHandler(resp http.ResponseWriter, request *http.Request) {
	if time.Now().Unix() - lastUpdate > 3*int64(delay) {
		resp.WriteHeader(500)
	}
	resp.Write(
		[]byte(fmt.Sprintf("Hi Mom!\nPolled last: [%v]", time.Now().Unix() - lastUpdate)),
	)
	log.Debug("Received health check")
}

func pingSlack(badInputs []PrestoInput, query PrestoQuery) {
	var attachments []slack.Attachment

	var totalPartitions int
	for _, i := range badInputs {
		ptnCount := len(i.ConnectorInfo.PartitionIds)
		totalPartitions += ptnCount
		attachment := slack.Attachment{}
		var color = "warning"
		attachment.Color = &color
		attachment.AddField(slack.Field{Title: "Schema", Value: fmt.Sprintf("%v.%v.%v", i.ConnectorID, i.Schema, i.Table), Short: true})
		attachment.AddField(slack.Field{Title: "Partitions", Value: fmt.Sprintf("%v", ptnCount), Short: true})
		attachments = append(attachments, attachment)
	}

	if query.Session.User == "mode" {
		var mqi ModeQueryInfo
		var color = "439FE0"
		lines := strings.Split(query.Query, "\n")
		modeTag := lines[len(lines)-1][3:]
		json.Unmarshal([]byte(modeTag), &mqi)
		queryInfo := slack.Attachment{}
		queryInfo.Color = &color
		queryInfo.AddField(slack.Field{Title: "Mode Username", Value: mqi.User, Short: true})
		queryInfo.AddField(slack.Field{Title: "Scheduled?", Value: fmt.Sprintf("%v", mqi.Scheduled), Short: true})
		queryInfo.AddField(slack.Field{Title: "URL", Value: mqi.URL})
		attachments = append(attachments, queryInfo)
	}

	payload := slack.Payload {
		Text: fmt.Sprintf(":bomb: :bomb: :bomb:\nPresto query <%v/ui/query.html?%v> is searching through more than *%v* partitions total! :sql_bandit:\n", opts.PrestoURL, query.QueryID, totalPartitions) +
			"Make sure your query has a filter for `date` and not `received_at`!\n" +
			"\n\n*If you want to disable this alert for your query*, add `-- sqlbandit:off` somewhere in your query.",
		Username: "SQLBandit",
		Attachments: attachments,
	}
	err := slack.Send(opts.SlackURL, "", payload)
	if len(err) > 0 {
		log.Errorf("Error sending message to Slack: %s\n", err)
	}
}

func checkQuery(queryStats PrestoQuery) error {
	// How many partitions does this query have?
	log.Debugf("Checking query [%v] for issues...", queryStats.QueryID)
	queryWrap, err := getQuery(queryStats.QueryID)
	if err != nil {
		return err
	}
	// Yeah, silly i know, but whatever.
	query := queryWrap[0]

	// Let us disable the slack alert per-query
	if strings.Contains(query.Query, "sqlbandit:off") {
		return nil
	}

	shouldPingSlack := false

	var badInputs []PrestoInput

	//log.Debugf("Query: %+v", query)
	for idx, input := range query.Inputs {
		log.Debugf("Checking query [%q] input index [%v] partition counts...", queryStats.QueryID, idx)
		if input.ConnectorID != opts.PrestoConnector {
			// not a hive query... bail, bail, bail!
			log.Debugf("Query [%q] input index [%v] connector [%v] != [%v], aborting check of this input index!", queryStats.QueryID, idx, input.ConnectorID, opts.PrestoConnector)
			return nil
		}
		log.Debugf("Partitions: %v", input.ConnectorInfo.PartitionIds)

		// emit partition names to datadog
		for _, ptn := range input.ConnectorInfo.PartitionIds {
			log.Debugf("Emit StatsD message for table: [%v.%v.%v] Partition: [%v]", input.ConnectorID, input.Schema, input.Table, ptn)
			metricsSink.IncrCounterWithLabels(
				[]string{"presto", "watcher", "queried_partitions",},
				1.0,
				[]metrics.Label{
					{
						Name: "table",
						Value: fmt.Sprintf("%s.%s.%s", input.ConnectorID, input.Schema, input.Table),
					},
					{
						Name: "partition",
						Value: ptn,
					},
				},
			)
		}

		if len(input.ConnectorInfo.PartitionIds) > maxParts {
			shouldPingSlack = true
			badInputs = append(badInputs, input)
			log.Warningf("Query [%v] Input [%v] Source [%v.%v.%v] is searching [%v] partitions!", queryStats.QueryID, idx, input.ConnectorID, input.Schema, input.Table, len(input.ConnectorInfo.PartitionIds))
			metricsSink.IncrCounterWithLabels(
				[]string{"presto", "watcher", "query_partition_counts"},
				float32(len(input.ConnectorInfo.PartitionIds)),
				[]metrics.Label{
					{
						Name: "table",
						Value: fmt.Sprintf("%s.%s.%s", input.ConnectorID, input.Schema, input.Table),
					},
				},
			)
		}
	}

	if shouldPingSlack {
		pingSlack(badInputs, query)
	}
	return nil
}

func getQuery(queryId string) ([]PrestoQuery, error) {
	var req *http.Request
	if queryId == "" {
		// Get all running query IDs
		req, _ = http.NewRequest("GET", fmt.Sprintf("%v/v1/query?state=running", opts.PrestoURL), nil)
	} else {
		// Get all specific query IDs
		req, _ = http.NewRequest("GET", fmt.Sprintf("%v/v1/query/%v", opts.PrestoURL, queryId), nil)
	}
	client := &http.Client{}
	resp, err := client.Do(req)

	// Was there an error with the collection?
	if err !=nil || resp.Body==nil {
		log.Errorf("Error with request to Presto server for query overview: %+v", err)
		return nil, err
	}

	buf := new(bytes.Buffer)
	buf.ReadFrom(resp.Body)

	if queryId == "" {
		var queries []PrestoQuery
		json.Unmarshal(buf.Bytes(), &queries)
		log.Debug("Received overview data from Presto!")
		return queries, nil
	} else {
		var query PrestoQuery
		json.Unmarshal(buf.Bytes(), &query)
		log.Debug("Received query data from Presto!")
		return []PrestoQuery{query}, nil
	}

}

func doCollect() bool {

	// Get all queries
	queries, err := getQuery("")
	if err != nil {
		log.Errorf("Got error while collecting queries. We'll retry again in [%v] seconds", opts.UpdateInterval)
		return false
	}

	for _, query := range queries {
		if query.State == "RUNNING" {
			log.Debugf("Found RUNNING query with id: [%+v]", query.QueryID)
			t, err := queryCache.GetIFPresent(query.QueryID)
			if err == gcache.KeyNotFoundError {
				log.Debugf("Query with id: [%v] not found in cache! [%v]", query.QueryID, err)
				// This is a new query we haven't seen before - check it!

				if e := checkQuery(query); e != nil {
					log.Errorf("Received error checking query [%v]. Error was [%v]", query.QueryID, e)
					return false
				}
				queryCache.Set(query.QueryID, time.Now())
			} else {
				log.Debugf("Query with id: [%v] was found in cache. Was cached at [%v], ignoring. [%v]", query.QueryID, t, err)
			}

		}
	}

	return true
}

func startCollector() {
	var e error
	metricsSink, e = datadog.NewDogStatsdSink(opts.StatsdHost, "")
	if e != nil || metricsSink==nil {
		log.Fatalf("Unable to start statsd sink. Addr: [%v], Error: [%v]", opts.StatsdHost, e.Error())
		os.Exit(-1)
	}

	ticker := time.NewTicker(delay * time.Second)
	quit := make(chan struct{})

	lastUpdate = time.Now().Unix()

	go func() {
		log.Debug("Starting collector thread")
		// initial run
		if doCollect() {
			lastUpdate = time.Now().Unix()
		}
		for {
			select {
			case <- ticker.C:
				// do work on timer tick
				log.Debug("Timer Tick!")
				if doCollect() {
					lastUpdate = time.Now().Unix()
				}

				// quit signal
			case <- quit:
				ticker.Stop()
				log.Info("Received stop signal. Exiting")
				break
			}
		}
	}()
}

func main() {
	// Parse arguments
	_, err := flags.Parse(&opts)
	// From https://www.snip2code.com/Snippet/605806/go-flags-suggested--h-documentation
	if err != nil {
		typ := err.(*flags.Error).Type
		if typ == flags.ErrHelp {
			os.Exit(0)
		} else {
			fmt.Println(err)
			os.Exit(1)
		}
	}

	// Print version number if requested from command line
	if opts.DoVersion == true {
		fmt.Printf("%s %s at your service.\n", APP_NAME, APP_VERSION)
		os.Exit(10)
	}

	// Configure logger
	log_backend := logging.NewLogBackend(os.Stderr, "", 0)
	backend_formatter := logging.NewBackendFormatter(log_backend, format)
	logging.SetBackend(backend_formatter)

	// Enable debug logging
	if opts.Verbose == true {
		logging.SetLevel(logging.DEBUG, "")
	} else {
		logging.SetLevel(logging.INFO, "")
	}

	log.Debugf("Commandline options: %+v", opts)

	// can we continue?
	if opts.PrestoURL == "" || opts.SlackURL == "" {
		log.Fatal("Missing options. Try again!")
	}

	// instanciate our cache
	queryCache = gcache.New(100).
		LFU().
		Expiration(time.Hour).
		EvictedFunc(func(key, value interface{}) {
			log.Debugf("Evicted query [%+v] from cache", key)
		}).
		Build()

	// Convert interval string from ENV / opts to integer
	if interval, err := strconv.Atoi(opts.UpdateInterval) ; err == nil {
		delay = time.Duration(interval)
		log.Debugf("Update interval: %v seconds", interval)
	} else {
		log.Fatalf("Unable to convert Update Interval '%s' to integer. Error was: %s", opts.UpdateInterval, err)
	}

	// Convert health check port string from ENV / opts to integer
	port, err := strconv.Atoi(opts.HealthHTTPPort) ;
	if err != nil {
		log.Fatalf("Unable to convert Health Check HTTP Port '%s' to integer. Error was: %s", opts.HealthHTTPPort, err)
	}

	// Convert max partitions string from ENV / opts to integer
	if maxPartsTmp, err := strconv.Atoi(opts.MaxPartitions) ; err == nil {
		maxParts = maxPartsTmp
	} else {
		log.Fatalf("Unable to convert max partitions '%s' to integer. Error was: %s", opts.MaxPartitions, err)
	}

	hostname, _ := os.Hostname()
	log.Infof("Starting %s version: %s on host %s", APP_NAME, APP_VERSION, hostname)

	//START COLLECTOR HERE!
	startCollector()

	// Start the health check handler
	http.HandleFunc("/", healthCheckHandler)
	http.ListenAndServe(fmt.Sprintf(":%d", port), nil)

	log.Info("Running, collecting queries from Presto!.")

}

