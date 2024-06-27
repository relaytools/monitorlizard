# MonitorLizard

This is a monitoring agent that can monitor websocket latency to nostr relays.

It can publish the results to InfluxDB and to nostr relays using NIP66

# Pre-built binaries 
Download the latest relase from the [releases page](https://github.com/relaytools/monitorlizard/releases)

Create an env file named .monitorlizard.env in the current directory.
Example contents of .monitorlizard.env:

```
# INFLUXDB IS OPTIONAL NO NEED TO SET THESE
INFLUXDB_URL=
INFLUXDB_TOKEN=
INFLUXDB_ORG=
INFLUXDB_BUCKET=
INFLUXDB_MEASUREMENT=
# Name of your monitor
MONITOR_NAME=mylizard

# Publish metrics to nostr relays
# Recommend generating a new nostr keypair for this monitor.
NOSTR_PUBLISH=true
NOSTR_PRIVATE_KEY=(hex)
# Where to publish events.
# This can be a comma separated list if you want to publish to multiple relays.
NOSTR_PUBLISH_RELAY_METRICS=wss://monitorlizard.nostr1.com
NOSTR_PUBLISH_MONITOR_PROFILE=true
MONITOR_COUNTRY_CODE=US
MONITOR_ABOUT=Relay Monitor new Lizard
# Frequency to run and publish checks.
MONITOR_FREQUENCY=60
```

Run it!
```
./monitorlizard wss://myrelay.example.com
```

# Building from source 
```
cp example.monitorlizard.env .monitorlizard.env
# Edit the settings to match your needs.
go build
./monitorlizard wss://myrelay.example.com
```
