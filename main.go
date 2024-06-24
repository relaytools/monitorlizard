package main

import (
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"time"

	influxdb2 "github.com/influxdata/influxdb-client-go/v2"
	"github.com/influxdata/influxdb-client-go/v2/api"
	"github.com/jakobilobi/go-wsstat"
	"github.com/spf13/viper"
)

type influxdbConfig struct {
	Url         string `mapstructure:"INFLUXDB_URL"`
	Token       string `mapstructure:"INFLUXDB_TOKEN"`
	Org         string `mapstructure:"INFLUXDB_ORG"`
	Bucket      string `mapstructure:"INFLUXDB_BUCKET"`
	Measurement string `mapstructure:"INFLUXDB_MEASUREMENT"`
	MonitorName string `mapstructure:"MONITOR_NAME"`
}

func main() {
	args := os.Args
	if len(args) < 2 {
		log.Fatalf("Usage: go run main.go URL")
	}
    rawUrl := args[1]

	url, err := url.Parse(rawUrl)
	if err != nil {
		log.Fatalf("Failed to parse URL: %v", err)
	}
	// InfluxDB optional config loading
	viper.AddConfigPath("/usr/local/etc")
	viper.AddConfigPath("./")
	viper.SetConfigName(".monitorlizard.env")
	viper.SetConfigType("env")
	influxEnabled := true
	var iConfig *influxdbConfig
	if err := viper.ReadInConfig(); err != nil {
		fmt.Print("Warn: error reading influxdb config file /usr/local/etc/.monitorlizard.env\n", err)
		influxEnabled = false
	}
	// Viper unmarshals the loaded env variables into the struct
	if err := viper.Unmarshal(&iConfig); err != nil {
		fmt.Print("Warn: unable to decode influxdb config into struct\n", err)
		influxEnabled = false
	}

	fmt.Printf("Info: influxdb: %t\n", influxEnabled)

	var client influxdb2.Client
	var writeAPI api.WriteAPI

	if influxEnabled {
		// INFLUX INIT
		client = influxdb2.NewClientWithOptions(iConfig.Url, iConfig.Token,
			influxdb2.DefaultOptions().SetBatchSize(20))
		// Get non-blocking write client
		writeAPI = client.WriteAPI(iConfig.Org, iConfig.Bucket)
	}



	if influxEnabled {
		ticker := time.NewTicker(10 * time.Second)

		go func() {
			for t := range ticker.C {

				msg := "[\"REQ\", \"1234abcdping\", {\"kinds\": [1], \"limit\": 1}]"
				result, _, err := wsstat.MeasureLatency(url, msg, http.Header{})
				if err != nil {
					fmt.Println("ERROR OCCURRED: ", err)
				}

				fmt.Printf("Collecting data for %s at %s. total latency %dms\n", url, t, result.TotalTime.Milliseconds())

				point := influxdb2.NewPoint(
					iConfig.Measurement,
					map[string]string{
						"relay": url.Hostname(),
						"monitor": iConfig.MonitorName,
					},
					map[string]interface{}{
						"dnslookup": result.DNSLookup.Milliseconds(),
						"tcpconnection": result.TCPConnection.Milliseconds(),
						"tlshandshake": result.TLSHandshake.Milliseconds(),
						"wshandshake": result.WSHandshake.Milliseconds(),
						"wsrtt": result.MessageRoundTrip.Milliseconds(),
						"totaltime": result.TotalTime.Milliseconds(),
					},
					time.Now())
				// write asynchronously
				writeAPI.WritePoint(point)
			}
		}()
		select {}
	} else {
		msg := "[\"REQ\", \"1234abcdping\", {\"kinds\": [1], \"limit\": 1}]"
		result, p, _ := wsstat.MeasureLatency(url, msg, http.Header{})
		fmt.Printf("%+v\n", result)
		fmt.Printf("Response: %s\n\n", p)
	}
}