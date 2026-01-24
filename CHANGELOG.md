# Changelog

All notable changes to this project will be documented in this file.

## [1.0.0]

### Added
- **NIP-66 compliance** - Full relay monitoring spec implementation
- **DNS check** - Separate `rtt-dns` tag for DNS resolution timing
- **Info check** - `info` tag indicating NIP-11 availability
- **nsec support** - Private keys can be provided in nsec or hex format
- **Separate relay pools** - Profile relays (Kind 0, 10002) and metrics relays (Kind 10166, 30166)
- **Unified connection pool** - Shared connections for relays in both profile and metrics lists
- **Batch NIP-11 fetches** - Parallel fetching at startup
- **Systemd service file** - `relaymonitor.service` for easy deployment
- **Docker support** - Dockerfile and docker-compose.yaml
- **GitHub Actions** - Automated releases with goreleaser

### Improved
- **JSON performance** - Switched to goccy/go-json (2-3x faster marshaling)
- **HTTP client pooling** - Shared client with connection reuse for NIP-11 fetches
- **Event pooling** - sync.Pool for Event objects reduces allocations
- **Buffer pooling** - Reusable byte buffers for JSON marshaling
- **Pre-computed geohash tags** - Calculated once per relay, reused every cycle
- **Write latency on first run** - `rtt-write` now measured immediately, not just after N cycles

### Fixed
- **BIP-340 signatures** - Switched from dcrd (EC-Schnorr-DCRv0) to btcec (BIP-340)
- **Software tag** - Added `s` tag for relay software per NIP-66

### Dependencies
- github.com/goccy/go-json v0.10.5
- github.com/btcsuite/btcd/btcec/v2 v2.3.6
