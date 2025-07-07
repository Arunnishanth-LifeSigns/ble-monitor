// File: main.go
package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// --- Configuration Variables ---
var (
	acDomain           string
	jsonNotifyEndpoint = "https://qa-testing-api-gw.lifesigns.us/api/v1/public/external-device"
	configMutex        sync.RWMutex
	servicesStarted    bool
	servicesMutex      sync.Mutex

	receiverMode     string // "perAP" or "local"
	localReceiverURL string // URL for the single local receiver_thread_local.js
)

const (
	SSE_PATH                      = "/api/aps/events"
	AP_STATE_PATH                 = "/api/aps/ap-state/open"
	AP_STATE_CLOSE_PATH           = "/api/aps/ap-state/close"
	SCAN_PATH                     = "/api/aps/scan/open"
	CONNECT_PATH                  = "/api/aps/connections/connect"
	CONNECTION_STATE_OPEN_PATH    = "/api/aps/connection-state/open"
	DISCONNECT_PATH               = "/api/aps/connections/disconnect"
	CONNECT_TIMEOUT               = 20 * time.Second
	RETRY_DELAY                   = 2 * time.Second
	HEARTBEAT_INTERVAL            = 30 * time.Second
	TOKEN_REFRESH_INTERVAL        = 59 * time.Minute
	DEVICE_CSV_PATH               = "Device Type - Sheet1.csv"
	AP_CONFIG_CSV_PATH            = "ap_config.csv"
	DEVICE_INACTIVITY_CONFIG_PATH = "device_inactivity_config.csv"
	SCANNED_DEVICE_LIFETIME       = 30 * time.Second
	NODEJS_PER_AP_PORT            = "60501"
	DEFAULT_RECEIVER_MODE         = "perAP" // CORRECTED: Changed default to perAP
	STALE_GRACE_PERIOD            = HEARTBEAT_INTERVAL + 5*time.Second
	MAX_LOG_ENTRIES               = 200
	STALLED_CONNECTION_THRESHOLD  = 4
)

// --- Struct Definitions ---
type TokenResponse struct {
	TokenType    string `json:"token_type"`
	AccessToken  string `json:"access_token"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refreshToken,omitempty"`
}

type ScanRequest struct {
	Aps    []string `json:"aps"`
	Active int      `json:"active"`
}

type ConnectedDevice struct {
	Name                 string    `json:"name"`
	MACID                string    `json:"mac"`
	AP                   string    `json:"ap"`
	Status               string    `json:"connectionState"`
	Type                 string    `json:"deviceType"`
	LastSeen             time.Time `json:"last_seen"`
	DeviceType           string    `json:"-"`
	NotificationSent     bool      `json:"-"`
	LastPacketActivityAt time.Time `json:"-"`
	SuspectedStale       bool      `json:"suspected_stale"`
	StalledCheckCounter  int       `json:"-"`
}

type ScannedDeviceEntry struct {
	Data     map[string]interface{}
	LastSeen time.Time
}

type ScannedDeviceDisplay struct {
	MACID      string                 `json:"mac"`
	Name       string                 `json:"name"`
	RSSI       int                    `json:"rssi"`
	AP         string                 `json:"ap"`
	DeviceType string                 `json:"deviceType"`
	RawData    map[string]interface{} `json:"-"`
}

// MODIFIED: BlacklistedDeviceEntry now includes the device name
type BlacklistedDeviceEntry struct {
	MAC           string    `json:"mac"`
	Name          string    `json:"name"`
	BlacklistedAt time.Time `json:"blacklistedAt"`
}

type ConnectionLogEntry struct {
	Timestamp  time.Time `json:"timestamp"`
	MAC        string    `json:"mac"`
	DeviceName string    `json:"deviceName"`
	AP         string    `json:"ap"`
	Status     string    `json:"status"`
}

type APInfo struct {
	MAC     string `json:"mac"`
	LocalIP string `json:"localip"`
	Status  string `json:"status"`
}

// --- Global Variables ---
var connectedDevices = make(map[string]ConnectedDevice)
var connectedDevicesMutex sync.RWMutex

var scannedDevices = make(map[string]ScannedDeviceEntry)
var scannedDevicesMutex sync.RWMutex

var deviceNameCache = make(map[string]string)
var deviceNameCacheMutex sync.RWMutex

var deviceTypeMappings = make(map[string]string)
var deviceMacToTypeCache = make(map[string]string)
var deviceMacToTypeCacheMutex sync.RWMutex

// MODIFIED: The blacklist now stores the BlacklistedDeviceEntry struct
var deviceBlacklist = make(map[string]BlacklistedDeviceEntry)
var deviceBlacklistMutex sync.RWMutex

var activeAPs = make(map[string]APInfo)
var activeAPsMutex sync.RWMutex

var apListForConnectionState = []string{}
var targetNamePrefixes = []string{}

var connectingQueue = make(map[string]struct{})
var connectingQueueMutex sync.Mutex

var (
	accessToken      string
	accessTokenMutex sync.RWMutex
)

var displayDevicesMap = make(map[string]ScannedDeviceDisplay)
var displayDevicesMapMutex sync.RWMutex

var apNodeJSMapping = make(map[string]string)
var apNodeJSMappingMutex sync.RWMutex

var deviceInactivitySettings = make(map[string]time.Duration)
var deviceInactivitySettingsMutex sync.RWMutex

var sseReadyChan = make(chan bool, 1)

var connectionLogs = make([]ConnectionLogEntry, 0, MAX_LOG_ENTRIES)
var connectionLogsMutex sync.RWMutex

var sseBody io.ReadCloser

// --- Utility Functions ---
// ... (loadDeviceTypes, loadAPConfig, etc. are unchanged)
func loadDeviceTypes(filePath string) error {
	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("failed to open device type CSV '%s': %w", filePath, err)
	}
	defer file.Close()
	reader := csv.NewReader(file)
	records, err := reader.ReadAll()
	if err != nil {
		return fmt.Errorf("failed to read device type CSV '%s': %w", filePath, err)
	}

	localTargetPrefixes := []string{}
	localDeviceTypeMappings := make(map[string]string)
	if len(records) > 1 { // Header + data
		for i, record := range records {
			if i == 0 {
				continue
			} // Skip header
			if len(record) >= 2 {
				prefix, deviceType := strings.TrimSpace(record[0]), strings.TrimSpace(record[1])
				if prefix != "" && deviceType != "" {
					localTargetPrefixes = append(localTargetPrefixes, prefix)
					localDeviceTypeMappings[prefix] = deviceType
					log.Printf("Loaded prefix: '%s', type: '%s'", prefix, deviceType)
				}
			}
		}
	}
	targetNamePrefixes = localTargetPrefixes
	deviceTypeMappings = localDeviceTypeMappings
	log.Printf("Loaded %d device type mappings. Target Prefixes: %v", len(deviceTypeMappings), targetNamePrefixes)
	return nil
}

func loadAPConfig(filePath string) error {
	file, err := os.Open(filePath)
	if err != nil {
		log.Printf("Warning: Could not open AP config CSV '%s' for fallback: %v", filePath, err)
		return nil
	}
	defer file.Close()
	reader := csv.NewReader(file)
	records, err := reader.ReadAll()
	if err != nil {
		return fmt.Errorf("failed to read AP config CSV '%s': %w", filePath, err)
	}

	localAPNodeJSMapping := make(map[string]string)
	initialAPList := []string{}
	if len(records) > 1 {
		for i, record := range records {
			if i == 0 {
				continue
			} // Skip header
			if len(record) >= 2 {
				apMAC, apNodeDomain := strings.TrimSpace(record[0]), strings.TrimSpace(record[1])
				if apMAC != "" {
					initialAPList = append(initialAPList, apMAC)
					if apNodeDomain != "" {
						localAPNodeJSMapping[apMAC] = apNodeDomain
					}
				}
			}
		}
	}
	apNodeJSMappingMutex.Lock()
	apNodeJSMapping = localAPNodeJSMapping
	apNodeJSMappingMutex.Unlock()

	activeAPsMutex.Lock()
	apListForConnectionState = initialAPList
	for _, mac := range initialAPList {
		activeAPs[mac] = APInfo{MAC: mac, LocalIP: localAPNodeJSMapping[mac], Status: "unknown"}
	}
	activeAPsMutex.Unlock()

	log.Printf("Loaded %d AP to NodeJS domain mappings from %s as initial fallback.", len(apNodeJSMapping), filePath)
	return nil
}

func loadDeviceInactivitySettings(filePath string) error {
	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("failed to open device inactivity config CSV '%s': %w", filePath, err)
	}
	defer file.Close()
	reader := csv.NewReader(file)
	records, err := reader.ReadAll()
	if err != nil {
		return fmt.Errorf("failed to read device inactivity config CSV '%s': %w", filePath, err)
	}

	localSettings := make(map[string]time.Duration)
	if len(records) > 1 {
		for i, record := range records {
			if i == 0 {
				continue
			} // Skip header
			if len(record) >= 2 {
				deviceType, timeoutMinutesStr := strings.TrimSpace(record[0]), strings.TrimSpace(record[1])
				if deviceType != "" && timeoutMinutesStr != "" {
					timeoutMinutes, errConv := strconv.Atoi(timeoutMinutesStr)
					if errConv == nil && timeoutMinutes > 0 {
						localSettings[deviceType] = time.Duration(timeoutMinutes) * time.Minute
					} else {
						log.Printf("Invalid timeout '%s' for device type '%s' in %s. Skipping.", timeoutMinutesStr, deviceType, filePath)
					}
				}
			}
		}
	}
	deviceInactivitySettingsMutex.Lock()
	deviceInactivitySettings = localSettings
	deviceInactivitySettingsMutex.Unlock()
	log.Printf("Loaded %d device inactivity settings from %s.", len(deviceInactivitySettings), filePath)
	return nil
}

func addConnectionLog(mac, deviceName, ap, status string) {
	log.Printf("LOGGING EVENT: MAC=%s, Name=%s, AP=%s, Status=%s", mac, deviceName, ap, status)
	connectionLogsMutex.Lock()
	defer connectionLogsMutex.Unlock()

	newLog := ConnectionLogEntry{
		Timestamp:  time.Now(),
		MAC:        mac,
		DeviceName: deviceName,
		AP:         ap,
		Status:     status,
	}
	connectionLogs = append([]ConnectionLogEntry{newLog}, connectionLogs...)
	if len(connectionLogs) > MAX_LOG_ENTRIES {
		connectionLogs = connectionLogs[:MAX_LOG_ENTRIES]
	}
}

func getDeviceTypeFromName(name string) string {
	for prefix, devType := range deviceTypeMappings {
		if strings.HasPrefix(name, prefix) {
			return devType
		}
	}
	return "unknown"
}

func getStableName(macID string) string {
	deviceNameCacheMutex.RLock()
	name, found := deviceNameCache[macID]
	deviceNameCacheMutex.RUnlock()
	if found && name != "" && name != "(unknown)" {
		return name
	}
	return "(unknown)"
}

func updateStableName(macID string, newName string) {
	if newName == "" || newName == "(unknown)" {
		return
	}
	deviceNameCacheMutex.Lock()
	oldName, exists := deviceNameCache[macID]
	if !exists || oldName == "(unknown)" || oldName == "" || newName != oldName {
		deviceNameCache[macID] = newName
		if newName != oldName && exists {
			deviceMacToTypeCacheMutex.Lock()
			delete(deviceMacToTypeCache, macID)
			deviceMacToTypeCacheMutex.Unlock()
		}
	}
	deviceNameCacheMutex.Unlock()
}

func getStableDeviceType(macID string, currentName string) string {
	deviceMacToTypeCacheMutex.RLock()
	devType, found := deviceMacToTypeCache[macID]
	deviceMacToTypeCacheMutex.RUnlock()
	if found && devType != "unknown" {
		return devType
	}

	nameForType := currentName
	if nameForType == "(unknown)" || nameForType == "" {
		nameForType = getStableName(macID)
	}

	derivedType := getDeviceTypeFromName(nameForType)
	if derivedType != "unknown" {
		deviceMacToTypeCacheMutex.Lock()
		deviceMacToTypeCache[macID] = derivedType
		deviceMacToTypeCacheMutex.Unlock()
	}
	return derivedType
}

// --- API Interaction Functions ---
func getAuthToken() (*TokenResponse, error) {
	configMutex.RLock()
	currentACDomain := acDomain
	configMutex.RUnlock()
	apiURL := currentACDomain + "/api/oauth2/token"
	clientID, clientSecret := "lifesigns", "ca04600dd2345948#"
	auth := base64.StdEncoding.EncodeToString([]byte(clientID + ":" + clientSecret))
	data := url.Values{}
	data.Set("grant_type", "client_credentials")
	req, err := http.NewRequest("POST", apiURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, fmt.Errorf("create auth request failed: %w", err)
	}
	req.Header.Set("Authorization", "Basic "+auth)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("exec auth request failed: %w", err)
	}
	defer resp.Body.Close()
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read auth response body failed: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("auth request failed status %d: %s", resp.StatusCode, string(bodyBytes))
	}
	var tokenResponse TokenResponse
	if err := json.Unmarshal(bodyBytes, &tokenResponse); err != nil {
		return nil, fmt.Errorf("unmarshal auth response failed: %w", err)
	}
	return &tokenResponse, nil
}

func refreshAccessToken() {
	log.Println("Attempting to refresh access token...")
	newToken, err := getAuthToken()
	if err != nil {
		log.Printf("Error refreshing access token: %v\n", err)
		return
	}
	accessTokenMutex.Lock()
	accessToken = newToken.AccessToken
	accessTokenMutex.Unlock()
	log.Println("Access token refreshed successfully.")
}

func getNodeJSBaseURL(hubAPMac string) (string, error) {
	configMutex.RLock()
	currentReceiverMode := receiverMode
	currentLocalReceiverURL := localReceiverURL
	configMutex.RUnlock()

	if currentReceiverMode == "local" {
		if currentLocalReceiverURL == "" {
			return "", fmt.Errorf("receiver mode is 'local', but local receiver URL is not configured")
		}
		return strings.TrimRight(currentLocalReceiverURL, "/"), nil
	}

	// perAP mode
	if hubAPMac == "" {
		return "", fmt.Errorf("perAP mode requires a valid hubAPMac for Node.js URL, but it was empty")
	}

	activeAPsMutex.RLock()
	apInfo, ok := activeAPs[hubAPMac]
	activeAPsMutex.RUnlock()
	if ok && apInfo.LocalIP != "" {
		return fmt.Sprintf("http://%s:%s", apInfo.LocalIP, NODEJS_PER_AP_PORT), nil
	}

	apNodeJSMappingMutex.RLock()
	apNodeDomain, ok := apNodeJSMapping[hubAPMac]
	apNodeJSMappingMutex.RUnlock()
	if !ok {
		return "", fmt.Errorf("NodeJS IP for AP %s not found in dynamic list or fallback ap_config.csv", hubAPMac)
	}
	return fmt.Sprintf("http://%s:%s", apNodeDomain, NODEJS_PER_AP_PORT), nil
}

func initiateNotification(deviceMAC, deviceType, hubAPMac string) error {
	baseURL, err := getNodeJSBaseURL(hubAPMac)
	if err != nil {
		log.Printf("Cannot initiate notification for %s on AP %s: %v", deviceMAC, hubAPMac, err)
		return err
	}
	apiURL := baseURL + "/start_notify"
	accessTokenMutex.RLock()
	currentAccessToken := accessToken
	accessTokenMutex.RUnlock()
	configMutex.RLock()
	currentACDomainForPayload := acDomain
	currentJsonNotifyEndpoint := jsonNotifyEndpoint
	configMutex.RUnlock()

	payload := map[string]string{
		"acDomain":     currentACDomainForPayload,
		"hubMac":       hubAPMac,
		"deviceMac":    deviceMAC,
		"deviceType":   deviceType,
		"accessToken":  currentAccessToken,
		"jsonEndpoint": currentJsonNotifyEndpoint,
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal start_notify payload failed: %w", err)
	}

	req, err := http.NewRequest("POST", apiURL, bytes.NewReader(payloadBytes))
	if err != nil {
		return fmt.Errorf("create start_notify request failed: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("execute start_notify request to %s failed: %w", apiURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		log.Printf("Successfully initiated/confirmed notification for %s (Type: %s) via AP %s (NodeJS URL: %s)", deviceMAC, deviceType, hubAPMac, apiURL)
		connectedDevicesMutex.Lock()
		currentDeviceState, stillExists := connectedDevices[deviceMAC]
		if stillExists && currentDeviceState.AP == hubAPMac {
			if !currentDeviceState.NotificationSent {
				currentDeviceState.NotificationSent = true
				currentDeviceState.LastPacketActivityAt = time.Now()
				currentDeviceState.StalledCheckCounter = 0
				connectedDevices[deviceMAC] = currentDeviceState
			}
		} else {
			log.Printf("Warning: Device %s state or AP (%s vs current AP %s) changed during notification POST to %s. Flags not updated.", deviceMAC, currentDeviceState.AP, hubAPMac, apiURL)
		}
		connectedDevicesMutex.Unlock()
		return nil
	}

	respBodyBytes, _ := io.ReadAll(resp.Body)
	log.Printf("start_notify request for %s to %s failed, status %d: %s", deviceMAC, apiURL, resp.StatusCode, string(respBodyBytes))
	return fmt.Errorf("start_notify to %s failed, status %d: %s", apiURL, resp.StatusCode, string(respBodyBytes))
}

func stopNotification(deviceMAC, hubAPMac string) error {
	if hubAPMac == "" {
		log.Printf("Error stopping notification for %s: hubAPMac is empty.", deviceMAC)
		return fmt.Errorf("hubAPMac is required for stopNotification")
	}
	baseURL, err := getNodeJSBaseURL(hubAPMac)
	if err != nil {
		log.Printf("Cannot stop notification for %s on AP %s: %v", deviceMAC, hubAPMac, err)
		return err
	}
	apiURL := baseURL + "/stop_notify"

	payload := map[string]string{"deviceMac": deviceMAC}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal stop_notify payload failed: %w", err)
	}

	req, err := http.NewRequest("POST", apiURL, bytes.NewReader(payloadBytes))
	if err != nil {
		return fmt.Errorf("create stop_notify request failed: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("execute stop_notify request to %s failed: %w", apiURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBodyBytes, _ := io.ReadAll(resp.Body)
		log.Printf("stop_notify request for %s to %s gave unexpected status %d: %s", deviceMAC, apiURL, resp.StatusCode, string(respBodyBytes))
		return fmt.Errorf("stop_notify to %s failed, status %d: %s", apiURL, resp.StatusCode, string(respBodyBytes))
	}

	log.Printf("Successfully sent stop notification request for %s via AP %s (NodeJS URL: %s).", deviceMAC, hubAPMac, apiURL)
	return nil
}

func setupSSEConnection(path string) (io.ReadCloser, *bufio.Reader, error) {
	accessTokenMutex.RLock()
	currentAccessToken := accessToken
	accessTokenMutex.RUnlock()
	configMutex.RLock()
	currentACDomain := acDomain
	configMutex.RUnlock()
	req, err := http.NewRequest("GET", currentACDomain+path, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("create SSE request for %s failed: %w", path, err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Authorization", "Bearer "+currentAccessToken)
	client := &http.Client{Timeout: 0}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("connect to SSE %s failed: %w", path, err)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		bodyBytes, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, nil, fmt.Errorf("SSE connection %s failed status %s: %s", path, resp.Status, string(bodyBytes))
	}
	return resp.Body, bufio.NewReader(resp.Body), nil
}

func scanBLEDevices() error {
	accessTokenMutex.RLock()
	currentAccessToken := accessToken
	accessTokenMutex.RUnlock()
	configMutex.RLock()
	currentACDomain := acDomain
	configMutex.RUnlock()

	activeAPsMutex.RLock()
	currentAPList := make([]string, 0, len(activeAPs))
	for mac := range activeAPs {
		currentAPList = append(currentAPList, mac)
	}
	activeAPsMutex.RUnlock()

	if len(currentAPList) == 0 {
		log.Println("scanBLEDevices: No active APs to scan on.")
		return nil
	}

	apiURL := currentACDomain + SCAN_PATH
	scanRequest := ScanRequest{Aps: currentAPList, Active: 1}
	requestBody, _ := json.Marshal(scanRequest)
	req, _ := http.NewRequest("POST", apiURL, bytes.NewReader(requestBody))
	req.Header.Set("Authorization", "Bearer "+currentAccessToken)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("execute scan request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("scan request failed status %d: %s", resp.StatusCode, string(respBody))
	}
	log.Println("Successfully initiated BLE scan on APs:", currentAPList)
	return nil
}

func connectDeviceAPI(deviceMAC string, apsToTry []string) error {
	accessTokenMutex.RLock()
	currentAccessToken := accessToken
	accessTokenMutex.RUnlock()
	configMutex.RLock()
	currentACDomain := acDomain
	configMutex.RUnlock()

	apiURL := currentACDomain + CONNECT_PATH

	if len(apsToTry) == 0 {
		return fmt.Errorf("no APs specified for connection attempt to %s", deviceMAC)
	}

	connectPayload := map[string][]string{"aps": apsToTry, "devices": []string{deviceMAC}}
	payloadBytes, err := json.Marshal(connectPayload)
	if err != nil {
		return fmt.Errorf("failed to marshal connect payload for %s: %w", deviceMAC, err)
	}

	req, err := http.NewRequest("POST", apiURL, bytes.NewReader(payloadBytes))
	if err != nil {
		return fmt.Errorf("failed to create new HTTP request for %s: %w", deviceMAC, err)
	}
	req.Header.Set("Authorization", "Bearer "+currentAccessToken)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("execute connect for %s failed: %w", deviceMAC, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("WARN: Could not read response body for device %s: %v", deviceMAC, err)
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("connect for %s failed status %d: %s", deviceMAC, resp.StatusCode, string(respBody))
	}

	return nil
}

// MODIFIED: Function signature now accepts a deviceName
func disconnectDevice(deviceMAC, deviceName string, blacklist bool) error {
	var hubAPMacToStopOn string
	connectedDevicesMutex.RLock()
	if dev, exists := connectedDevices[deviceMAC]; exists {
		hubAPMacToStopOn = dev.AP
	}
	connectedDevicesMutex.RUnlock()

	if hubAPMacToStopOn != "" {
		if err := stopNotification(deviceMAC, hubAPMacToStopOn); err != nil {
			log.Printf("Warning: Error sending stop_notify for %s (AP: %s): %v. Proceeding.", deviceMAC, hubAPMacToStopOn, err)
		}
	}

	if blacklist {
		deviceBlacklistMutex.Lock()
		// MODIFIED: Store the struct with the device name
		deviceBlacklist[deviceMAC] = BlacklistedDeviceEntry{
			MAC:           deviceMAC,
			Name:          deviceName,
			BlacklistedAt: time.Now(),
		}
		deviceBlacklistMutex.Unlock()
		log.Printf("Device %s (%s) added to blacklist.", deviceMAC, deviceName)
	}

	accessTokenMutex.RLock()
	currentAccessToken := accessToken
	accessTokenMutex.RUnlock()
	configMutex.RLock()
	currentACDomain := acDomain
	configMutex.RUnlock()
	apiURL := currentACDomain + DISCONNECT_PATH
	disconnectPayload := map[string][]string{"devices": []string{deviceMAC}}
	payloadBytes, err := json.Marshal(disconnectPayload)
	if err != nil {
		return fmt.Errorf("failed to marshal disconnect payload for %s: %w", deviceMAC, err)
	}

	req, err := http.NewRequest("POST", apiURL, bytes.NewReader(payloadBytes))
	if err != nil {
		return fmt.Errorf("failed to create disconnect request for %s: %w", deviceMAC, err)
	}
	req.Header.Set("Authorization", "Bearer "+currentAccessToken)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to execute disconnect request for %s: %w", deviceMAC, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("AC disconnect request failed for %s with status %d: %s", deviceMAC, resp.StatusCode, string(respBody))
	}

	log.Printf("Successfully initiated AC disconnection for device %s.\n", deviceMAC)
	return nil
}

func disconnectAllDevices() error {
	connectedDevicesMutex.RLock()
	macsToDisconnect := make([]string, 0, len(connectedDevices))
	for mac := range connectedDevices {
		macsToDisconnect = append(macsToDisconnect, mac)
	}
	connectedDevicesMutex.RUnlock()

	if len(macsToDisconnect) == 0 {
		log.Println("No devices to disconnect.")
		return nil
	}
	log.Printf("Disconnecting all %d devices...", len(macsToDisconnect))

	var firstErr error
	for _, mac := range macsToDisconnect {
		// MODIFIED: Pass empty device name since we are not blacklisting
		if err := disconnectDevice(mac, "", false); err != nil {
			log.Printf("Error disconnecting %s from AC during shutdown: %v\n", mac, err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}

	log.Println("Finished disconnectAllDevices process.")
	return firstErr
}

func fetchConnectedDevicesFromHub(hubMac string) ([]map[string]interface{}, error) {
	accessTokenMutex.RLock()
	currentAccessToken := accessToken
	accessTokenMutex.RUnlock()
	configMutex.RLock()
	currentACDomain := acDomain
	configMutex.RUnlock()

	apiURL := fmt.Sprintf("%s/api/gap/nodes?connection_state=connected&mac=%s", currentACDomain, hubMac)
	req, _ := http.NewRequest("GET", apiURL, nil)
	req.Header.Set("Authorization", "Bearer "+currentAccessToken)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch from hub %s failed: %w", hubMac, err)
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusInternalServerError && strings.Contains(strings.ToLower(string(bodyBytes)), "gateway is offline") {
			return nil, fmt.Errorf("gateway for hub %s offline", hubMac)
		}
		if resp.StatusCode == http.StatusNotFound {
			log.Printf("No connected devices found on hub %s (404 response), treating as empty list.", hubMac)
			return []map[string]interface{}{}, nil
		}
		return nil, fmt.Errorf("fetch from hub %s status %d: %s", hubMac, resp.StatusCode, string(bodyBytes))
	}

	var result struct {
		Nodes []map[string]interface{} `json:"nodes"`
	}
	if err := json.Unmarshal(bodyBytes, &result); err != nil {
		if strings.Contains(err.Error(), "cannot unmarshal object") && strings.Contains(string(bodyBytes), `"nodes":{}`) {
			return []map[string]interface{}{}, nil
		}
		return nil, fmt.Errorf("unmarshal hub %s response failed: %w. Body: %s", hubMac, err, string(bodyBytes))
	}

	if result.Nodes == nil {
		return []map[string]interface{}{}, nil
	}
	return result.Nodes, nil
}
func openConnectionState() error {
	activeAPsMutex.RLock()
	currentAPList := make([]string, 0, len(activeAPs))
	for mac := range activeAPs {
		currentAPList = append(currentAPList, mac)
	}
	activeAPsMutex.RUnlock()

	if len(currentAPList) == 0 {
		log.Println("openConnectionState: No active APs to monitor.")
		return nil
	}

	accessTokenMutex.RLock()
	currentAccessToken := accessToken
	accessTokenMutex.RUnlock()
	configMutex.RLock()
	currentACDomain := acDomain
	configMutex.RUnlock()
	apiURL := currentACDomain + CONNECTION_STATE_OPEN_PATH
	payload := map[string][]string{"aps": currentAPList}
	payloadBytes, _ := json.Marshal(payload)

	req, _ := http.NewRequest("POST", apiURL, bytes.NewReader(payloadBytes))
	req.Header.Set("Authorization", "Bearer "+currentAccessToken)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("execute open conn state failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("open conn state failed status %d: %s", resp.StatusCode, string(respBody))
	}
	log.Println("Successfully opened connection state monitoring for APs:", currentAPList)
	return nil
}

func openAPStateMonitoring() error {
	accessTokenMutex.RLock()
	currentAccessToken := accessToken
	accessTokenMutex.RUnlock()
	configMutex.RLock()
	currentACDomain := acDomain
	configMutex.RUnlock()

	apiURL := currentACDomain + AP_STATE_PATH
	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return fmt.Errorf("create open ap-state request failed: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+currentAccessToken)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("execute open ap-state request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("open ap-state request failed status %d: %s", resp.StatusCode, string(respBody))
	}

	log.Println("Successfully opened AP state monitoring.")
	return nil
}

func closeAPStateMonitoring() error {
	accessTokenMutex.RLock()
	currentAccessToken := accessToken
	accessTokenMutex.RUnlock()
	configMutex.RLock()
	currentACDomain := acDomain
	configMutex.RUnlock()

	apiURL := currentACDomain + AP_STATE_CLOSE_PATH
	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return fmt.Errorf("create close ap-state request failed: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+currentAccessToken)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("execute close ap-state request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("close ap-state request failed status %d: %s", resp.StatusCode, string(respBody))
	}

	log.Println("Successfully closed AP state monitoring.")
	return nil
}

// --- Housekeeping (Periodic Tasks) ---
func housekeeping() error {
	log.Println("--- Housekeeping Started ---")
	latestHubState := make(map[string]ConnectedDevice)
	var fetchHubErrors []error
	activeAPsMutex.RLock()
	currentAPList := make([]string, 0, len(activeAPs))
	for mac := range activeAPs {
		currentAPList = append(currentAPList, mac)
	}
	activeAPsMutex.RUnlock()

	for _, hubMac := range currentAPList {
		hubConnectedDevicesData, err := fetchConnectedDevicesFromHub(hubMac)
		if err != nil {
			log.Printf("Housekeeping: Error fetching from Cassia hub %s: %v", hubMac, err)
			fetchHubErrors = append(fetchHubErrors, err)
			continue
		}
		for _, deviceData := range hubConnectedDevicesData {
			mac, macOk := deviceData["id"].(string)
			if !macOk || mac == "" {
				if bdaddrsMap, bdaddrsOk := deviceData["bdaddrs"].(map[string]interface{}); bdaddrsOk {
					mac, macOk = bdaddrsMap["bdaddr"].(string)
				}
			}
			status, statusOk := deviceData["connectionState"].(string)
			nameFromHub, nameOk := deviceData["name"].(string)
			if !macOk || mac == "" || !statusOk || status != "connected" {
				continue
			}
			apFromHub := hubMac
			if nameOk && nameFromHub != "" && nameFromHub != "(unknown)" {
				updateStableName(mac, nameFromHub)
			}
			stableName := getStableName(mac)
			deviceType := getStableDeviceType(mac, stableName)
			latestHubState[mac] = ConnectedDevice{
				Name:     stableName,
				MACID:    mac,
				AP:       apFromHub,
				Status:   status,
				Type:     deviceType,
				LastSeen: time.Now(),
			}
		}
	}
	connectedDevicesMutex.Lock()
	for mac, hubDevice := range latestHubState {
		existingDevice, exists := connectedDevices[mac]
		if !exists || existingDevice.AP != hubDevice.AP {
			log.Printf("Housekeeping: Device %s (re)detected on AP %s.", mac, hubDevice.AP)
			hubDevice.NotificationSent = false
			hubDevice.LastPacketActivityAt = time.Now()
			connectedDevices[mac] = hubDevice
			if hubDevice.Type != "unknown" {
				go initiateNotification(mac, hubDevice.Type, hubDevice.AP)
			}
		} else {
			hubDevice.NotificationSent = existingDevice.NotificationSent
			hubDevice.LastPacketActivityAt = existingDevice.LastPacketActivityAt
			hubDevice.StalledCheckCounter = existingDevice.StalledCheckCounter
			hubDevice.LastSeen = time.Now()
			connectedDevices[mac] = hubDevice
			if hubDevice.Type != "unknown" && !hubDevice.NotificationSent {
				go initiateNotification(mac, hubDevice.Type, hubDevice.AP)
			}
		}
	}
	currentLocalMACs := make([]string, 0, len(connectedDevices))
	for mac := range connectedDevices {
		currentLocalMACs = append(currentLocalMACs, mac)
	}
	for _, mac := range currentLocalMACs {
		if _, foundOnHub := latestHubState[mac]; !foundOnHub {
			staleDevice := connectedDevices[mac]
			if staleDevice.SuspectedStale {
				log.Printf("Housekeeping: Device %s confirmed stale. Removing.", mac)
				delete(connectedDevices, mac)
				// *** MODIFICATION START ***
				// If you want to disconnect but NOT blacklist these devices automatically
				go disconnectDevice(mac, "", false) // Disconnect without blacklisting
				// *** MODIFICATION END ***
			} else {
				log.Printf("Housekeeping: Device %s now suspected stale.", mac)
				staleDevice.SuspectedStale = true
				connectedDevices[mac] = staleDevice
			}
		} else {
			goodDevice := connectedDevices[mac]
			if goodDevice.SuspectedStale {
				goodDevice.SuspectedStale = false
				connectedDevices[mac] = goodDevice
			}
		}
	}
	connectedDevicesMutex.Unlock()

	// Health Check with Node.js Receiver
	configMutex.RLock()
	currentReceiverMode := receiverMode
	configMutex.RUnlock()

	// Health check logic for both perAP and local modes
	devicesToCheck := make(map[string]string) // mac -> apMac
	connectedDevicesMutex.RLock()
	for _, device := range connectedDevices {
		if device.NotificationSent {
			devicesToCheck[device.MACID] = device.AP
		}
	}
	connectedDevicesMutex.RUnlock()

	if currentReceiverMode == "local" {
		// Single check for all devices
		baseURL, err := getNodeJSBaseURL("") // hubMac is ignored for local
		if err != nil {
			log.Printf("Housekeeping Health Check (local): %v", err)
		} else {
			checkPacketCountsForURL(baseURL, devicesToCheck)
		}
	} else { // perAP mode
		// Group devices by their AP
		apToDevicesMap := make(map[string]map[string]string)
		for mac, apMac := range devicesToCheck {
			if _, ok := apToDevicesMap[apMac]; !ok {
				apToDevicesMap[apMac] = make(map[string]string)
			}
			apToDevicesMap[apMac][mac] = apMac
		}
		// Check each AP's Node.js instance
		for apMac, devicesOnAP := range apToDevicesMap {
			baseURL, err := getNodeJSBaseURL(apMac)
			if err != nil {
				log.Printf("Housekeeping Health Check (per-AP): Cannot get URL for AP %s: %v", apMac, err)
				continue
			}
			go checkPacketCountsForURL(baseURL, devicesOnAP)
		}
	}

	// Disconnect Logic (without blacklisting from here)
	macsToDisconnectDetails := make(map[string]string)
	deviceInactivitySettingsMutex.RLock()
	connectedDevicesMutex.RLock()
	deviceBlacklistMutex.RLock()
	for mac, device := range connectedDevices {
		if _, isBlacklisted := deviceBlacklist[mac]; isBlacklisted {
			if device.Status == "connected" {
				macsToDisconnectDetails[mac] = "already_blacklisted_and_connected"
			}
			continue
		}

		if device.Type == "unknown" {
			macsToDisconnectDetails[mac] = "unknown_type"
			continue
		}
		if device.NotificationSent && device.StalledCheckCounter >= STALLED_CONNECTION_THRESHOLD {
			log.Printf("Housekeeping: Device %s (AP: %s) is stalled. Scheduling disconnect.", mac, device.AP)
			macsToDisconnectDetails[mac] = "stalled"
			continue
		}
		if device.NotificationSent {
			timeoutDuration, typeHasSetting := deviceInactivitySettings[device.Type]
			if typeHasSetting && timeoutDuration > 0 {
				if !device.LastPacketActivityAt.IsZero() && time.Since(device.LastPacketActivityAt) > timeoutDuration {
					macsToDisconnectDetails[mac] = "inactive"
				}
			}
		}
	}
	deviceBlacklistMutex.RUnlock()
	connectedDevicesMutex.RUnlock()
	deviceInactivitySettingsMutex.RUnlock()

	for mac, reason := range macsToDisconnectDetails {
		log.Printf("Housekeeping: Finalizing disconnect for device %s due to: %s (NO BLACKLISTING FROM HOUSEKEEPING)", mac, reason)
		go disconnectDevice(mac, "", false) // CHANGE THIS LINE from 'true' to 'false'
	}

	// Cleanup Scanned Devices
	scannedDevicesMutex.Lock()
	displayDevicesMapMutex.Lock()
	for macID, entry := range scannedDevices {
		if time.Since(entry.LastSeen) > SCANNED_DEVICE_LIFETIME {
			delete(scannedDevices, macID)
			delete(displayDevicesMap, macID)
		}
	}
	currentDisplayableScanned := make(map[string]ScannedDeviceDisplay)
	for macID, entry := range scannedDevices {
		deviceBlacklistMutex.RLock()
		_, isBlacklisted := deviceBlacklist[macID]
		deviceBlacklistMutex.RUnlock()
		if isBlacklisted {
			delete(displayDevicesMap, macID)
			continue
		}
		nameFromScan, _ := entry.Data["name"].(string)
		updateStableName(macID, nameFromScan)
		stableName := getStableName(macID)
		deviceType := getStableDeviceType(macID, stableName)
		var shouldDisplay bool
		if len(targetNamePrefixes) == 0 {
			shouldDisplay = true
		} else {
			for _, prefix := range targetNamePrefixes {
				if strings.HasPrefix(stableName, prefix) {
					shouldDisplay = true
					break
				}
			}
		}
		if shouldDisplay {
			rssiVal, _ := entry.Data["rssi"].(float64)
			apName, _ := entry.Data["ap"].(string)
			currentDisplayableScanned[macID] = ScannedDeviceDisplay{
				MACID:      macID,
				Name:       stableName,
				RSSI:       int(rssiVal),
				AP:         apName,
				DeviceType: deviceType,
				RawData:    entry.Data,
			}
		}
	}
	displayDevicesMap = currentDisplayableScanned
	displayDevicesMapMutex.Unlock()
	scannedDevicesMutex.Unlock()

	log.Println("--- Housekeeping Finished ---")
	return nil
}

// Helper for housekeeping health check
func checkPacketCountsForURL(baseURL string, devicesToCheck map[string]string) {
	apiURL := baseURL + "/packet-counts"
	resp, err := http.Get(apiURL)
	if err != nil {
		log.Printf("Housekeeping Health Check: Failed to get packet counts from %s: %v", apiURL, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		var packetCounts map[string]int
		if err := json.NewDecoder(resp.Body).Decode(&packetCounts); err == nil {
			connectedDevicesMutex.Lock()
			for mac := range devicesToCheck {
				device, exists := connectedDevices[mac]
				if !exists {
					continue
				}
				count, hasCount := packetCounts[mac]
				if hasCount && count > 0 {
					device.StalledCheckCounter = 0
				} else if hasCount {
					device.StalledCheckCounter++
					log.Printf("Housekeeping Health Check: Device %s on %s has 0 packets. Stall count: %d", mac, baseURL, device.StalledCheckCounter)
				}
				connectedDevices[mac] = device
			}
			connectedDevicesMutex.Unlock()
		}
	} else {
		log.Printf("Housekeeping Health Check: Node.js receiver at %s returned non-OK status %d.", baseURL, resp.StatusCode)
	}
}

// --- SSE Event Handler Goroutine ---
func handleSSEEvents() {
	var reader *bufio.Reader
	var err error

	defer func() {
		if sseBody != nil {
			sseBody.Close()
		}
	}()

	for {
		servicesMutex.Lock()
		if !servicesStarted {
			servicesMutex.Unlock()
			log.Printf("SSE handler: Services stopped, exiting.")
			return
		}
		servicesMutex.Unlock()

		sseBody, reader, err = setupSSEConnection(SSE_PATH)
		if err != nil {
			log.Printf("Main SSE setup failed: %v. Retrying...", err)
			time.Sleep(15 * time.Second)
			continue
		}

		log.Println("Main SSE connection established.")
		// Use a non-blocking send to avoid getting stuck if runServices has timed out
		select {
		case sseReadyChan <- true: // Signal main service start flow
		default:
			log.Println("Warning: sseReadyChan was full or no listener. Continuing.")
		}

		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				// CORRECTED: Check if services were stopped before logging an error.
				servicesMutex.Lock()
				if !servicesStarted {
					servicesMutex.Unlock()
					log.Println("SSE stream closed due to service stop.")
					return // Exit the goroutine cleanly.
				}
				servicesMutex.Unlock()

				log.Printf("Error reading from main SSE stream: %v. Re-establishing...", err)
				sseBody.Close()
				time.Sleep(RETRY_DELAY)
				break // Break inner loop to reconnect
			}

			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "data:") {
				eventData := strings.TrimPrefix(line, "data:")
				go processMainSSEEvent(eventData) // Process events in parallel
			}
		}
	}
}

func processMainSSEEvent(eventData string) {
	var sseEvent map[string]interface{}
	if err := json.Unmarshal([]byte(eventData), &sseEvent); err != nil {
		log.Printf("Error unmarshaling main SSE event data: %v. Data: %s\n", err, eventData)
		return
	}
	dataType, _ := sseEvent["dataType"].(string)
	switch dataType {
	case "scan":
		var mac string
		if bdaddrs, ok := sseEvent["bdaddrs"].([]interface{}); ok && len(bdaddrs) > 0 {
			if bdaddrMap, ok := bdaddrs[0].(map[string]interface{}); ok {
				if bdaddr, ok := bdaddrMap["bdaddr"].(string); ok {
					mac = bdaddr
				}
			}
		}
		name, _ := sseEvent["name"].(string)
		_, apOk := sseEvent["ap"].(string)

		if mac != "" && apOk {
			updateStableName(mac, name)
			stableName := getStableName(mac)

			deviceBlacklistMutex.RLock()
			_, isBlacklisted := deviceBlacklist[mac]
			deviceBlacklistMutex.RUnlock()
			if isBlacklisted {
				return
			}

			// Acquire locks in consistent order: connectedDevicesMutex (RLock) then connectingQueueMutex (Lock)
			connectedDevicesMutex.RLock() // RLock for checking isConnected
			connectingQueueMutex.Lock()   // Lock for connectingQueue

			_, isConnecting := connectingQueue[mac]

			isTargetDevice := false
			if stableName != "(unknown)" {
				for _, prefix := range targetNamePrefixes {
					if strings.HasPrefix(stableName, prefix) {
						isTargetDevice = true
						break
					}
				}
			}

			_, isConnected := connectedDevices[mac] // Check while holding connectedDevicesMutex.RLock()

			if !isConnecting && isTargetDevice && !isConnected {
				connectingQueue[mac] = struct{}{}
				// Unlock the mutexes in reverse order before launching goroutine
				connectingQueueMutex.Unlock()
				connectedDevicesMutex.RUnlock()

				activeAPsMutex.RLock()
				apsToTry := make([]string, 0, len(activeAPs))
				for apMac := range activeAPs {
					apsToTry = append(apsToTry, apMac)
				}
				activeAPsMutex.RUnlock()

				go func(deviceMAC string, aps []string) {
					if err := connectDeviceAPI(deviceMAC, aps); err != nil {
						log.Printf("Error connecting device %s: %v", deviceMAC, err)
					}
					connectingQueueMutex.Lock() // Re-acquire to delete
					delete(connectingQueue, deviceMAC)
					connectingQueueMutex.Unlock()
				}(mac, apsToTry)
			} else {
				// If not connecting, or already connected/not a target device, release locks immediately
				connectingQueueMutex.Unlock()
				connectedDevicesMutex.RUnlock()
			}

			scannedDevicesMutex.Lock()
			scannedDevices[mac] = ScannedDeviceEntry{Data: sseEvent, LastSeen: time.Now()}
			scannedDevicesMutex.Unlock()
		}

	case "connection_state":
		mac, macOk := sseEvent["handle"].(string)
		state, stateOk := sseEvent["connectionState"].(string)
		ap, apOk := sseEvent["ap"].(string)

		if macOk && apOk && stateOk {
			stableName := getStableName(mac)
			addConnectionLog(mac, stableName, ap, state)

			connectedDevicesMutex.Lock()
			currentDevice, exists := connectedDevices[mac]
			if state == "connected" {
				if !exists || currentDevice.AP != ap || currentDevice.Status != "connected" {
					deviceType := getStableDeviceType(mac, stableName)
					connectedDevices[mac] = ConnectedDevice{
						Name:                 stableName,
						MACID:                mac,
						AP:                   ap,
						Status:               "connected",
						Type:                 deviceType,
						LastSeen:             time.Now(),
						NotificationSent:     false,
						LastPacketActivityAt: time.Now(),
						SuspectedStale:       false,
						StalledCheckCounter:  0,
					}
					// This part correctly acquires connectingQueueMutex AFTER connectedDevicesMutex
					connectingQueueMutex.Lock()
					delete(connectingQueue, mac)
					connectingQueueMutex.Unlock()
					if deviceType != "unknown" {
						go initiateNotification(mac, deviceType, ap)
					}
				} else {
					currentDevice.LastSeen = time.Now()
					currentDevice.LastPacketActivityAt = time.Now()
					currentDevice.StalledCheckCounter = 0
					connectedDevices[mac] = currentDevice
				}
			} else if state == "disconnected" {
				if exists {
					go stopNotification(mac, currentDevice.AP)
					// This part correctly acquires connectingQueueMutex AFTER connectedDevicesMutex
					connectingQueueMutex.Lock()
					delete(connectingQueue, mac)
					connectingQueueMutex.Unlock()
					delete(connectedDevices, mac)
				}
			}
			connectedDevicesMutex.Unlock()
		}

	case "notification":
		mac, macOk := sseEvent["device"].(string)
		if !macOk || mac == "" {
			return
		}
		connectedDevicesMutex.Lock()
		if dev, exists := connectedDevices[mac]; exists {
			dev.LastPacketActivityAt = time.Now()
			dev.StalledCheckCounter = 0
			connectedDevices[mac] = dev
		}
		connectedDevicesMutex.Unlock()

	case "state":
		processAPStateEvent(eventData)
	}
}

func processAPStateEvent(eventData string) {
	var sseEvent map[string]interface{}
	if err := json.Unmarshal([]byte(eventData), &sseEvent); err != nil {
		log.Printf("Error unmarshaling ap_state event data: %v. Data: %s\n", err, eventData)
		return
	}

	apsData, ok := sseEvent["aps"].(map[string]interface{})
	if !ok {
		return
	}

	newActiveAPs := make(map[string]APInfo)
	newAPList := []string{}
	newAPNodeJSMapping := make(map[string]string)

	for mac, apDataInterface := range apsData {
		apData, ok := apDataInterface.(map[string]interface{})
		if !ok {
			continue
		}
		status, _ := apData["status"].(string)
		localIP, _ := apData["localip"].(string)

		if status == "online" {
			newActiveAPs[mac] = APInfo{
				MAC:     mac,
				LocalIP: localIP,
				Status:  status,
			}
			newAPList = append(newAPList, mac)
			if localIP != "" {
				newAPNodeJSMapping[mac] = localIP
			}
		}
	}

	activeAPsMutex.Lock()
	apListForConnectionState = newAPList
	activeAPs = newActiveAPs
	activeAPsMutex.Unlock()

	apNodeJSMappingMutex.Lock()
	apNodeJSMapping = newAPNodeJSMapping
	apNodeJSMappingMutex.Unlock()

	log.Printf("Dynamically updated AP list: %d online APs found. Current list: %v", len(newAPList), newAPList)
}

// --- HTTP API Handlers ---
type FullConfigPayload struct {
	ACDomain         string `json:"acDomain"`
	JSONEndpoint     string `json:"jsonEndpoint"`
	ReceiverMode     string `json:"receiverMode"`
	LocalReceiverURL string `json:"localReceiverURL"`
	DevicePrefixes   string `json:"devicePrefixes"`
	InactivityRules  string `json:"inactivityRules"`
}

func configHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Only POST allowed", http.StatusMethodNotAllowed)
		return
	}
	var payload FullConfigPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "Invalid payload: "+err.Error(), http.StatusBadRequest)
		return
	}
	configMutex.Lock()
	if payload.ACDomain != "" {
		acDomain = payload.ACDomain
	}
	if payload.JSONEndpoint != "" {
		jsonNotifyEndpoint = payload.JSONEndpoint
	}
	if payload.ReceiverMode == "perAP" || payload.ReceiverMode == "local" {
		receiverMode = payload.ReceiverMode
	}
	if receiverMode == "local" && payload.LocalReceiverURL != "" {
		localReceiverURL = payload.LocalReceiverURL
	}
	configMutex.Unlock()

	if payload.DevicePrefixes != "" {
		newPrefixes := strings.Split(payload.DevicePrefixes, ",")
		newMappings := make(map[string]string)
		var updatedPrefixes []string
		for _, p := range newPrefixes {
			trimmed := strings.TrimSpace(p)
			if trimmed != "" {
				updatedPrefixes = append(updatedPrefixes, trimmed)
				newMappings[trimmed] = trimmed
			}
		}
		targetNamePrefixes = updatedPrefixes
		deviceTypeMappings = newMappings
		log.Printf("Updated target prefixes to: %v", targetNamePrefixes)
	}

	if payload.InactivityRules != "" {
		newSettings := make(map[string]time.Duration)
		rules := strings.Split(payload.InactivityRules, ",")
		for _, rule := range rules {
			parts := strings.Split(strings.TrimSpace(rule), ":")
			if len(parts) == 2 {
				deviceType := strings.TrimSpace(parts[0])
				timeoutMinutes, err := strconv.Atoi(strings.TrimSpace(parts[1]))
				if err == nil && timeoutMinutes > 0 {
					newSettings[deviceType] = time.Duration(timeoutMinutes) * time.Minute
				}
			}
		}
		deviceInactivitySettingsMutex.Lock()
		deviceInactivitySettings = newSettings
		deviceInactivitySettingsMutex.Unlock()
		log.Println("Updated device inactivity settings.")
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"message": "Configuration updated successfully."})
}

var tokenRefreshTicker *time.Ticker
var housekeepingTicker *time.Ticker

// --- Service Start/Stop Logic ---
func runServices() {
	servicesMutex.Lock()
	if servicesStarted {
		servicesMutex.Unlock()
		log.Println("runServices called, but services are already running.")
		return
	}
	servicesStarted = true
	servicesMutex.Unlock()

	log.Println("--- Services Starting ---")

	if err := loadDeviceTypes(DEVICE_CSV_PATH); err != nil {
		log.Printf("Warning: Failed to load device types: %v", err)
	}
	if err := loadAPConfig(AP_CONFIG_CSV_PATH); err != nil {
		log.Printf("Warning: Failed to load AP config: %v", err)
	}
	if err := loadDeviceInactivitySettings(DEVICE_INACTIVITY_CONFIG_PATH); err != nil {
		log.Printf("Warning: Failed to load inactivity settings: %v", err)
	}

	token, err := getAuthToken()
	if err != nil {
		log.Printf("Fatal: Could not get initial auth token: %v.", err)
		servicesMutex.Lock()
		servicesStarted = false
		servicesMutex.Unlock()
		return
	}
	accessTokenMutex.Lock()
	accessToken = token.AccessToken
	accessTokenMutex.Unlock()
	log.Println("Initial backend authentication successful.")

	go handleSSEEvents()

	log.Println("Waiting for main SSE connection...")
	select {
	case <-sseReadyChan:
		log.Println("Main SSE connection established.")
	case <-time.After(20 * time.Second):
		log.Println("Timed out waiting for main SSE connection. Aborting.")
		directStopServices()
		return
	}

	if err := openAPStateMonitoring(); err != nil {
		log.Printf("Error opening AP state monitoring: %v", err)
	}
	if err := openConnectionState(); err != nil {
		log.Printf("Error opening connection state monitoring: %v", err)
	}
	if err := scanBLEDevices(); err != nil {
		log.Printf("Error initiating initial BLE scan: %v\n", err)
	}

	if tokenRefreshTicker != nil {
		tokenRefreshTicker.Stop()
	}
	tokenRefreshTicker = time.NewTicker(TOKEN_REFRESH_INTERVAL)
	go func() {
		for range tokenRefreshTicker.C {
			if !servicesStarted {
				return
			}
			refreshAccessToken()
		}
	}()

	if housekeepingTicker != nil {
		housekeepingTicker.Stop()
	}
	housekeepingTicker = time.NewTicker(HEARTBEAT_INTERVAL)
	go func() {
		time.Sleep(10 * time.Second)
		for range housekeepingTicker.C {
			if !servicesStarted {
				return
			}
			if err := housekeeping(); err != nil {
				log.Printf("Error during housekeeping: %v\n", err)
			}
		}
	}()
	log.Println("--- Services Started Successfully ---")
}

func startServicesHandler(w http.ResponseWriter, r *http.Request) {
	go runServices()
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{"message": "Service start initiated."})
}

func stopServicesHandler(w http.ResponseWriter, r *http.Request) {
	directStopServices()
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"message": "Services stopped successfully"})
}

func getScannedDevicesHandler(w http.ResponseWriter, r *http.Request) {
	servicesMutex.Lock()
	active := servicesStarted
	servicesMutex.Unlock()
	if !active {
		http.Error(w, "Services not running", http.StatusServiceUnavailable)
		return
	}

	displayDevicesMapMutex.RLock()
	currentDisplayDevices := make([]ScannedDeviceDisplay, 0, len(displayDevicesMap))
	for _, device := range displayDevicesMap {
		currentDisplayDevices = append(currentDisplayDevices, device)
	}
	displayDevicesMapMutex.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(currentDisplayDevices)
}

func getConnectedDevicesHandler(w http.ResponseWriter, r *http.Request) {
	servicesMutex.Lock()
	active := servicesStarted
	servicesMutex.Unlock()
	if !active {
		http.Error(w, "Services not running", http.StatusServiceUnavailable)
		return
	}

	connectedDevicesMutex.RLock()
	currentConnected := make([]ConnectedDevice, 0, len(connectedDevices))
	for _, device := range connectedDevices {
		currentConnected = append(currentConnected, device)
	}
	connectedDevicesMutex.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(currentConnected)
}

// MODIFIED: DeviceActionPayload now includes DeviceName
type DeviceActionPayload struct {
	MAC        string `json:"mac"`
	AP         string `json:"ap,omitempty"`
	DeviceName string `json:"deviceName,omitempty"`
}

func disconnectAndBlacklistHandler(w http.ResponseWriter, r *http.Request) {
	if !servicesStarted {
		http.Error(w, "Services not running", http.StatusServiceUnavailable)
		return
	}
	var payload DeviceActionPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}
	if payload.MAC == "" {
		http.Error(w, "MAC is required", http.StatusBadRequest)
		return
	}
	log.Printf("API: Disconnect and blacklist request for MAC: %s", payload.MAC)
	// MODIFIED: Pass the device name to the disconnect function
	go disconnectDevice(payload.MAC, payload.DeviceName, true)
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{"message": "Disconnection and blacklisting initiated for " + payload.MAC})
}

func disconnectOnlyHandler(w http.ResponseWriter, r *http.Request) {
	if !servicesStarted {
		http.Error(w, "Services not running", http.StatusServiceUnavailable)
		return
	}
	var payload DeviceActionPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}
	if payload.MAC == "" {
		http.Error(w, "MAC is required", http.StatusBadRequest)
		return
	}
	log.Printf("API: Disconnect ONLY request for MAC: %s", payload.MAC)
	// MODIFIED: Pass an empty device name since we are not blacklisting
	go disconnectDevice(payload.MAC, "", false)
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{"message": "Disconnection initiated for " + payload.MAC})
}

func forceConnectHandler(w http.ResponseWriter, r *http.Request) {
	if !servicesStarted {
		http.Error(w, "Services not running", http.StatusServiceUnavailable)
		return
	}
	var payload DeviceActionPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}
	if payload.MAC == "" || payload.AP == "" {
		http.Error(w, "MAC and target AP are required", http.StatusBadRequest)
		return
	}

	log.Printf("API: Force connect request for device %s to AP %s", payload.MAC, payload.AP)

	deviceBlacklistMutex.Lock()
	delete(deviceBlacklist, payload.MAC)
	deviceBlacklistMutex.Unlock()
	log.Printf("Device %s removed from blacklist for force connect.", payload.MAC)

	go func() {
		if err := connectDeviceAPI(payload.MAC, []string{payload.AP}); err != nil {
			log.Printf("Error during force connect for %s to AP %s: %v", payload.MAC, payload.AP, err)
		} else {
			log.Printf("Force connect attempt for %s to AP %s initiated.", payload.MAC, payload.AP)
		}
	}()

	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{"message": "Force connect initiated for " + payload.MAC + " to AP " + payload.AP})
}

func notifyDeviceHandler(w http.ResponseWriter, r *http.Request) {
	if !servicesStarted {
		http.Error(w, "Services not running", http.StatusServiceUnavailable)
		return
	}
	var payload DeviceActionPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "Invalid request payload", http.StatusBadRequest)
		return
	}
	if payload.MAC == "" {
		http.Error(w, "MAC is required", http.StatusBadRequest)
		return
	}

	connectedDevicesMutex.RLock()
	device, exists := connectedDevices[payload.MAC]
	connectedDevicesMutex.RUnlock()

	if !exists {
		http.Error(w, "Device not found", http.StatusNotFound)
		return
	}
	if device.Type == "unknown" {
		http.Error(w, "Cannot notify device of unknown type", http.StatusBadRequest)
		return
	}

	log.Printf("API: Manual notify request for MAC: %s", device.MACID)
	go initiateNotification(device.MACID, device.Type, device.AP)
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{"message": "Notification attempt initiated for " + device.MACID})
}

func getBlacklistedDevicesHandler(w http.ResponseWriter, r *http.Request) {
	if !servicesStarted {
		http.Error(w, "Services not running", http.StatusServiceUnavailable)
		return
	}
	deviceBlacklistMutex.RLock()
	defer deviceBlacklistMutex.RUnlock()
	blacklisted := make([]BlacklistedDeviceEntry, 0, len(deviceBlacklist))
	// MODIFIED: Iterate over the map values (the structs)
	for _, entry := range deviceBlacklist {
		blacklisted = append(blacklisted, entry)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(blacklisted)
}

func unblacklistDeviceHandler(w http.ResponseWriter, r *http.Request) {
	if !servicesStarted {
		http.Error(w, "Services not running", http.StatusServiceUnavailable)
		return
	}
	var payload DeviceActionPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}
	if payload.MAC == "" {
		http.Error(w, "MAC is required", http.StatusBadRequest)
		return
	}

	deviceBlacklistMutex.Lock()
	delete(deviceBlacklist, payload.MAC)
	deviceBlacklistMutex.Unlock()
	log.Printf("Device %s removed from blacklist.", payload.MAC)
	go scanBLEDevices()
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"message": "Device " + payload.MAC + " removed from blacklist."})
}

func getAPsHandler(w http.ResponseWriter, r *http.Request) {
	if !servicesStarted {
		http.Error(w, "Services not running", http.StatusServiceUnavailable)
		return
	}
	activeAPsMutex.RLock()
	defer activeAPsMutex.RUnlock()
	apList := make([]APInfo, 0, len(activeAPs))
	for _, ap := range activeAPs {
		apList = append(apList, ap)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(apList)
}

func getConnectionLogsHandler(w http.ResponseWriter, r *http.Request) {
	if !servicesStarted {
		http.Error(w, "Services not running", http.StatusServiceUnavailable)
		return
	}
	connectionLogsMutex.RLock()
	logsCopy := make([]ConnectionLogEntry, len(connectionLogs))
	copy(logsCopy, connectionLogs)
	connectionLogsMutex.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(logsCopy)
}

func directStopServices() {
	servicesMutex.Lock()
	if !servicesStarted {
		servicesMutex.Unlock()
		return
	}
	log.Println("--- Services Stopping ---")
	servicesStarted = false
	servicesMutex.Unlock()

	if tokenRefreshTicker != nil {
		tokenRefreshTicker.Stop()
	}
	if housekeepingTicker != nil {
		housekeepingTicker.Stop()
	}

	if sseBody != nil {
		sseBody.Close()
	}

	if err := closeAPStateMonitoring(); err != nil {
		log.Printf("Error closing AP state monitoring: %v", err)
	}

	if err := disconnectAllDevices(); err != nil {
		log.Printf("Error during disconnectAllDevices: %v", err)
	}

	connectedDevicesMutex.Lock()
	connectedDevices = make(map[string]ConnectedDevice)
	connectedDevicesMutex.Unlock()
	scannedDevicesMutex.Lock()
	scannedDevices = make(map[string]ScannedDeviceEntry)
	scannedDevicesMutex.Unlock()
	displayDevicesMapMutex.Lock()
	displayDevicesMap = make(map[string]ScannedDeviceDisplay)
	displayDevicesMapMutex.Unlock()
	deviceNameCacheMutex.Lock()
	deviceNameCache = make(map[string]string)
	deviceNameCacheMutex.Unlock()
	deviceMacToTypeCacheMutex.Lock()
	deviceMacToTypeCache = make(map[string]string)
	deviceMacToTypeCacheMutex.Unlock()
	deviceBlacklistMutex.Lock()
	// MODIFIED: Re-initialize the correct type for the blacklist
	deviceBlacklist = make(map[string]BlacklistedDeviceEntry)
	deviceBlacklistMutex.Unlock()
	connectionLogsMutex.Lock()
	connectionLogs = make([]ConnectionLogEntry, 0, MAX_LOG_ENTRIES)
	connectionLogsMutex.Unlock()
	activeAPsMutex.Lock()
	activeAPs = make(map[string]APInfo)
	activeAPsMutex.Unlock()

	log.Println("--- Services Stopped ---")
}

// init function runs automatically when the program starts
func init() {
	// Read AC_DOMAIN from environment variable, fallback to default if not set
	if os.Getenv("AC_DOMAIN") != "" {
		acDomain = os.Getenv("AC_DOMAIN")
	} else {
		acDomain = "http://192.168.0.3" // Your fallback default
	}
	log.Printf("AC_DOMAIN set to: %s", acDomain)

	// You might also want to do the same for jsonNotifyEndpoint if it can be configured via ENV
	if os.Getenv("JSON_NOTIFY_ENDPOINT") != "" {
		jsonNotifyEndpoint = os.Getenv("JSON_NOTIFY_ENDPOINT")
	} else {
		jsonNotifyEndpoint = "https://qa-testing-api-gw.lifesigns.us/api/v1/public/external-device" // Your fallback default
	}
	log.Printf("JSON_NOTIFY_ENDPOINT set to: %s", jsonNotifyEndpoint) // <--- ADD THIS LINE FOR LOGGING

	// Initialize log settings (keep your existing log setup here)
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	// ... any other init logic you might have
}

// getConfigHandler serves the current application configuration to the UI
func getConfigHandler(w http.ResponseWriter, r *http.Request) {
	configMutex.RLock()
	defer configMutex.RUnlock()

	config := struct {
		ACDomain           string `json:"acDomain"`
		JsonNotifyEndpoint string `json:"jsonNotifyEndpoint"`
	}{
		ACDomain:           acDomain,
		JsonNotifyEndpoint: jsonNotifyEndpoint,
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(config); err != nil {
		log.Printf("Error encoding config: %v", err)
		http.Error(w, "Error encoding config", http.StatusInternalServerError)
	}
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile | log.Lmicroseconds)

	configMutex.Lock()
	if receiverMode == "" {
		receiverMode = DEFAULT_RECEIVER_MODE
	}
	configMutex.Unlock()

	fs := http.FileServer(http.Dir("./frontend"))
	http.Handle("/", fs)

	http.HandleFunc("/api/get-config", getConfigHandler)
	http.HandleFunc("/api/config", configHandler)
	http.HandleFunc("/api/start-services", startServicesHandler)
	http.HandleFunc("/api/stop-services", stopServicesHandler)
	http.HandleFunc("/api/scanned-devices", getScannedDevicesHandler)
	http.HandleFunc("/api/connected-devices", getConnectedDevicesHandler)
	http.HandleFunc("/api/disconnect-device", disconnectAndBlacklistHandler)
	http.HandleFunc("/api/disconnect-device-only", disconnectOnlyHandler)
	http.HandleFunc("/api/force-connect-device", forceConnectHandler)
	http.HandleFunc("/api/notify-device", notifyDeviceHandler)
	http.HandleFunc("/api/blacklisted-devices", getBlacklistedDevicesHandler)
	http.HandleFunc("/api/unblacklist-device", unblacklistDeviceHandler)
	http.HandleFunc("/api/connection-logs", getConnectionLogsHandler)
	http.HandleFunc("/api/get-aps", getAPsHandler)

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		log.Println("\nShutdown signal received.")
		directStopServices()
		os.Exit(0)
	}()

	go runServices()

	port := "8083" // Changed port to match log
	log.Printf("Go backend server starting on port %s", port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatalf("Failed to start Go backend server: %v", err)
	}
}
