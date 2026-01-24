package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/coder/websocket"
	json "github.com/goccy/go-json"
	influxdb2 "github.com/influxdata/influxdb-client-go/v2"
	"github.com/influxdata/influxdb-client-go/v2/api"
	"github.com/mmcloughlin/geohash"
	"github.com/relaytools/go-wsstat"
	"github.com/spf13/viper"
)

// ============================================================================
// Minimal Nostr Implementation
// ============================================================================

// Timestamp is a Unix timestamp in seconds
type Timestamp int64

// Tag is a single tag (array of strings)
type Tag []string

// Tags is a collection of tags
type Tags []Tag

// AppendUnique appends a tag only if it doesn't already exist
func (tags Tags) AppendUnique(tag Tag) Tags {
	for _, t := range tags {
		if len(t) > 0 && len(tag) > 0 && t[0] == tag[0] {
			// For single-letter tags, check if values match
			if len(t) > 1 && len(tag) > 1 && t[1] == tag[1] {
				return tags
			}
		}
	}
	return append(tags, tag)
}

// Event represents a Nostr event
type Event struct {
	ID        string    `json:"id"`
	PubKey    string    `json:"pubkey"`
	CreatedAt Timestamp `json:"created_at"`
	Kind      int       `json:"kind"`
	Tags      Tags      `json:"tags"`
	Content   string    `json:"content"`
	Sig       string    `json:"sig"`
}

// eventPool is a sync.Pool for reusing Event objects
var eventPool = sync.Pool{
	New: func() interface{} {
		return &Event{
			Tags: make(Tags, 0, 32), // Pre-allocate typical tag capacity
		}
	},
}

// bufferPool is a sync.Pool for reusing byte buffers in JSON marshaling
var bufferPool = sync.Pool{
	New: func() interface{} {
		// Pre-allocate 4KB buffer (typical event size)
		buf := make([]byte, 0, 4096)
		return &buf
	},
}

// GetEvent gets an Event from the pool
func GetEvent() *Event {
	return eventPool.Get().(*Event)
}

// PutEvent returns an Event to the pool after resetting it
func PutEvent(e *Event) {
	e.ID = ""
	e.PubKey = ""
	e.CreatedAt = 0
	e.Kind = 0
	e.Tags = e.Tags[:0] // Keep capacity, reset length
	e.Content = ""
	e.Sig = ""
	eventPool.Put(e)
}

// getBuffer gets a buffer from the pool
func getBuffer() *[]byte {
	return bufferPool.Get().(*[]byte)
}

// putBuffer returns a buffer to the pool after resetting it
func putBuffer(buf *[]byte) {
	*buf = (*buf)[:0]
	bufferPool.Put(buf)
}

// Shared HTTP client with connection pooling for NIP-11 fetches
var httpClient = &http.Client{
	Timeout: 10 * time.Second,
	Transport: &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
	},
}

// Serialize returns the canonical JSON serialization for signing
func (e *Event) Serialize() []byte {
	// NIP-01: [0, pubkey, created_at, kind, tags, content]
	arr := []interface{}{
		0,
		e.PubKey,
		int64(e.CreatedAt),
		e.Kind,
		e.Tags,
		e.Content,
	}
	data, _ := json.Marshal(arr)
	return data
}

// GetID calculates and returns the event ID
func (e *Event) GetID() string {
	hash := sha256.Sum256(e.Serialize())
	return hex.EncodeToString(hash[:])
}

// Sign signs the event with the given private key (hex encoded)
func (e *Event) Sign(privateKeyHex string) error {
	privKeyBytes, err := hex.DecodeString(privateKeyHex)
	if err != nil {
		return fmt.Errorf("invalid private key hex: %w", err)
	}

	privKey, pubKey := btcec.PrivKeyFromBytes(privKeyBytes)

	// Verify the event's pubkey matches the signing key
	expectedPubKey := hex.EncodeToString(schnorr.SerializePubKey(pubKey))
	if e.PubKey != expectedPubKey {
		return fmt.Errorf("event pubkey %s does not match signing key %s", e.PubKey, expectedPubKey)
	}

	// Calculate event ID (hash of serialized event)
	serialized := e.Serialize()
	hash := sha256.Sum256(serialized)
	e.ID = hex.EncodeToString(hash[:])

	// Sign the hash directly
	sig, err := schnorr.Sign(privKey, hash[:])
	if err != nil {
		return fmt.Errorf("signing failed: %w", err)
	}

	e.Sig = hex.EncodeToString(sig.Serialize())
	return nil
}

// ============================================================================
// Bech32 / NIP-19 Support
// ============================================================================

// bech32 charset
const bech32Charset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"

// bech32CharsetRev is the reverse lookup table for bech32 characters
var bech32CharsetRev = func() [128]int8 {
	var rev [128]int8
	for i := range rev {
		rev[i] = -1
	}
	for i, c := range bech32Charset {
		rev[c] = int8(i)
	}
	return rev
}()

// bech32Decode decodes a bech32 string into its human-readable part and data bytes
func bech32Decode(bech string) (string, []byte, error) {
	// Find the separator (last '1' in the string)
	sepIdx := strings.LastIndex(bech, "1")
	if sepIdx < 1 || sepIdx+7 > len(bech) || len(bech) > 90 {
		return "", nil, fmt.Errorf("invalid bech32 string")
	}

	hrp := strings.ToLower(bech[:sepIdx])
	dataStr := strings.ToLower(bech[sepIdx+1:])

	// Decode data characters to 5-bit values
	data5bit := make([]byte, len(dataStr))
	for i, c := range dataStr {
		if c > 127 || bech32CharsetRev[c] == -1 {
			return "", nil, fmt.Errorf("invalid bech32 character: %c", c)
		}
		data5bit[i] = byte(bech32CharsetRev[c])
	}

	// Remove checksum (last 6 characters)
	if len(data5bit) < 6 {
		return "", nil, fmt.Errorf("bech32 string too short")
	}
	data5bit = data5bit[:len(data5bit)-6]

	// Convert 5-bit values to 8-bit bytes
	data8bit, err := convertBits(data5bit, 5, 8, false)
	if err != nil {
		return "", nil, err
	}

	return hrp, data8bit, nil
}

// convertBits converts a byte slice from one bit-per-element to another
func convertBits(data []byte, fromBits, toBits int, pad bool) ([]byte, error) {
	acc := 0
	bits := 0
	maxv := (1 << toBits) - 1
	var result []byte

	for _, value := range data {
		if int(value) >= (1 << fromBits) {
			return nil, fmt.Errorf("invalid data range")
		}
		acc = (acc << fromBits) | int(value)
		bits += fromBits
		for bits >= toBits {
			bits -= toBits
			result = append(result, byte((acc>>bits)&maxv))
		}
	}

	if pad {
		if bits > 0 {
			result = append(result, byte((acc<<(toBits-bits))&maxv))
		}
	} else if bits >= fromBits || ((acc<<(toBits-bits))&maxv) != 0 {
		// Ignore padding bits if they're zero
		if bits > 0 && ((acc<<(toBits-bits))&maxv) != 0 {
			return nil, fmt.Errorf("invalid padding")
		}
	}

	return result, nil
}

// DecodeNsec decodes an nsec bech32 string to hex private key
func DecodeNsec(nsec string) (string, error) {
	hrp, data, err := bech32Decode(nsec)
	if err != nil {
		return "", fmt.Errorf("bech32 decode failed: %w", err)
	}
	if hrp != "nsec" {
		return "", fmt.Errorf("invalid prefix: expected 'nsec', got '%s'", hrp)
	}
	if len(data) != 32 {
		return "", fmt.Errorf("invalid nsec length: expected 32 bytes, got %d", len(data))
	}
	return hex.EncodeToString(data), nil
}

// NormalizePrivateKey converts an nsec to hex, or returns hex as-is
func NormalizePrivateKey(key string) (string, error) {
	key = strings.TrimSpace(key)
	if strings.HasPrefix(key, "nsec1") {
		return DecodeNsec(key)
	}
	// Assume it's already hex, validate it
	if len(key) != 64 {
		return "", fmt.Errorf("invalid private key length: expected 64 hex chars, got %d", len(key))
	}
	if _, err := hex.DecodeString(key); err != nil {
		return "", fmt.Errorf("invalid hex private key: %w", err)
	}
	return key, nil
}

// GetPublicKey derives the public key from a private key (both hex encoded)
func GetPublicKey(privateKeyHex string) (string, error) {
	privKeyBytes, err := hex.DecodeString(privateKeyHex)
	if err != nil {
		return "", fmt.Errorf("invalid private key hex: %w", err)
	}

	_, pubKey := btcec.PrivKeyFromBytes(privKeyBytes)

	// Return x-only public key (32 bytes) as hex using BIP-340 serialization
	return hex.EncodeToString(schnorr.SerializePubKey(pubKey)), nil
}

// Relay represents a connection to a Nostr relay
type Relay struct {
	URL  string
	conn *websocket.Conn
	ctx  context.Context
}

// RelayConnect establishes a WebSocket connection to a relay
func RelayConnect(ctx context.Context, relayURL string) (*Relay, error) {
	conn, _, err := websocket.Dial(ctx, relayURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to relay %s: %w", relayURL, err)
	}

	return &Relay{
		URL:  relayURL,
		conn: conn,
		ctx:  ctx,
	}, nil
}

// Publish sends an event to the relay
func (r *Relay) Publish(ctx context.Context, ev Event) error {
	// NIP-01: ["EVENT", <event JSON>]
	// Use buffer pool to reduce allocations
	buf := getBuffer()
	defer putBuffer(buf)

	// Build message manually to reuse buffer
	*buf = append(*buf, `["EVENT",`...)
	evJSON, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("failed to marshal event: %w", err)
	}
	*buf = append(*buf, evJSON...)
	*buf = append(*buf, ']')

	err = r.conn.Write(ctx, websocket.MessageText, *buf)
	if err != nil {
		return fmt.Errorf("failed to send event: %w", err)
	}

	// Read OK response from relay
	// NIP-01: ["OK", <event_id>, <true|false>, <message>]
	_, respData, err := r.conn.Read(ctx)
	if err != nil {
		return fmt.Errorf("failed to read OK response: %w", err)
	}

	var response []interface{}
	if err := json.Unmarshal(respData, &response); err != nil {
		return fmt.Errorf("failed to parse OK response: %w", err)
	}

	if len(response) < 3 {
		return fmt.Errorf("invalid OK response: %v", response)
	}

	msgType, ok := response[0].(string)
	if !ok || msgType != "OK" {
		return fmt.Errorf("unexpected response type: %v", response[0])
	}

	accepted, ok := response[2].(bool)
	if !ok {
		return fmt.Errorf("invalid OK accepted field: %v", response[2])
	}

	if !accepted {
		reason := ""
		if len(response) > 3 {
			reason, _ = response[3].(string)
		}
		return fmt.Errorf("event rejected: %s", reason)
	}

	return nil
}

// Ping sends a ping to check if connection is alive
func (r *Relay) Ping(ctx context.Context) error {
	return r.conn.Ping(ctx)
}

// Close closes the relay connection
func (r *Relay) Close() error {
	if r.conn != nil {
		return r.conn.Close(websocket.StatusNormalClosure, "closing")
	}
	return nil
}

// MeasureWriteLatency measures the time to publish an event and receive OK
func MeasureWriteLatency(ctx context.Context, relayURL string, privateKey string, pubKey string) (time.Duration, error) {
	// Connect to relay
	conn, _, err := websocket.Dial(ctx, relayURL, nil)
	if err != nil {
		return 0, fmt.Errorf("connect failed: %w", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "done")

	// Create a test event (kind 1 ephemeral-ish, will be rejected by most relays but we measure the response time)
	ev := Event{
		PubKey:    pubKey,
		CreatedAt: Timestamp(time.Now().Unix()),
		Kind:      1,
		Tags:      Tags{},
		Content:   "relaymonitor latency test",
	}

	if err := ev.Sign(privateKey); err != nil {
		return 0, fmt.Errorf("sign failed: %w", err)
	}

	// Marshal the EVENT message using buffer pool
	buf := getBuffer()
	defer putBuffer(buf)

	*buf = append(*buf, `["EVENT",`...)
	evJSON, err := json.Marshal(ev)
	if err != nil {
		return 0, fmt.Errorf("marshal failed: %w", err)
	}
	*buf = append(*buf, evJSON...)
	*buf = append(*buf, ']')

	// Start timing
	start := time.Now()

	// Send event
	if err := conn.Write(ctx, websocket.MessageText, *buf); err != nil {
		return 0, fmt.Errorf("write failed: %w", err)
	}

	// Wait for OK response
	// NIP-01: ["OK", <event_id>, <true|false>, <message>]
	_, response, err := conn.Read(ctx)
	if err != nil {
		return 0, fmt.Errorf("read failed: %w", err)
	}

	// Stop timing
	elapsed := time.Since(start)

	// Parse response to verify it's an OK for our event
	var resp []interface{}
	if err := json.Unmarshal(response, &resp); err == nil {
		if len(resp) >= 2 && resp[0] == "OK" {
			// Valid OK response
			return elapsed, nil
		}
	}

	// Even if response isn't OK, we got a response
	return elapsed, nil
}

// ============================================================================
// End Minimal Nostr Implementation
// ============================================================================

// ============================================================================
// Performance-Optimized Relay Monitor
// ============================================================================

// RelayMonitor holds pre-computed and cached data for monitoring a single relay
type RelayMonitor struct {
	// Immutable after creation (no locks needed)
	URL           string
	NormalizedURL string
	ParsedURL     *url.URL
	NetworkType   string
	GeoTags       Tags // Pre-computed geohash tags

	// Mutable state (protected by mutex)
	mu            sync.RWMutex
	nip11Tags     Tags
	nip11Raw      string
	nip11Valid    bool  // Whether NIP-11 was successfully fetched
	writeLatency  int64 // Cached write latency (ms)
	writeCycle    int   // Cycle counter for write latency sampling

	// Pre-allocated buffers for the monitoring message
	pingMsg []byte
}

// NewRelayMonitor creates a new relay monitor with pre-computed values
func NewRelayMonitor(relayURL string, relayLat, relayLon float64) (*RelayMonitor, error) {
	normalizedURL, err := normalizeURL(relayURL)
	if err != nil {
		return nil, fmt.Errorf("normalize URL: %w", err)
	}

	parsedURL, err := url.Parse(relayURL)
	if err != nil {
		return nil, fmt.Errorf("parse URL: %w", err)
	}

	// Pre-compute geohash tags (they never change)
	geoTags := make(Tags, 0, 9)
	fullGeo := geohash.EncodeWithPrecision(relayLat, relayLon, 9)
	for i := 1; i <= 9; i++ {
		geoTags = append(geoTags, Tag{"g", fullGeo[:i]})
	}

	// Pre-allocate the ping message (reused every cycle)
	pingMsg := []byte(`["REQ", "1234abcdping", {"kinds": [1], "limit": 1}]`)

	return &RelayMonitor{
		URL:           relayURL,
		NormalizedURL: normalizedURL,
		ParsedURL:     parsedURL,
		NetworkType:   detectNetwork(relayURL),
		GeoTags:       geoTags,
		pingMsg:       pingMsg,
		writeCycle:    0,
	}, nil
}

// UpdateNIP11 updates the cached NIP-11 data
func (rm *RelayMonitor) UpdateNIP11(tags Tags, raw string, valid bool) {
	rm.mu.Lock()
	rm.nip11Tags = tags
	rm.nip11Raw = raw
	rm.nip11Valid = valid
	rm.mu.Unlock()
}

// GetNIP11 returns the cached NIP-11 data and validity status
func (rm *RelayMonitor) GetNIP11() (Tags, string, bool) {
	rm.mu.RLock()
	defer rm.mu.RUnlock()
	return rm.nip11Tags, rm.nip11Raw, rm.nip11Valid
}

// UpdateWriteLatency updates the cached write latency
func (rm *RelayMonitor) UpdateWriteLatency(latencyMs int64) {
	rm.mu.Lock()
	rm.writeLatency = latencyMs
	rm.mu.Unlock()
}

// GetWriteLatency returns the cached write latency
func (rm *RelayMonitor) GetWriteLatency() int64 {
	rm.mu.RLock()
	defer rm.mu.RUnlock()
	return rm.writeLatency
}

// ShouldMeasureWrite returns true on first run and every N cycles after
// Default: measure write latency on first run, then every 6 cycles
func (rm *RelayMonitor) ShouldMeasureWrite(interval int) bool {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	// First run (cycle 0) always measures
	if rm.writeCycle == 0 {
		rm.writeCycle = 1
		return true
	}

	rm.writeCycle++
	if rm.writeCycle > interval {
		rm.writeCycle = 1
		return true
	}
	return false
}

// BuildEventTags builds the tags for a Kind 30166 event efficiently
func (rm *RelayMonitor) BuildEventTags(rttDNS, rttOpen, rttRead, rttWrite int64) Tags {
	rm.mu.RLock()
	nip11Tags := rm.nip11Tags
	nip11Valid := rm.nip11Valid
	rm.mu.RUnlock()

	// Pre-allocate with estimated capacity
	tags := make(Tags, 0, len(nip11Tags)+len(rm.GeoTags)+8)

	// Copy NIP-11 tags
	tags = append(tags, nip11Tags...)

	// Add d tag
	tags = append(tags, Tag{"d", rm.NormalizedURL})

	// Add pre-computed geohash tags
	tags = append(tags, rm.GeoTags...)

	// Add RTT tags using strconv (faster than fmt.Sprintf)
	tags = append(tags, Tag{"rtt-dns", strconv.FormatInt(rttDNS, 10)})
	tags = append(tags, Tag{"rtt-open", strconv.FormatInt(rttOpen, 10)})
	tags = append(tags, Tag{"rtt-read", strconv.FormatInt(rttRead, 10)})
	if rttWrite > 0 {
		tags = append(tags, Tag{"rtt-write", strconv.FormatInt(rttWrite, 10)})
	}

	// Add network tag
	tags = append(tags, Tag{"n", rm.NetworkType})

	// Add info check tag (whether NIP-11 was successfully fetched)
	if nip11Valid {
		tags = append(tags, Tag{"info", "true"})
	} else {
		tags = append(tags, Tag{"info", "false"})
	}

	return tags
}

// ============================================================================
// End Performance-Optimized Relay Monitor
// ============================================================================

// detectNetwork determines the network type based on the relay URL
// Returns: clearnet, tor, i2p, or loki
func detectNetwork(relayURL string) string {
	lowerURL := strings.ToLower(relayURL)
	if strings.Contains(lowerURL, ".onion") {
		return "tor"
	}
	if strings.Contains(lowerURL, ".i2p") {
		return "i2p"
	}
	if strings.Contains(lowerURL, ".loki") {
		return "loki"
	}
	return "clearnet"
}

// fetchNIP11Full fetches NIP-11 data and returns both parsed tags and raw JSON
// Uses our own complete NIP-11 struct to capture all fields from the spec
func fetchNIP11Full(ctx context.Context, relayURL string) (Tags, string, error) {
	tags := Tags{}

	// Fetch raw JSON first - this captures everything
	rawJSON, err := fetchNIP11Raw(ctx, relayURL)
	if err != nil {
		return tags, "", err
	}

	// Parse into our complete NIP-11 struct
	var nip11Doc NIP11Document
	if err := json.Unmarshal([]byte(rawJSON), &nip11Doc); err != nil {
		return tags, rawJSON, fmt.Errorf("failed to parse NIP-11 JSON: %w", err)
	}

	// Build tags from parsed data
	// Add supported NIPs as tags (NIP-66: N tag)
	for _, t := range nip11Doc.SupportedNIPs {
		switch v := t.(type) {
		case float64:
			tags = tags.AppendUnique(Tag{"N", fmt.Sprintf("%d", int(v))})
		case int:
			tags = tags.AppendUnique(Tag{"N", fmt.Sprintf("%d", v)})
		}
	}

	// Add relay requirement/limitation tags (NIP-66: R tag)
	if nip11Doc.Limitation != nil {
		if nip11Doc.Limitation.PaymentRequired {
			tags = tags.AppendUnique(Tag{"R", "payment"})
		} else {
			tags = tags.AppendUnique(Tag{"R", "!payment"})
		}
		if nip11Doc.Limitation.AuthRequired {
			tags = tags.AppendUnique(Tag{"R", "auth"})
		} else {
			tags = tags.AppendUnique(Tag{"R", "!auth"})
		}
		if nip11Doc.Limitation.RestrictedWrites {
			tags = tags.AppendUnique(Tag{"R", "writes"})
		} else {
			tags = tags.AppendUnique(Tag{"R", "!writes"})
		}
		if nip11Doc.Limitation.MinPowDifficulty > 0 {
			tags = tags.AppendUnique(Tag{"R", "pow"})
		} else {
			tags = tags.AppendUnique(Tag{"R", "!pow"})
		}
	}

	// Add relay country tags (NIP-66: G tag)
	for _, c := range nip11Doc.RelayCountries {
		tags = tags.AppendUnique(Tag{"G", c, "countryCode"})
	}

	// Add language tags (NIP-66: l tag)
	for _, lang := range nip11Doc.LanguageTags {
		tags = tags.AppendUnique(Tag{"l", lang})
	}

	// Add software tag (NIP-66: s tag)
	if nip11Doc.Software != "" {
		tags = tags.AppendUnique(Tag{"s", nip11Doc.Software})
	}

	// Add relay general tags (NIP-66: t tag)
	for _, t := range nip11Doc.Tags {
		tags = tags.AppendUnique(Tag{"t", t})
	}

	return tags, rawJSON, nil
}

// fetchNIP11Raw fetches the raw NIP-11 JSON document from a relay
// This captures all fields including those not in go-nostr's struct
func fetchNIP11Raw(ctx context.Context, relayURL string) (string, error) {
	// Convert ws(s):// to http(s)://
	httpURL := relayURL
	if strings.HasPrefix(httpURL, "wss://") {
		httpURL = "https://" + strings.TrimPrefix(httpURL, "wss://")
	} else if strings.HasPrefix(httpURL, "ws://") {
		httpURL = "http://" + strings.TrimPrefix(httpURL, "ws://")
	}

	req, err := http.NewRequestWithContext(ctx, "GET", httpURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/nostr+json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	// Validate it's valid JSON
	var js json.RawMessage
	if err := json.Unmarshal(body, &js); err != nil {
		return "", fmt.Errorf("invalid JSON: %w", err)
	}

	return string(body), nil
}

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
	NIP11RefreshInterval  int     `mapstructure:"NIP11_REFRESH_INTERVAL"`
	Publish               bool    `mapstructure:"NOSTR_PUBLISH"`
	PrivateKey            string  `mapstructure:"NOSTR_PRIVATE_KEY"`
	PublishRelayMetrics   string  `mapstructure:"NOSTR_PUBLISH_RELAY_METRICS"`   // Deprecated: use PublishProfileRelays and PublishMetricsRelays
	PublishProfileRelays  string  `mapstructure:"NOSTR_PUBLISH_PROFILE_RELAYS"`  // For Kind 0, 10002
	PublishMetricsRelays  string  `mapstructure:"NOSTR_PUBLISH_METRICS_RELAYS"`  // For Kind 10166, 30166
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

// NIP11Limitation contains all limitation fields from NIP-11 spec
// Includes fields missing from go-nostr's implementation
type NIP11Limitation struct {
	MaxMessageLength    int  `json:"max_message_length,omitempty"`
	MaxSubscriptions    int  `json:"max_subscriptions,omitempty"`
	MaxFilters          int  `json:"max_filters,omitempty"`
	MaxLimit            int  `json:"max_limit,omitempty"`
	MaxSubidLength      int  `json:"max_subid_length,omitempty"`
	MaxEventTags        int  `json:"max_event_tags,omitempty"`
	MaxContentLength    int  `json:"max_content_length,omitempty"`
	MinPowDifficulty    int  `json:"min_pow_difficulty,omitempty"`
	AuthRequired        bool `json:"auth_required"`
	PaymentRequired     bool `json:"payment_required"`
	RestrictedWrites    bool `json:"restricted_writes"`
	// Fields missing from go-nostr
	CreatedAtLowerLimit int64 `json:"created_at_lower_limit,omitempty"`
	CreatedAtUpperLimit int64 `json:"created_at_upper_limit,omitempty"`
	DefaultLimit        int   `json:"default_limit,omitempty"`
}

// NIP11Document is a complete NIP-11 relay information document
// Includes all fields from the spec, including those missing from go-nostr
type NIP11Document struct {
	Name          string   `json:"name,omitempty"`
	Description   string   `json:"description,omitempty"`
	PubKey        string   `json:"pubkey,omitempty"`
	Contact       string   `json:"contact,omitempty"`
	SupportedNIPs []any    `json:"supported_nips,omitempty"`
	Software      string   `json:"software,omitempty"`
	Version       string   `json:"version,omitempty"`
	Icon          string   `json:"icon,omitempty"`
	// Fields missing from go-nostr
	Banner         string `json:"banner,omitempty"`
	Self           string `json:"self,omitempty"`
	PrivacyPolicy  string `json:"privacy_policy,omitempty"`
	TermsOfService string `json:"terms_of_service,omitempty"`

	Limitation     *NIP11Limitation `json:"limitation,omitempty"`
	RelayCountries []string         `json:"relay_countries,omitempty"`
	LanguageTags   []string         `json:"language_tags,omitempty"`
	Tags           []string         `json:"tags,omitempty"`
	PostingPolicy  string           `json:"posting_policy,omitempty"`
	PaymentsURL    string           `json:"payments_url,omitempty"`
}

// RelayPool manages persistent connections to Nostr relays
type RelayPool struct {
	mu      sync.RWMutex
	relays  map[string]*Relay
	urls    []string
	ctx     context.Context
	cancel  context.CancelFunc
}

// NewRelayPool creates a new relay pool and establishes connections
func NewRelayPool(urls []string) *RelayPool {
	ctx, cancel := context.WithCancel(context.Background())
	pool := &RelayPool{
		relays: make(map[string]*Relay),
		urls:   urls,
		ctx:    ctx,
		cancel: cancel,
	}
	pool.connectAll()
	return pool
}

// connectAll establishes connections to all relays in the pool
func (p *RelayPool) connectAll() {
	var wg sync.WaitGroup
	for _, url := range p.urls {
		wg.Add(1)
		go func(u string) {
			defer wg.Done()
			p.connect(u)
		}(url)
	}
	wg.Wait()
}

// connect establishes a connection to a single relay
func (p *RelayPool) connect(url string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Skip if already connected
	if relay, exists := p.relays[url]; exists && relay != nil {
		return
	}

	ctx, cancel := context.WithTimeout(p.ctx, 10*time.Second)
	defer cancel()

	relay, err := RelayConnect(ctx, url)
	if err != nil {
		fmt.Printf("Failed to connect to relay %s: %v\n", url, err)
		return
	}
	p.relays[url] = relay
	fmt.Printf("Connected to relay: %s\n", url)
}

// reconnect attempts to reconnect to a specific relay
func (p *RelayPool) reconnect(url string) {
	p.mu.Lock()
	// Close existing connection if any
	if relay, exists := p.relays[url]; exists && relay != nil {
		relay.Close()
	}
	delete(p.relays, url)
	p.mu.Unlock()

	p.connect(url)
}

// Publish sends an event to all relays in parallel
func (p *RelayPool) Publish(ev Event) (successCount int, failCount int) {
	return p.PublishTo(ev, nil)
}

// PublishTo sends an event to specific relays (or all if urls is nil)
func (p *RelayPool) PublishTo(ev Event, urls []string) (successCount int, failCount int) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	// Build target set for O(1) lookup
	var targetSet map[string]bool
	if urls != nil {
		targetSet = make(map[string]bool, len(urls))
		for _, u := range urls {
			targetSet[u] = true
		}
	}

	var wg sync.WaitGroup
	var mu sync.Mutex

	for url, relay := range p.relays {
		// Skip if not in target set (when urls is specified)
		if targetSet != nil && !targetSet[url] {
			continue
		}
		if relay == nil {
			continue
		}
		wg.Add(1)
		go func(u string, r *Relay) {
			defer wg.Done()

			ctx, cancel := context.WithTimeout(p.ctx, 10*time.Second)
			defer cancel()

			if err := r.Publish(ctx, ev); err != nil {
				fmt.Printf("  -> %s: %v\n", u, err)
				mu.Lock()
				failCount++
				mu.Unlock()
				// Schedule reconnection in background
				go p.reconnect(u)
			} else {
				mu.Lock()
				successCount++
				mu.Unlock()
			}
		}(url, relay)
	}

	wg.Wait()
	return
}

// Close closes all relay connections
func (p *RelayPool) Close() {
	p.cancel()
	p.mu.Lock()
	defer p.mu.Unlock()

	for url, relay := range p.relays {
		if relay != nil {
			relay.Close()
			fmt.Printf("Closed connection to relay: %s\n", url)
		}
	}
	p.relays = make(map[string]*Relay)
}

// EnsureConnections checks and reconnects any disconnected relays
func (p *RelayPool) EnsureConnections() {
	p.mu.RLock()
	toReconnect := []string{}

	for _, url := range p.urls {
		relay, exists := p.relays[url]
		if !exists || relay == nil {
			toReconnect = append(toReconnect, url)
			continue
		}

		// Ping to check if connection is alive
		ctx, cancel := context.WithTimeout(p.ctx, 5*time.Second)
		if err := relay.Ping(ctx); err != nil {
			toReconnect = append(toReconnect, url)
		}
		cancel()
	}
	p.mu.RUnlock()

	for _, url := range toReconnect {
		p.reconnect(url)
	}
}

func main() {

	// Config loading
	viper.AddConfigPath("/usr/local/etc")
	viper.AddConfigPath("./")
	viper.SetConfigName(".relaymonitor.env")
	viper.SetConfigType("env")

	var iConfig *MonitorConfig
	if err := viper.ReadInConfig(); err != nil {
		fmt.Println("Warn: error reading relaymonitor config file from current directory -or- /usr/local/etc/.relaymonitor.env\n", err)
		os.Exit(1)
	}
	// Viper unmarshals the loaded env variables into the struct
	if err := viper.Unmarshal(&iConfig); err != nil {
		fmt.Print("Warn: unable to decode relaymonitor config into struct\n", err)
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
			log.Fatalf("Usage: %s URL, or specify RELAY_URLS in config", args[0])
		}
		rawUrl := args[1]

		_, err := url.Parse(rawUrl)
		if err != nil {
			log.Fatalf("Failed to parse URL: %v", err)
		}
		relayUrls = []string{rawUrl}
	}

	// Parse profile relays (for Kind 0, 10002)
	var profileRelays []string
	if iConfig.PublishProfileRelays != "" {
		if strings.Contains(iConfig.PublishProfileRelays, ",") {
			profileRelays = strings.Split(iConfig.PublishProfileRelays, ",")
		} else {
			profileRelays = []string{iConfig.PublishProfileRelays}
		}
	} else if iConfig.PublishRelayMetrics != "" {
		// Fallback to old config for backward compatibility
		if strings.Contains(iConfig.PublishRelayMetrics, ",") {
			profileRelays = strings.Split(iConfig.PublishRelayMetrics, ",")
		} else {
			profileRelays = []string{iConfig.PublishRelayMetrics}
		}
	}

	// Parse metrics relays (for Kind 10166, 30166)
	var metricsRelays []string
	if iConfig.PublishMetricsRelays != "" {
		if strings.Contains(iConfig.PublishMetricsRelays, ",") {
			metricsRelays = strings.Split(iConfig.PublishMetricsRelays, ",")
		} else {
			metricsRelays = []string{iConfig.PublishMetricsRelays}
		}
	} else if iConfig.PublishRelayMetrics != "" {
		// Fallback to old config for backward compatibility
		if strings.Contains(iConfig.PublishRelayMetrics, ",") {
			metricsRelays = strings.Split(iConfig.PublishRelayMetrics, ",")
		} else {
			metricsRelays = []string{iConfig.PublishRelayMetrics}
		}
	}

	fmt.Printf("Publishing profiles to %d relays: %v\n", len(profileRelays), profileRelays)
	fmt.Printf("Publishing metrics to %d relays: %v\n", len(metricsRelays), metricsRelays)

	// Create unified relay pool with all unique URLs (avoids duplicate connections)
	allRelayURLs := make(map[string]bool)
	for _, u := range profileRelays {
		if u != "" {
			allRelayURLs[u] = true
		}
	}
	for _, u := range metricsRelays {
		if u != "" {
			allRelayURLs[u] = true
		}
	}
	uniqueURLs := make([]string, 0, len(allRelayURLs))
	for u := range allRelayURLs {
		uniqueURLs = append(uniqueURLs, u)
	}

	var relayPool *RelayPool
	if len(uniqueURLs) > 0 {
		fmt.Printf("Creating unified relay pool with %d unique relays\n", len(uniqueURLs))
		relayPool = NewRelayPool(uniqueURLs)
	}

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

	// Normalize private key (convert nsec to hex if needed)
	privateKeyHex, err := NormalizePrivateKey(iConfig.PrivateKey)
	if err != nil {
		log.Fatalf("Failed to normalize private key: %v", err)
	}

	pub, err := GetPublicKey(privateKeyHex)
	if err != nil {
		log.Fatalf("Failed to derive public key from private key: %v", err)
	}

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

		newProfileJson, err := json.Marshal(newProfile)
		if err != nil {
			fmt.Printf("Error marshaling profile JSON: %s\n", err)
		}

		profileEv := Event{
			PubKey:    pub,
			CreatedAt: Timestamp(time.Now().Unix()),
			Kind:      0,
			Tags:      Tags{},
			Content:   string(newProfileJson),
		}

		if err := profileEv.Sign(privateKeyHex); err != nil {
			fmt.Printf("Error signing kind 0 event: %s\n", err)
		}

		if relayPool != nil && len(profileRelays) > 0 {
			success, fail := relayPool.PublishTo(profileEv, profileRelays)
			fmt.Printf("published monitor profile kind:0 to %d/%d relays\n", success, success+fail)
		}

		// 10002 - Monitor Relay List (include both profile and metrics relays)
		relayTags := Tags{}
		allRelays := make(map[string]bool)
		for _, t := range profileRelays {
			allRelays[t] = true
		}
		for _, t := range metricsRelays {
			allRelays[t] = true
		}
		for t := range allRelays {
			relayTags = relayTags.AppendUnique(Tag{"r", t, "write"})
		}
		fmt.Printf("Building relay list event with %d relays\n", len(allRelays))
		relayListEv := Event{
			PubKey:    pub,
			CreatedAt: Timestamp(time.Now().Unix()),
			Kind:      10002,
			Tags:      relayTags,
			Content:   "",
		}
		if err := relayListEv.Sign(privateKeyHex); err != nil {
			fmt.Printf("Error signing kind 10002 event: %s\n", err)
		}
		if relayPool != nil && len(profileRelays) > 0 {
			success, fail := relayPool.PublishTo(relayListEv, profileRelays)
			fmt.Printf("published monitor relayList kind:10002 to %d/%d relays\n", success, success+fail)
		}

		// Publish to Nostr
		// 10166 - Monitor Announcement (NIP-66)
		profileTags := Tags{
			Tag{"frequency", useFrequencySecondsString},
			Tag{"o", pub},
			Tag{"k", "30166"},
			Tag{"c", "open"},
			Tag{"c", "read"},
			Tag{"c", "write"},
			Tag{"timeout", "5000", "open"},
			Tag{"timeout", "15000", "read"},
			Tag{"timeout", "15000", "write"},
			Tag{"G", iConfig.MonitorCountryCode, "countryCode"},
		}

		// for every geo tag, encode all precisions from 1 to 9
		monitorGeo := geohash.EncodeWithPrecision(iConfig.MonitorLatitude, iConfig.MonitorLongitude, 9)
		fmt.Printf("Monitor location geohash: %s (lat: %.4f, lon: %.4f)\n", monitorGeo, iConfig.MonitorLatitude, iConfig.MonitorLongitude)
		for i := 1; i <= 9; i++ {
			profileTags = profileTags.AppendUnique(Tag{"g", monitorGeo[:i]})
		}

		ev := Event{
			PubKey:    pub,
			CreatedAt: Timestamp(time.Now().Unix()),
			Kind:      10166,
			Tags:      profileTags,
			Content:   "",
		}

		if err := ev.Sign(privateKeyHex); err != nil {
			fmt.Printf("Error signing kind 10166 event: %s\n", err)
		}
		if relayPool != nil && len(metricsRelays) > 0 {
			success, fail := relayPool.PublishTo(ev, metricsRelays)
			fmt.Printf("published monitor registration kind:10166 to %d/%d relays\n", success, success+fail)
		}
	}

	// Channel to collect tickers for cleanup on shutdown
	var tickers []*time.Ticker

	// NIP-11 refresh interval (default 10 minutes)
	nip11RefreshInterval := 10 * time.Minute
	if iConfig.NIP11RefreshInterval > 0 {
		nip11RefreshInterval = time.Duration(iConfig.NIP11RefreshInterval) * time.Second
		fmt.Printf("NIP-11 refresh interval: %v\n", nip11RefreshInterval)
	}

	// Write latency sampling interval (measure every N cycles to reduce overhead)
	// At 10s frequency, 6 cycles = 1 minute between write latency measurements
	writeSampleInterval := 6

	// Create all relay monitors first
	monitors := make([]*RelayMonitor, 0, len(relayUrls))
	for _, u := range relayUrls {
		monitor, err := NewRelayMonitor(u, iConfig.RelayLatitude, iConfig.RelayLongitude)
		if err != nil {
			fmt.Printf("Error creating monitor for %s: %s\n", u, err)
			continue
		}
		monitors = append(monitors, monitor)
	}

	// Batch fetch all NIP-11 documents in parallel
	fmt.Printf("Fetching NIP-11 for %d relays in parallel...\n", len(monitors))
	var nip11Wg sync.WaitGroup
	for _, mon := range monitors {
		nip11Wg.Add(1)
		go func(m *RelayMonitor) {
			defer nip11Wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if tags, rawJSON, err := fetchNIP11Full(ctx, m.URL); err != nil {
				fmt.Printf("Error fetching NIP-11 for %s: %s\n", m.URL, err)
				m.UpdateNIP11(nil, "", false)
			} else {
				m.UpdateNIP11(tags, rawJSON, true)
				fmt.Printf("Fetched NIP-11 for %s: %d tags, %d bytes\n", m.URL, len(tags), len(rawJSON))
			}
		}(mon)
	}
	nip11Wg.Wait()
	fmt.Printf("NIP-11 fetch complete for all relays\n")

	//FOR EACH RELAY
	for i, monitor := range monitors {

		// Start NIP-11 refresh goroutine for this relay
		nip11RefreshTicker := time.NewTicker(nip11RefreshInterval)
		tickers = append(tickers, nip11RefreshTicker)
		go func(mon *RelayMonitor, refreshTicker *time.Ticker) {
			for range refreshTicker.C {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				if tags, rawJSON, err := fetchNIP11Full(ctx, mon.URL); err == nil {
					mon.UpdateNIP11(tags, rawJSON, true)
				} else {
					mon.UpdateNIP11(nil, "", false)
				}
				cancel()
			}
		}(monitor, nip11RefreshTicker)

		// Stagger startup using time.AfterFunc (non-blocking)
		staggerDelay := time.Duration(i) * time.Duration(rand.Intn(int(useFrequency.Seconds())/len(monitors)+1)) * time.Second

		ticker := time.NewTicker(useFrequency)
		tickers = append(tickers, ticker)

		// Start monitoring goroutine after stagger delay
		// Capture variables for closure
		monitorCopy := monitor
		tickerCopy := ticker
		time.AfterFunc(staggerDelay, func() {
			go func(mon *RelayMonitor, t *time.Ticker) {
				// Pre-allocate reusable http.Header (empty, but avoids nil allocation)
				emptyHeader := http.Header{}

				// Run immediately on first iteration, then wait for ticker
				firstRun := true
				for {
					if !firstRun {
						<-t.C
					}
					firstRun = false
					whatTime := time.Now()

					// Use cached parsed URL and pre-allocated ping message
					result, _, err := wsstat.MeasureLatency(mon.ParsedURL, string(mon.pingMsg), emptyHeader)
					if err != nil {
						fmt.Printf("Error measuring latency for %s: %v\n", mon.URL, err)
						continue
					}

					// Calculate RTT values
					dnsMs := result.DNSLookup.Milliseconds()
					openConnMs := dnsMs + result.TCPConnection.Milliseconds() +
						result.TLSHandshake.Milliseconds() + result.WSHandshake.Milliseconds()
					readMs := result.MessageRoundTrip.Milliseconds()

					// Only measure write latency periodically (expensive operation)
					writeMs := mon.GetWriteLatency()
					if mon.ShouldMeasureWrite(writeSampleInterval) {
						writeCtx, writeCancel := context.WithTimeout(context.Background(), 15*time.Second)
						if latency, err := MeasureWriteLatency(writeCtx, mon.URL, privateKeyHex, pub); err == nil {
							writeMs = latency.Milliseconds()
							mon.UpdateWriteLatency(writeMs)
						}
						writeCancel()
					}

					// Write to InfluxDB (if enabled)
					if influxEnabled {
						point := influxdb2.NewPoint(
							iConfig.InfluxMeasurement,
							map[string]string{
								"relay":   mon.URL,
								"monitor": iConfig.MonitorName,
							},
							map[string]interface{}{
								"dnslookup":     result.DNSLookup.Milliseconds(),
								"tcpconnection": result.TCPConnection.Milliseconds(),
								"tlshandshake":  result.TLSHandshake.Milliseconds(),
								"wshandshake":   result.WSHandshake.Milliseconds(),
								"wsrtt":         readMs,
								"wswrite":       writeMs,
								"totaltime":     result.TotalTime.Milliseconds(),
							},
							whatTime,
						)
						writeAPI.WritePoint(point)
					}

					// Get NIP-11 content for event
					_, nip11Content, _ := mon.GetNIP11()

					// Build tags efficiently using pre-computed values
					newTags := mon.BuildEventTags(dnsMs, openConnMs, readMs, writeMs)

					if iConfig.Publish && relayPool != nil && len(metricsRelays) > 0 {
						// Publish to Nostr stats/kind 30166 using pooled event
						ev := GetEvent()
						ev.PubKey = pub
						ev.CreatedAt = Timestamp(whatTime.Unix())
						ev.Kind = 30166
						ev.Tags = newTags
						ev.Content = nip11Content

						if err := ev.Sign(privateKeyHex); err != nil {
							fmt.Printf("Error signing kind 30166 for %s: %v\n", mon.URL, err)
							PutEvent(ev)
							continue
						}
						success, fail := relayPool.PublishTo(*ev, metricsRelays)
						PutEvent(ev)
						fmt.Printf("Published kind 30166 for %s to %d/%d relays (rtt-dns: %dms, rtt-open: %dms, rtt-read: %dms, rtt-write: %dms)\n",
							mon.URL, success, success+fail, dnsMs, openConnMs, readMs, writeMs)
					}
				}
			}(monitorCopy, tickerCopy)
		})
	}

	// Start periodic connection health check
	healthCheckTicker := time.NewTicker(60 * time.Second)
	go func() {
		for range healthCheckTicker.C {
			if relayPool != nil {
				relayPool.EnsureConnections()
			}
		}
	}()

	// Graceful shutdown handling
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	fmt.Println("\nShutting down gracefully...")

	// Stop health check ticker
	healthCheckTicker.Stop()

	// Stop all monitoring tickers
	for _, t := range tickers {
		t.Stop()
	}

	// Close relay pool connections
	if relayPool != nil {
		relayPool.Close()
	}

	// Close InfluxDB client and flush remaining data
	if influxEnabled && client != nil {
		writeAPI.Flush()
		client.Close()
		fmt.Println("InfluxDB client closed")
	}

	fmt.Println("Shutdown complete")
}
