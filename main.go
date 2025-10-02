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

	"github.com/mmcloughlin/geohash"
)

// normalizeURL normalizes a URL by converting it to lowercase and ensuring consistent format
func normalizeURL(rawURL string) (string, error) {
	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}

	// Convert scheme and host to lowercase
	parsedURL.Scheme = strings.ToLower(parsedURL.Scheme)
	parsedURL.Host = strings.ToLower(parsedURL.Host)

	// Remove default ports
	if (parsedURL.Scheme == "ws" && strings.HasSuffix(parsedURL.Host, ":80")) ||
		(parsedURL.Scheme == "wss" && strings.HasSuffix(parsedURL.Host, ":443")) {
		parsedURL.Host = parsedURL.Host[:strings.LastIndex(parsedURL.Host, ":")]
	}

	// Ensure path ends with / if it's empty
	if parsedURL.Path == "" {
		parsedURL.Path = "/"
	}

	return parsedURL.String(), nil
}

type MonitorConfig struct {
	InfluxUrl             string  `mapstructure:"INFLUXDB_URL"`
	InfluxToken           string  `mapstructure:"INFLUXDB_TOKEN"`
	InfluxOrg             string  `mapstructure:"INFLUXDB_ORG"`
	InfluxBucket          string  `mapstructure:"INFLUXDB_BUCKET"`
	InfluxMeasurement     string  `mapstructure:"INFLUXDB_MEASUREMENT"`
	MonitorName           string  `mapstructure:"MONITOR_NAME"`
	MonitorFrequency      int     `mapstructure:"MONITOR_FREQUENCY"`
	Publish               bool    `mapstructure:"NOSTR_PUBLISH"`
	PrivateKey            string  `mapstructure:"NOSTR_PRIVATE_KEY"`
	PublishRelayMetrics   string  `mapstructure:"NOSTR_PUBLISH_RELAY_METRICS"`
	PublishMonitorProfile bool    `mapstructure:"NOSTR_PUBLISH_MONITOR_PROFILE"`
	MonitorCountryCode    string  `mapstructure:"MONITOR_COUNTRY_CODE"`
	MonitorLatitude       float64 `mapstructure:"MONITOR_LATITUDE"`
	MonitorLongitude      float64 `mapstructure:"MONITOR_LONGITUDE"`
	MonitorAbout          string  `mapstructure:"MONITOR_ABOUT"`
	MonitorPicture        string  `mapstructure:"MONITOR_PICTURE"`

	RelayUrls      string  `mapstructure:"RELAY_URLS"`
	RelayLatitude  float64 `mapstructure:"RELAY_LATITUDE"`
	RelayLongitude float64 `mapstructure:"RELAY_LONGITUDE"`
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
		relay, err := nostr.RelayConnect(ctx, url)
		if err != nil {
			isError = true
			lastError = err
		}

		if err := relay.Publish(ctx, ev); err != nil {
			isError = true
			lastError = err
		}

		relay.Close()
	}
	if isError {
		return lastError
	}
	return nil
}

func main() {

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

	relayUrls := []string{}
	if iConfig.RelayUrls != "" && strings.Contains(iConfig.RelayUrls, ",") {
		relayUrls = strings.Split(iConfig.RelayUrls, ",")
	} else if iConfig.RelayUrls != "" {
		// single url
		relayUrls = []string{iConfig.RelayUrls}
	} else {
		// command line arg
		args := os.Args
		if len(args) < 2 {
			log.Printf("Usage: go run main.go URL, or specify RELAY_URLS in config")
		}
		rawUrl := args[1]

		_, err := url.Parse(rawUrl)
		if err != nil {
			log.Fatalf("Failed to parse URL: %v", err)
		}
		relayUrls = []string{rawUrl}
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
		// 0 - Monitor Profile
		newProfile := NostrProfile{
			Name:    iConfig.MonitorName,
			About:   iConfig.MonitorAbout,
			Picture: iConfig.MonitorPicture,
		}

		var err error
		newProfileJson, err := json.Marshal(newProfile)
		if err != nil {
			fmt.Println(err)
		}

		profileEv := nostr.Event{
			PubKey:    pub,
			CreatedAt: nostr.Timestamp(time.Now().Unix()),
			Kind:      0,
			Tags:      nostr.Tags{},
			Content:   string(newProfileJson),
		}

		profileEv.Sign(iConfig.PrivateKey)

		err = publishEv(profileEv, publishRelays)
		if err != nil {
			fmt.Printf("Error publishing kind 0: %s\n", err)
		} else {
			fmt.Printf("published monitor profile kind:0 to %v\n", publishRelays)
		}

		// 10002 - Monitor Relay List
		relayTags := nostr.Tags{}
		for _, t := range publishRelays {
			fmt.Println(t)
			relayTags = relayTags.AppendUnique(nostr.Tag{"r", t, "write"})
		}
		relayListEv := nostr.Event{
			PubKey:    pub,
			CreatedAt: nostr.Timestamp(time.Now().Unix()),
			Kind:      10002,
			Tags:      relayTags,
			Content:   "",
		}
		fmt.Println(publishRelays)
		fmt.Println(relayTags)
		relayListEv.Sign(iConfig.PrivateKey)
		err = publishEv(relayListEv, publishRelays)
		if err != nil {
			fmt.Printf("Error publishing kind 10002: %s\n", err)
		} else {
			fmt.Printf("published monitor relayList kind:10002 to %v\n", publishRelays)
		}

		// Publish to Nostr
		// 10166 - Monitor Profile
		profileTags := nostr.Tags{
			nostr.Tag{"url"},
			nostr.Tag{"frequency", useFrequencySecondsString},
			nostr.Tag{"o", pub},
			nostr.Tag{"k", "30066"},
			nostr.Tag{"c", "open"},
			nostr.Tag{"c", "read"},
			nostr.Tag{"timeout", "5000", "open"},
			nostr.Tag{"timeout", "15000", "read"},
			nostr.Tag{"timeout", "15000", "write"},
			nostr.Tag{"G", iConfig.MonitorCountryCode, "countryCode"},
		}

		// for every geo tag, encode all lesser precisions also
		monitorGeo := geohash.EncodeWithPrecision(iConfig.MonitorLatitude, iConfig.MonitorLongitude, 9)
		fmt.Println("monitor geohash was: ", monitorGeo)
		for i := 1; i < 9; i++ {
			profileTags = profileTags.AppendUnique(nostr.Tag{"g", monitorGeo[:i]})
		}

		ev := nostr.Event{
			PubKey:    pub,
			CreatedAt: nostr.Timestamp(time.Now().Unix()),
			Kind:      10166,
			Tags:      profileTags,
			Content:   "",
		}

		ev.Sign(iConfig.PrivateKey)
		err = publishEv(ev, publishRelays)
		if err != nil {
			fmt.Printf("Error publishing kind 10166: %s\n", err)
		} else {
			fmt.Printf("published monitor registration profile kind:10166 to %v\n", publishRelays)
		}
	}

	//FOR EACH RELAY
	for _, u := range relayUrls {
		// Normalize the URL for consistent d tag usage
		normalizedURL, err := normalizeURL(u)
		if err != nil {
			fmt.Printf("Error normalizing URL %s: %s\n", u, err)
			continue
		}

		// fetch NIP11 document
		theseTags := nostr.Tags{}
		nip11Info, err := nip11.Fetch(context.Background(), u)
		gotNip11 := true
		if err != nil {
			fmt.Printf("Error fetching NIP11 document: %s\n", err)
			gotNip11 = false
		}

		if gotNip11 {

			for _, t := range nip11Info.SupportedNIPs {
				// Convert interface{} to int (SupportedNIPs contains float64 values)
				if nipNum, ok := t.(float64); ok {
					theseTags = theseTags.AppendUnique(nostr.Tag{"N", fmt.Sprintf("%d", int(nipNum))})
				}
			}

			/*
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

				// general tags
				if len(nip11Info.Tags) > 0 {
					for _, t := range nip11Info.Tags {
						theseTags = theseTags.AppendUnique(nostr.Tag{"t", t})
					}
				}

				theseTags = theseTags.AppendUnique(nostr.Tag{"d", u})

				// Todo:

				//// don't need these but maybe
				// accepted kinds?
				// fees? probably don't need this
				// restricted writes? that's new..
				// language tags?
			*/

		}
		// stagger the requests for multiple relays (random sleep?)

		ticker := time.NewTicker(useFrequency)
		go func(relayURL string, normalizedRelayURL string, relayTags nostr.Tags) {
			parsedUrl, err := url.Parse(relayURL)
			if err != nil {
				fmt.Printf("fatal error: unable to parse url: %s, %s\n", relayURL, err)
				os.Exit(1)
			}
			for t := range ticker.C {
				msg := "[\"REQ\", \"1234abcdping\", {\"kinds\": [1], \"limit\": 1}]"
				whatTime := time.Now()
				result, _, err := wsstat.MeasureLatency(parsedUrl, msg, http.Header{})
				if err != nil {
					fmt.Println("ERROR OCCURRED: ", err)
					continue
				}

				fmt.Printf("Collecting data for %s at %s. total latency %dms\n", relayURL, t, result.TotalTime.Milliseconds())

				if influxEnabled {
					point := influxdb2.NewPoint(
						iConfig.InfluxMeasurement,
						map[string]string{
							"relay":   relayURL,
							"monitor": iConfig.MonitorName,
						},
						map[string]interface{}{
							"dnslookup":     result.DNSLookup.Milliseconds(),
							"tcpconnection": result.TCPConnection.Milliseconds(),
							"tlshandshake":  result.TLSHandshake.Milliseconds(),
							"wshandshake":   result.WSHandshake.Milliseconds(),
							"wsrtt":         result.MessageRoundTrip.Milliseconds(),
							"totaltime":     result.TotalTime.Milliseconds(),
						},
						whatTime,
					)
					// write asynchronously
					writeAPI.WritePoint(point)
				}

				openConnMs := result.DNSLookup.Milliseconds() + result.TCPConnection.Milliseconds() + result.TLSHandshake.Milliseconds() + result.WSHandshake.Milliseconds()
				openConnString := fmt.Sprintf("%d", openConnMs)
				openConnReadString := fmt.Sprintf("%d", result.MessageRoundTrip.Milliseconds())

				newTags := nostr.Tags{}
				for _, tag := range relayTags {
					newTags = newTags.AppendUnique(tag)
				}

				newTags = newTags.AppendUnique(nostr.Tag{"d", normalizedRelayURL})

				// for every geo tag, encode all lesser precisions also
				fullGeo := geohash.EncodeWithPrecision(iConfig.RelayLatitude, iConfig.RelayLongitude, 9)
				for i := 1; i < 9; i++ {
					newTags = newTags.AppendUnique(nostr.Tag{"g", fullGeo[:i]})
				}

				newTags = newTags.AppendUnique(nostr.Tag{"rtt-open", openConnString})
				newTags = newTags.AppendUnique(nostr.Tag{"rtt-read", openConnReadString})
				newTags = newTags.AppendUnique(nostr.Tag{"other", "network", "clearnet"})

				if iConfig.Publish {
					// Publish to Nostr stats/kind 30166
					ev := nostr.Event{
						PubKey:    pub,
						CreatedAt: nostr.Timestamp(whatTime.Unix()),
						Kind:      30166,
						Tags:      newTags,
						Content:   "",
					}
					ev.Sign(iConfig.PrivateKey)
					publishEv(ev, publishRelays)
				}
			}
		}(u, normalizedURL, theseTags)
	}
	select {}
}
