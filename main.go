package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	influxdb2 "github.com/influxdata/influxdb-client-go/v2"
	"github.com/influxdata/influxdb-client-go/v2/api"
	"github.com/relaytools/go-wsstat"
	"github.com/spf13/viper"

	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip11"
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
	ctx := context.Background()
	for _, url := range urls {
		fmt.Println("publishing to -> ", url)
		relay, err := nostr.RelayConnect(ctx, url)
		if err != nil {
			isError = true
			lastError = err
		}

		if err := relay.Publish(ctx, ev); err != nil {
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

	// if a comma is detected in the iConfig.PublishRelayMetrics, split it into a slice
	publishRelays := []string{iConfig.PublishRelayMetrics}
	if iConfig.PublishRelayMetrics != "" && strings.Contains(iConfig.PublishRelayMetrics, ",") {
		publishRelays = strings.Split(iConfig.PublishRelayMetrics, ",")
	}

	fmt.Printf("Publishing to %d relays: %v\n", len(publishRelays), publishRelays)

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

	// fetch NIP11 document
	nip11Info, err := nip11.Fetch(context.Background(), rawUrl)
	gotNip11 := true
	if err != nil {
		fmt.Printf("Error fetching NIP11 document: %s\n", err)
		gotNip11 = false
	}

	if gotNip11 {
		//fmt.Printf("NIP11 document: %s\n", nip11Info)
		//os.Exit(0)
		theseTags := nostr.Tags{}

		for _, t := range nip11Info.SupportedNIPs {
			theseTags = theseTags.AppendUnique(nostr.Tag{"N", fmt.Sprintf("%d", t)})
		}

		if nip11Info.Limitation.PaymentRequired {
			theseTags = theseTags.AppendUnique(nostr.Tag{"R", "payment"})
		} else {
			theseTags = theseTags.AppendUnique(nostr.Tag{"R", "!payment"})
		}

		if nip11Info.Limitation.AuthRequired {
			theseTags = theseTags.AppendUnique(nostr.Tag{"R", "auth"})
		} else {
			theseTags = theseTags.AppendUnique(nostr.Tag{"R", "!auth"})
		}

		// relay_countries (it's in nip11, could be used for geotags)
		if len(nip11Info.RelayCountries) > 0 {
			for _, c := range nip11Info.RelayCountries {
				theseTags = theseTags.AppendUnique(nostr.Tag{"G", c})
			}
		}

		// Todo:

		// stuff in the spec nip11:
		// restricted writes? that's new..
		// accepted kinds?
		// language tags?
		// general tags? what are these, they're new

		// stuff not yet in the spec nip11:
		// fees?
		// other stuff we might want, description?

		fmt.Printf("tags were %v\n", theseTags)

		nip11Ev := nostr.Event {
			PubKey: pub,
			CreatedAt: nostr.Timestamp(time.Now().Unix()), 
			Kind: 30166,
			Tags: theseTags,
		}

		nip11Ev.Sign(iConfig.PrivateKey)
		err := publishEv(nip11Ev, publishRelays)
		if err != nil {
			fmt.Printf("Error publishing kind 30166: %s\n", err)
		} else {
			fmt.Printf("published relay registration for %s kind:30166 to %v\n", rawUrl, publishRelays)
		}
	}

	if iConfig.PublishMonitorProfile {
		// Publish to Nostr
		// 10166 - Monitor Profile
		ev := nostr.Event {
			PubKey: pub,
			CreatedAt: nostr.Timestamp(time.Now().Unix()), 
			Kind: 10166,
			Tags: nostr.Tags{
				nostr.Tag{ "frequency", useFrequencySecondsString },
				nostr.Tag{ "o", pub },
				nostr.Tag{ "k", "30066" },
				nostr.Tag{ "c", "open" },
				nostr.Tag{ "c", "read" },
				nostr.Tag{ "timeout", "open", "5000" },
				nostr.Tag{ "timeout", "read", "15000" },
				nostr.Tag{ "timeout", "write", "15000" },
				nostr.Tag{ "G", iConfig.MonitorCountryCode, "countryCode" },
				//nostr.Tag{ "G", iConfig.MonitorCountryName, "countryName" },
				//nostr.Tag{ "G", iConfig.MonitorCityName, "cityName" },
			},
			Content: "",
		}
		ev.Sign(iConfig.PrivateKey)
		var err error
		err = publishEv(ev, publishRelays)
		if err != nil {
			fmt.Printf("Error publishing kind 10166: %s\n", err)
		} else {
			fmt.Printf("published monitor registration profile kind:10166 to %v\n", publishRelays)
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
			fmt.Printf("published monitor relayList kind:10002 to %v\n", publishRelays)
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
			fmt.Printf("published monitor profile kind:0 to %v\n", publishRelays)
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
				// Publish to Nostr stats kind:30066
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
