package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"time"

	influxdb2 "github.com/influxdata/influxdb-client-go/v2"
	"github.com/influxdata/influxdb-client-go/v2/api"
	"github.com/relaytools/go-wsstat"
	"github.com/spf13/viper"

	"github.com/nbd-wtf/go-nostr"
)

type MonitorConfig struct {
	InfluxUrl         string `mapstructure:"INFLUXDB_URL"`
	InfluxToken       string `mapstructure:"INFLUXDB_TOKEN"`
	InfluxOrg         string `mapstructure:"INFLUXDB_ORG"`
	InfluxBucket      string `mapstructure:"INFLUXDB_BUCKET"`
	InfluxMeasurement string `mapstructure:"INFLUXDB_MEASUREMENT"`
	MonitorName string `mapstructure:"MONITOR_NAME"`
	MonitorFrequency int `mapstructure:"MONITOR_FREQUENCY"`
	Publish bool `mapstructure:"NOSTR_PUBLISH"`
	PrivateKey string `mapstructure:"NOSTR_PRIVATE_KEY"`
	PublishRelayMetrics string `mapstructure:"NOSTR_PUBLISH_RELAY_METRICS"`
	PublishMonitorProfile bool `mapstructure:"NOSTR_PUBLISH_MONITOR_PROFILE"`
	MonitorCountryCode string `mapstructure:"MONITOR_COUNTRY_CODE"`
	MonitorCountryName string `mapstructure:"MONITOR_COUNTRY_NAME"`
	MonitorCityName string `mapstructure:"MONITOR_CITY_NAME"`
	MonitorAbout string `mapstructure:"MONITOR_ABOUT"`
	MonitorPicture string `mapstructure:"MONITOR_PICTURE"`
}

type NostrProfile struct {
	Name    string `json:"name"`
    About   string `json:"about"`
    Picture string `json:"picture"`
}

func publishEv(ev nostr.Event, urls []string) (err error) {
	isError := false
	var lastError error
	lastError = nil
	for _, url := range urls {
		relay, err := nostr.RelayConnect(context.Background(), url)
		if err != nil {
			isError = true
			lastError = err
		}

		if err := relay.Publish(context.Background(), ev); err != nil {
			isError = true
			lastError = err
		}
	}
	if isError {
		return lastError
	}

	return nil
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
	// Config loading
	viper.AddConfigPath("/usr/local/etc")
	viper.AddConfigPath("./")
	viper.SetConfigName(".monitorlizard.env")
	viper.SetConfigType("env")


	var iConfig *MonitorConfig
	if err := viper.ReadInConfig(); err != nil {
		fmt.Println("Warn: error reading monitorlizard config file from current directory -or- /usr/local/etc/.monitorlizard.env\n", err)
		os.Exit(1)
	}
	// Viper unmarshals the loaded env variables into the struct
	if err := viper.Unmarshal(&iConfig); err != nil {
		fmt.Print("Warn: unable to decode monitorlizard config into struct\n", err)
		os.Exit(1)
	}

	publishRelays := viper.GetStringSlice("NOSTR_PUBLISH_RELAY_METRICS")

	influxEnabled := true
	if iConfig.InfluxUrl == "" || iConfig.InfluxToken == "" || iConfig.InfluxOrg == "" || iConfig.InfluxBucket == "" || iConfig.InfluxMeasurement == "" {
		fmt.Println("Warn: InfluxDB configuration missing, disabling InfluxDB")
		influxEnabled = false
	}

	// Default to frequency 10 seconds
	useFrequency := time.Second * 10
	useFrequencySecondsString := "10"
	if iConfig.MonitorFrequency != 0 {
		useFrequency = time.Second * time.Duration(iConfig.MonitorFrequency)
		useFrequencySecondsString = fmt.Sprintf("%d", iConfig.MonitorFrequency)
		if err != nil {
			fmt.Printf("Error: unable to parse duration %s\n", iConfig.MonitorFrequency)
		}
	}

	pub, _ := nostr.GetPublicKey(iConfig.PrivateKey)

	fmt.Printf("Info: influxdb: %t\n", influxEnabled)

	var client influxdb2.Client
	var writeAPI api.WriteAPI

	if influxEnabled {
		// INFLUX INIT
		client = influxdb2.NewClientWithOptions(iConfig.InfluxUrl, iConfig.InfluxToken,
			influxdb2.DefaultOptions().SetBatchSize(20))
		// Get non-blocking write client
		writeAPI = client.WriteAPI(iConfig.InfluxOrg, iConfig.InfluxBucket)
	}

	if iConfig.PublishMonitorProfile {
		// Publish to Nostr
		// use go-nostr to publish 3 events
		// 10166 - Monitor Profile
		ev := nostr.Event {
			PubKey: pub,
			CreatedAt: nostr.Timestamp(time.Now().Unix()), 
			Kind: 10166,
			Tags: nostr.Tags{
				nostr.Tag{ "frequency", useFrequencySecondsString },
				nostr.Tag{ "o", pub },
				nostr.Tag{ "k", "30066" },
				nostr.Tag{ "c", "read" },
				nostr.Tag{ "timeout", "open", "5000" },
				nostr.Tag{ "timeout", "read", "15000" },
				nostr.Tag{ "timeout", "read", "15000" },
				nostr.Tag{ "g", iConfig.MonitorCountryCode, "countryCode" },
				nostr.Tag{ "g", iConfig.MonitorCountryName, "countryName" },
				nostr.Tag{ "g", iConfig.MonitorCityName, "cityName" },
			},
			Content: "",
		}
		ev.Sign(iConfig.PrivateKey)
		var err error
		err = publishEv(ev, publishRelays)
		if err != nil {
			fmt.Printf("Error publishing kind 10166: %s\n", err)
		} else {
			fmt.Printf("published monitor profile to %v\n", publishRelays)
		}

		// 10002 - Monitor Relay List
		relayListEv := nostr.Event {
			PubKey: pub,
			CreatedAt: nostr.Timestamp(time.Now().Unix()), 
			Kind: 10002,
			Tags: nostr.Tags{
				nostr.Tag{ "r", iConfig.PublishRelayMetrics, "write" },
			},
			Content: "",
		}
		relayListEv.Sign(iConfig.PrivateKey)
		err = publishEv(relayListEv, publishRelays)
		if err != nil {
			fmt.Printf("Error publishing kind 10002: %s\n", err)
		} else {
			fmt.Printf("published monitor relayList 10002 to %v\n", publishRelays)
		}

		// 0 - Monitor Profile
		newProfile := NostrProfile {
			Name: iConfig.MonitorName,
			About: iConfig.MonitorAbout,
			Picture: iConfig.MonitorPicture,
		}

		newProfileJson, err := json.Marshal(newProfile)
		if err != nil {
			fmt.Println(err)
		}

		profileEv := nostr.Event {
			PubKey: pub,
			CreatedAt: nostr.Timestamp(time.Now().Unix()),
			Kind: 0,
			Tags: nostr.Tags{},
			Content: string(newProfileJson),
		}

		profileEv.Sign(iConfig.PrivateKey)

		err = publishEv(profileEv, publishRelays)
		if err != nil {
			fmt.Printf("Error publishing kind 0: %s\n", err)
		} else {
			fmt.Printf("published monitor profile to %v\n", publishRelays)
		}
	}

	ticker := time.NewTicker(useFrequency)
	go func() {
		for t := range ticker.C {
			msg := "[\"REQ\", \"1234abcdping\", {\"kinds\": [1], \"limit\": 1}]"
			whatTime := time.Now()
			result, _, err := wsstat.MeasureLatency(url, msg, http.Header{})
			if err != nil {
				fmt.Println("ERROR OCCURRED: ", err)
			}

			fmt.Printf("Collecting data for %s at %s. total latency %dms\n", url, t, result.TotalTime.Milliseconds())


			if influxEnabled {
				point := influxdb2.NewPoint(
					iConfig.InfluxMeasurement,
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
					whatTime,
				)
				// write asynchronously
				writeAPI.WritePoint(point)
			}

			openConnMs := result.DNSLookup.Milliseconds() + result.TCPConnection.Milliseconds() + result.TLSHandshake.Milliseconds() + result.WSHandshake.Milliseconds()
			openConnString := fmt.Sprintf("%d", openConnMs)
			openConnReadString := fmt.Sprintf("%d", result.MessageRoundTrip.Milliseconds())
			if iConfig.Publish {
				// Publish to Nostr
				// use go-nostr to publish an event
				ev := nostr.Event {
					PubKey: pub,
					CreatedAt: nostr.Timestamp(whatTime.Unix()), 
					Kind: 30066,
					Tags: nostr.Tags{
						nostr.Tag{ "d", url.String() },
						nostr.Tag{ "other", "network", "clearnet" },
						nostr.Tag{"rtt", "open", openConnString },
						nostr.Tag{"rtt", "read", openConnReadString },
					},
					Content: "",
				}
				ev.Sign(iConfig.PrivateKey)
				publishEv(ev, publishRelays)
			}
		}
	}()
	select {}
}