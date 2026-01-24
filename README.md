# RelayMonitor

A Nostr relay monitoring agent that measures latency and publishes results using [NIP-66](https://github.com/nostr-protocol/nips/blob/master/66.md).

## Checks

| Check | Description |
|-------|-------------|
| dns | DNS resolution time |
| info | NIP-11 relay info availability |
| open | Connection time (DNS + TCP + TLS + WebSocket) |
| read | Round-trip time for a request |
| write | Time to publish an event and receive OK |

## Quick Start

1. Download from [releases](https://github.com/Letdown2491/relaymonitor/releases) or build from source:
   ```
   go build
   ```

2. Create config file `.relaymonitor.env`:
   ```
   MONITOR_NAME=mymonitor
   NOSTR_PRIVATE_KEY=nsec1...
   NOSTR_PUBLISH=true
   NOSTR_PUBLISH_PROFILE_RELAYS=wss://purplepag.es,wss://relay.damus.io
   NOSTR_PUBLISH_METRICS_RELAYS=wss://relay.nostr.watch,wss://relay.damus.io
   RELAY_URLS=wss://relay1.example.com,wss://relay2.example.com
   MONITOR_LATITUDE=47.620564
   MONITOR_LONGITUDE=-122.350616
   RELAY_LATITUDE=43.000000
   RELAY_LONGITUDE=-75.000000
   ```

3. Run:
   ```
   ./relaymonitor
   ```

## Configuration

| Variable | Description |
|----------|-------------|
| `MONITOR_NAME` | Display name for your monitor |
| `NOSTR_PRIVATE_KEY` | Private key (nsec or hex format) |
| `NOSTR_PUBLISH` | Enable publishing to Nostr (true/false) |
| `NOSTR_PUBLISH_PROFILE_RELAYS` | Relays for profile events (Kind 0, 10002) |
| `NOSTR_PUBLISH_METRICS_RELAYS` | Relays for metrics events (Kind 10166, 30166) |
| `NOSTR_PUBLISH_MONITOR_PROFILE` | Publish monitor profile on startup (true/false) |
| `RELAY_URLS` | Comma-separated list of relays to monitor |
| `MONITOR_FREQUENCY` | Check interval in seconds (default: 300) |
| `MONITOR_LATITUDE` | Monitor location latitude |
| `MONITOR_LONGITUDE` | Monitor location longitude |
| `MONITOR_COUNTRY_CODE` | Monitor country code (e.g., US) |
| `MONITOR_ABOUT` | Monitor profile description |
| `RELAY_LATITUDE` | Relay location latitude |
| `RELAY_LONGITUDE` | Relay location longitude |
| `NIP11_REFRESH_INTERVAL` | NIP-11 refresh interval in seconds (default: 600) |

Optional InfluxDB support: `INFLUXDB_URL`, `INFLUXDB_TOKEN`, `INFLUXDB_ORG`, `INFLUXDB_BUCKET`, `INFLUXDB_MEASUREMENT`

## Systemd Service

```bash
sudo cp relaymonitor /usr/local/bin/
sudo cp .relaymonitor.env /usr/local/etc/
sudo cp relaymonitor.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now relaymonitor
```

View logs:
```bash
sudo journalctl -u relaymonitor -f
```
