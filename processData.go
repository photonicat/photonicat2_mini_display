package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-ping/ping"
)

// uaTransport wraps an http.RoundTripper to inject our User-Agent header.
type uaTransport struct {
	base http.RoundTripper
}

func (t *uaTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.Header.Set("User-Agent", getUserAgent())
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

// Secure HTTP client with timeouts and proper TLS configuration
var secureHTTPClient = &http.Client{
	Timeout: 10 * time.Second,
	Transport: &uaTransport{base: &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: false, // Always verify certificates
			MinVersion:         tls.VersionTLS12,
		},
		TLSHandshakeTimeout: 5 * time.Second,
	}},
}

// Local HTTP client for internal APIs (localhost)
var localHTTPClient = &http.Client{
	Timeout:   15 * time.Second,
	Transport: &uaTransport{},
}

// Pre-compiled regexes. These used to be compiled on every call inside hot,
// per-cycle functions (sanitizeCommandArg runs on EVERY secureExecCommand);
// compiling once at startup avoids that repeated cost on the slow ARM core.
var (
	reValidArg = regexp.MustCompile(`^[a-zA-Z0-9._/-]+$`)
	reStaMode  = regexp.MustCompile(`^wireless\.([^.]+)\.mode='?sta'?$`)
	reESSID    = regexp.MustCompile(`ESSID:"(.*?)"`)
)

// isOpenWRT result is fixed for the life of the process; compute it once.
var (
	openWrtOnce sync.Once
	openWrtVal  bool
)

// stringCache memoises the result of an expensive lookup (a shell-out or HTTP
// round-trip) for a fixed TTL. On refresh failure it keeps serving the last
// good value, so a transient error doesn't cause a thundering retry every cycle.
type stringCache struct {
	mu    sync.Mutex
	ttl   time.Duration
	val   string
	at    time.Time
	valid bool
}

func newStringCache(ttl time.Duration) *stringCache {
	return &stringCache{ttl: ttl}
}

// get returns the cached value if it is still fresh; otherwise it calls fetch,
// stores the result, and returns it. If fetch errors but we have a previous
// value, the stale value is returned with no error.
func (c *stringCache) get(fetch func() (string, error)) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.valid && time.Since(c.at) < c.ttl {
		return c.val, nil
	}

	v, err := fetch()
	if err != nil {
		if c.valid {
			return c.val, nil // serve last good value
		}
		return "", err
	}

	c.val = v
	c.at = time.Now()
	c.valid = true
	return v, nil
}

func getUserAgent() string {
	return "photonicat2_display/r7700"
}

// sanitizeCommandArg validates and sanitizes command arguments
func sanitizeCommandArg(arg string) string {
	// Remove any shell metacharacters and limit to alphanumeric, dash, underscore, dot, slash
	if !reValidArg.MatchString(arg) {
		return ""
	}
	return arg
}

// secureExecCommand executes a command with sanitized arguments
func secureExecCommand(command string, args ...string) ([]byte, error) {
	// Validate command name
	if sanitizeCommandArg(command) == "" {
		return nil, fmt.Errorf("invalid command: %s", command)
	}

	// Sanitize all arguments
	sanitizedArgs := make([]string, 0, len(args))
	for _, arg := range args {
		if arg == "" {
			continue
		}
		// Allow some special arguments for system commands
		if arg == "default" || arg == "--json" || arg == "-r" || arg == "-t" || arg == "-f" ||
			arg == "-c" || arg == "-v" || strings.HasPrefix(arg, "wireless.@wifi-iface") ||
			strings.HasPrefix(arg, "/dev/") || strings.HasPrefix(arg, "-") {
			sanitizedArgs = append(sanitizedArgs, arg)
		} else if sanitized := sanitizeCommandArg(arg); sanitized != "" {
			sanitizedArgs = append(sanitizedArgs, sanitized)
		} else {
			return nil, fmt.Errorf("invalid argument: %s", arg)
		}
	}

	return exec.Command(command, sanitizedArgs...).Output()
}

// WiFiInterface mirrors each element of "wifi_interfaces" in the JSON.
type WiFiInterface struct {
	Band       string `json:"band"`
	Device     string `json:"device"`
	DeviceType string `json:"device_type"`
	Enabled    bool   `json:"enabled"`
	Encryption string `json:"encryption"`
	Exist      bool   `json:"exist"`
	Hidden     string `json:"hidden"`
	Htmode     string `json:"htmode"`
	Password   string `json:"password,omitempty"`
	SSID       string `json:"ssid"`
	Frequency  string `json:"frequency,omitempty"`
}

// DashboardInfo matches the top‐level keys in your sample JSON.
type DashboardInfo struct {
	BatteryCurrent      float64         `json:"battery_current"`
	BatteryWattage      float64         `json:"battery_wattage"`
	BoardTemperature    int             `json:"board_temperature"`
	Carrier             string          `json:"carrier"`
	ChargePercent       int             `json:"charge_percent"`
	ChargeVoltage       int             `json:"charge_voltage"`
	Connection          string          `json:"connection"`
	// ActiveEgress is the interface that actually carries the default route:
	// "wan" (eth0), "lan" (eth1), "wifi" (Smart WAN STA uplink) or "mobile".
	// It is more precise than Connection for choosing the top-bar icon.
	ActiveEgress        string          `json:"active_egress"`
	// NetworkMode is the user's "Internet via" selection: "auto", "eth_only",
	// "cell_only", "smart_wan" or "all_off".
	NetworkMode         string          `json:"network_mode"`
	// WifiSignalPercent is the Smart WAN upstream RSSI as 0-100% (or null).
	WifiSignalPercent   *int            `json:"wifi_signal_percent"`
	DHCPClientsCount    int             `json:"dhcp_clients_count"`
	UpSpeedBps          float64         `json:"up_speed"`
	DownSpeedBps        float64         `json:"down_speed"`
	FirmwareVersion     string          `json:"firmware_version"`
	Hostname            string          `json:"hostname"`
	ISPName             string          `json:"isp_name"`
	Kernel              string          `json:"kernel"`
	Model               string          `json:"model"`
	ModemModel          string          `json:"modem_model"`
	ModemSignalStrength int             `json:"modem_signal_strength"`
	OnCharging          bool            `json:"on_charging"`
	OpenWRTVersion      string          `json:"openwrt_version"`
	SdState             int             `json:"sd_state"`
	ServerLocation      string          `json:"server_location"`
	SimState            string          `json:"sim_state"`
	SimNumber           string          `json:"sim_number"`
	Uptime              string          `json:"uptime"`
	Voltage             int             `json:"voltage"`
	PublicIP            string          `json:"public_ip"`
	WiFiClientsCount    int             `json:"wifi_clients_count"`
	WiFiInterfaces      []WiFiInterface `json:"wifi_interfaces"`
}

type ModemBasicInfo struct {
	CellCarrierInfo     string         `json:"cell_carrier_info"`
	FirmwareVersion     string         `json:"firmware_version"`
	IMEINum             string         `json:"imei_num"`
	Messages            []interface{}  `json:"messages"`
	ModemCellID         string         `json:"modem_cell_id"`
	ModemCellInfo       string         `json:"modem_cell_info"`
	ModemCellSignals    string         `json:"modem_cell_signals"`
	ModemCPIN           string         `json:"modem_cpin"`
	ModemIspDetails     string         `json:"modem_isp_details"`
	ModemModel          string         `json:"modem_model"`
	ModemNetworkInfo    string         `json:"modem_network_info"`
	ModemRoamPref       string         `json:"modem_roam_pref"`
	ModemServingInfo    string         `json:"modem_serving_info"`
	ModemServingQuality string         `json:"modem_serving_quality"`
	ModemTemperature    map[string]int `json:"modem_temperature"`
	ModemUSBSpeed       string         `json:"modem_usb_speed"`
	ModemUSBNetMode     string         `json:"modem_usbnet_mode"`
	ModemValid          bool           `json:"modem_valid"`
	PolicyLTEBands      string         `json:"policy_lte_bands"`
	PolicyNR5GBands     string         `json:"policy_nr5g_bands"`
	SelectedLTEBands    string         `json:"selected_lte_bands"`
	SelectedNR5GBands   string         `json:"selected_nr5g_bands"`
	SimNumber           string         `json:"sim_number"`
	SimState            string         `json:"sim_state"`
	SMSCheckInterval    int            `json:"sms_check_interval"`
	SMSForward          bool           `json:"sms_forward"`
	SMSForwardTo        string         `json:"sms_forward_to"`
}

// NetworkStats matches the keys returned by /api/v1/data_stats.json?network_type=mobile
type NetworkStats struct {
	TodayUsed     float64 `json:"today_used"`
	WeekUsed      float64 `json:"week_used"`
	MonthUsed     float64 `json:"month_used"`
	LastMonthUsed float64 `json:"last_month_used"`
}

// NetworkSpeed represents upload/download in bytes per second
type NetworkSpeed struct {
	UploadMbps   float64
	DownloadMbps float64
}

func collectBatteryData() {
	var err error
	if battSOC, err = getBatterySoc(); err != nil {
		fmt.Printf("Could not get battery soc: %v\n", err)
		globalData.Store("BatterySoc", -1)
	} else {
		globalData.Store("BatterySoc", battSOC)
	}

	if battChargingStatus, err = getBatteryCharging(); err != nil {
		fmt.Printf("Could not get battery charging: %v\n", err)
		globalData.Store("BatteryCharging", false)
	} else {
		globalData.Store("BatteryCharging", battChargingStatus)
	}

	//if charging status change, we trigger lastActivity
	if battChargingStatus != lastChargingStatus {
		log.Println("Battery charging status changed to: ", battChargingStatus)
		if idleState == STATE_ACTIVE {
			lastActivity = time.Now().Add(-fadeInDur) //reset lastActivity for screen to stay on, - fadeInDur to send state to active
		} else {
			lastActivity = time.Now() //bring back screen with some fade in
		}
		lastChargingStatus = battChargingStatus

		log.Printf("idleTimeout: %v", idleTimeout)
	}
	if battChargingStatus {
		idleTimeout = time.Duration(cfg.ScreenDimmerTimeOnDCSeconds) * time.Second
	} else {
		idleTimeout = time.Duration(cfg.ScreenDimmerTimeOnBatterySeconds) * time.Second
	}
}

func getInfoFromPcatWeb() {
	dashbarodURL := "http://localhost:80/api/v1/dashboard.json"
	networkStatsURL := "http://localhost:80/api/v1/data_stats.json?network_type=mobile"
	basicURL := "http://localhost:80/api/v1/modem/basic.json"

	var info DashboardInfo

	// === 1) Fetch dashboard.json ===
	resp, err := localHTTPClient.Get(dashbarodURL)
	if err != nil {
		fmt.Println("Could not get dashboard info:", err)
	} else {
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			fmt.Println("Failed to read dashboard response body:", err)
		} else {
			if err2 := secureUnmarshal(body, &info); err2 != nil {
				fmt.Println("Could not unmarshal dashboard info:", err2)
			} else {
				// Store each field into globalData under a sensible key.
				globalData.Store("BoardTemperature", info.BoardTemperature)
				globalData.Store("Carrier", info.Carrier)
				globalData.Store("GatewayDevice", info.Connection)
				globalData.Store("ActiveEgress", info.ActiveEgress)
				globalData.Store("NetworkMode", info.NetworkMode)
				if info.WifiSignalPercent != nil {
					globalData.Store("WifiSignalPercent", *info.WifiSignalPercent)
				} else {
					globalData.Store("WifiSignalPercent", -1)
				}
				globalData.Store("DHCPClientsCount", info.DHCPClientsCount)
				globalData.Store("FirmwareVersion", info.FirmwareVersion)
				globalData.Store("ISPName", info.ISPName)
				globalData.Store("Model", info.Model)
				globalData.Store("ModemModel", info.ModemModel)
				globalData.Store("ModemSignalStrength", info.ModemSignalStrength)
				if info.SdState == 0 {
					globalData.Store("SdState", "No")
				} else {
					globalData.Store("SdState", "Yes")
				}
				globalData.Store("ServerLocation", info.ServerLocation)
				globalData.Store("SimNumber", info.SimNumber)

				if info.SimState == "ready" {
					globalData.Store("SimState", "Yes")
				} else {
					globalData.Store("SimState", "No")
				}

				globalData.Store("WiFiClientsCount", info.WiFiClientsCount)
				globalData.Store("WiFiInterfaces", info.WiFiInterfaces)
				globalData.Store("PublicIP", info.PublicIP)
				globalData.Store("UpSpeedBps", info.UpSpeedBps)
				globalData.Store("DownSpeedBps", info.DownSpeedBps)
				theOS := ""
				raw := info.OpenWRTVersion // e.g. "R25.02.0 / r7465-d1ccd1687"
				parts := strings.SplitN(raw, "/", 2)
				if len(parts) == 2 {
					ver := strings.TrimSpace(parts[0])    // "R25.02.0"
					commit := strings.TrimSpace(parts[1]) // "r7465-d1ccd1687"

					// remove trailing ".0" from version
					ver = strings.TrimSuffix(ver, ".0") // "R25.02"

					// keep only up to the first dash in commit
					commit = strings.SplitN(commit, "-", 2)[0] // "r7465"

					theOS = fmt.Sprintf("%s / %s", ver, commit) // "R25.02 / r7465"
				} else {
					theOS = raw
				}
				globalData.Store("OSVersion", theOS)

				// Build a slice of SSIDs for convenience
				var ssids []string
				for _, iface := range info.WiFiInterfaces {
					ssids = append(ssids, iface.SSID)
				}
				globalData.Store("WiFiSSIDs", ssids)
			}
		}
	}

	// === 2) Fetch data_stats.json ===
	resp2, err := localHTTPClient.Get(networkStatsURL)
	if err != nil {
		fmt.Println("Could not get network stats:", err)
	} else {
		defer resp2.Body.Close()
		body2, err := io.ReadAll(resp2.Body)
		if err != nil {
			fmt.Println("Failed to read network stats body:", err)
		} else {
			var stats NetworkStats
			if err3 := secureUnmarshal(body2, &stats); err3 != nil {
				fmt.Println("Could not unmarshal network stats:", err3)
			} else {
				// Now store exactly the fields you want:
				strTodayUsed := fmt.Sprintf("%0.2f", stats.TodayUsed/1024/1024/1024)
				strWeekUsed := fmt.Sprintf("%0.2f", stats.WeekUsed/1024/1024/1024)
				strMonthUsed := fmt.Sprintf("%0.2f", stats.MonthUsed/1024/1024/1024)
				strLastMonthUsed := fmt.Sprintf("%0.2f", stats.LastMonthUsed/1024/1024/1024)

				globalData.Store("DailyDataUsage", strTodayUsed)
				globalData.Store("WeeklyDataUsage", strWeekUsed)
				globalData.Store("MonthlyDataUsage", strMonthUsed)
				globalData.Store("LastMonthUsage", strLastMonthUsed)
			}
		}
	}

	// 3) Modem basic
	if resp, err := localHTTPClient.Get(basicURL); err != nil {
		fmt.Println("Could not get modem basic info:", err)
	} else {
		defer resp.Body.Close()
		if body, err := io.ReadAll(resp.Body); err != nil {
			fmt.Println("Failed to read modem basic body:", err)
		} else {
			var mb ModemBasicInfo
			if err := secureUnmarshal(body, &mb); err != nil {
				fmt.Println("Could not unmarshal modem basic info:", err)
			} else {
				globalData.Store("CellCarrierInfo", mb.CellCarrierInfo)
				globalData.Store("ModemFirmwareVer", mb.FirmwareVersion)
				globalData.Store("IMEINum", mb.IMEINum)
				globalData.Store("ModemCellID", mb.ModemCellID)
				globalData.Store("ModemCellInfo", mb.ModemCellInfo)
				globalData.Store("ModemSignals", mb.ModemCellSignals)
				globalData.Store("ModemISPDetails", mb.ModemIspDetails)

				networkInfo := mb.ModemNetworkInfo
				if strings.Contains(networkInfo, "BAND ") {
					networkInfo = strings.ReplaceAll(networkInfo, "BAND ", "B.")
				}

				globalData.Store("ModemNetworkInfo", networkInfo)

				globalData.Store("ModemRoamPref", mb.ModemRoamPref)
				globalData.Store("ModemServingInfo", mb.ModemServingInfo)
				globalData.Store("ModemServingQual", mb.ModemServingQuality)
				globalData.Store("ModemUSBSpeed", mb.ModemUSBSpeed)
				globalData.Store("ModemUSBNetMode", mb.ModemUSBNetMode)
				globalData.Store("ModemValid", mb.ModemValid)
				globalData.Store("PolicyLTEBands", mb.PolicyLTEBands)
				globalData.Store("PolicyNR5GBands", mb.PolicyNR5GBands)
				globalData.Store("SelectedLTEBands", mb.SelectedLTEBands)
				globalData.Store("SelectedNR5GBands", mb.SelectedNR5GBands)
				globalData.Store("SMSCheckInterval", mb.SMSCheckInterval)
				globalData.Store("SMSForward", mb.SMSForward)
				globalData.Store("SMSForwardTo", mb.SMSForwardTo)
				globalData.Store("ModemTemperature", mb.ModemTemperature)
			}
		}
	}
}

// formatSpeed formats speed into value and units as Mbps
func formatSpeed(mbps float64) (string, string) {
	if mbps > 100000 || mbps < 0.0 { //clamping
		mbps = 0.0
	}

	if mbps >= 1.0 {
		// For speeds ≥1 Mbps, use 3 significant digits
		return fmt.Sprintf("%.3g", mbps), "Mbps"
	}
	// For speeds <1 Mbps, keep up to 3 digits after decimal point
	return fmt.Sprintf("%.2f", mbps), "Mbps"
}

func getWANInterface() (string, error) {
	if isOpenWRT() {
		return "br-lan", nil
	}
	out, err := secureExecCommand("ip", "route", "show", "default")
	if err != nil {
		return "", err
	}

	fields := strings.Fields(string(out))
	for i, field := range fields {
		if field == "dev" && (i+1) < len(fields) {
			return fields[i+1], nil
		}
	}

	return "", fmt.Errorf("WAN interface not found")
}

func collectWANNetworkSpeed() {
	var err error
	if isOpenWRT() {
		upSpeed, ok1 := globalData.Load("UpSpeedBps")
		downSpeed, ok2 := globalData.Load("DownSpeedBps")
		if !ok1 || !ok2 {
			log.Printf("Could not get WAN network speed data\n")
			globalData.Store("WanUP", "-")
			globalData.Store("WanDOWN", "-")
			globalData.Store("WanUP_Unit", "")
			globalData.Store("WanDOWN_Unit", "")
		} else {
			wanUPVal, wanUPUnit := formatSpeed(upSpeed.(float64) / 1024 / 1024 * 8)
			wanDOWNVal, wanDOWNUnit := formatSpeed(downSpeed.(float64) / 1024 / 1024 * 8)

			globalData.Store("WanUP", wanUPVal)
			globalData.Store("WanDOWN", wanDOWNVal)
			globalData.Store("WanUP_Unit", wanUPUnit)
			globalData.Store("WanDOWN_Unit", wanDOWNUnit)
		}
	} else {
		// Cache WAN interface to avoid repeated lookups
		if wanInterface == "" || wanInterface == "null" {
			wanInterface, err = getWANInterface()
			if err != nil {
				log.Printf("Could not get WAN interface: %v\n", err)
				globalData.Store("WanUP", "0")
				globalData.Store("WanDOWN", "0")
				time.Sleep(5 * time.Second) // prevent infinite loop
				return
			}
		}

		netData, err := getNetworkSpeed(wanInterface)
		if err != nil {
			log.Printf("Could not get network speed: %v\n", err)
			globalData.Store("WanUP", "0")
			globalData.Store("WanDOWN", "0")
			return
		}
		wanUPVal, wanUPUnit := formatSpeed(netData.UploadMbps)
		wanDOWNVal, wanDOWNUnit := formatSpeed(netData.DownloadMbps)
		globalData.Store("WanUP", wanUPVal)
		globalData.Store("WanDOWN", wanDOWNVal)
		globalData.Store("WanUP_Unit", wanUPUnit)
		globalData.Store("WanDOWN_Unit", wanDOWNUnit)
	}
}

func collectFixedData() {
	kernelDate, _ := getKernelDate()
	globalData.Store("Kernel", kernelDate)
	sn, _ := getSN()
	globalData.Store("SN", sn)
}

// collectData gathers several pieces of system and network information and stores them in globalData.
func collectLinuxData(cfg Config) {
	if uptime, err := getUptime(); err != nil {
		fmt.Printf("Could not get uptime: %v\n", err)
		globalData.Store("Uptime", "N/A")
	} else {
		globalData.Store("Uptime", uptime)
	}

	// Battery voltage.
	voltageUV, err := getBatteryVoltageUV()
	if err != nil {
		fmt.Printf("Could not get battery voltage: %v\n", err)
		globalData.Store("BatteryVoltage", "N/A")
	} else {
		voltage_2digit := fmt.Sprintf("%0.2f", voltageUV/1000/1000)
		globalData.Store("BatteryVoltage", voltage_2digit)
	}

	// Battery current.
	currentUA, err := getBatteryCurrentUA()
	if err != nil {
		fmt.Printf("Could not get battery current: %v\n", err)
		globalData.Store("BatteryCurrent", -9999)
	} else {
		current_2digit := fmt.Sprintf("%0.2f", currentUA/1000/1000)
		globalData.Store("BatteryCurrent", current_2digit)
	}

	// Battery wattage.
	wattage := float64(voltageUV) * float64(currentUA) / 1000 / 1000 / 1000 / 1000
	globalData.Store("BatteryWattage", fmt.Sprintf("%0.1f", wattage))

	// DC voltage.
	dcVoltageUV, err := getDCVoltageUV()
	if err != nil {
		fmt.Printf("Could not get DC voltage: %v\n", err)
		globalData.Store("DCVoltage", -9999)
	} else {
		globalData.Store("DCVoltage", fmt.Sprintf("%0.1f", dcVoltageUV/1000/1000))
	}

	// CPU temperature.
	if cpuTemp, err := getCpuTemp(); err != nil {
		fmt.Printf("Could not get CPU temperature: %v\n", err)
		globalData.Store("CpuTemp", -9999)
	} else {
		cpuTemp_1digit := fmt.Sprintf("%0.1f", cpuTemp/1000)
		globalData.Store("CpuTemp", cpuTemp_1digit)
	}

	// CPU usage.
	cpuUsage, err := getCPUUsage()
	if err != nil {
		fmt.Printf("Could not get CPU usage: %v\n", err)
		globalData.Store("CpuUsage", 0)
	} else {
		cpuUsageInt := int(cpuUsage)
		globalData.Store("CpuUsage", cpuUsageInt)
	}

	// Memory usage.
	if memUsed, memTotal, err := getMemUsedAndTotalGB(); err != nil {
		fmt.Printf("Could not get memory usage: %v\n", err)
		globalData.Store("MemUsage", nil)
	} else {
		memUsed_1digit := fmt.Sprintf("%0.1f", memUsed)
		memTotal_ceilInt := int(math.Ceil(memTotal))
		memString := fmt.Sprintf("%s/%d", memUsed_1digit, memTotal_ceilInt)
		globalData.Store("MemUsage", memString)
	}

	// Disk usage.
	if diskData, err := getDiskUsage(); err != nil {
		fmt.Printf("Could not get disk usage: %v\n", err)
		globalData.Store("DiskData", nil)
	} else {
		globalData.Store("DiskData", diskData)
	}

	//Fan speed
	fanSpeed, err := getFanSpeed()
	if err != nil {
		fmt.Printf("Could not get fan speed: %v\n", err)
		globalData.Store("FanRPM", "N/A")
	} else {
		globalData.Store("FanRPM", fanSpeed)
	}
}

// getFanSpeed scans /sys/class/hwmon/hwmon*/fan1_input and returns the first
// valid integer it reads.
func getFanSpeed() (int, error) {
	// Glob all fan1_input files in hwmon directories
	paths, err := filepath.Glob("/sys/class/hwmon/hwmon*/fan1_input")
	if err != nil {
		return 0, fmt.Errorf("failed to glob hwmon paths: %w", err)
	}

	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			// skip files we can't read
			continue
		}
		s := strings.TrimSpace(string(data))
		if s == "" {
			continue
		}

		speed, err := strconv.Atoi(s)
		if err != nil {
			// skip non-integer contents
			continue
		}
		return speed, nil
	}
	return 0, fmt.Errorf("no valid fan1_input found under /sys/class/hwmon")
}

func collectNetworkData(cfg Config) {
	if isOpenWRT() {
		//we have aonther func to get data from pcat-manager-web
	} else {
		if sessionDataUsage, err := getSessionDataUsageGB(wanInterface); err != nil {
			fmt.Printf("Could not get session data usage: %v\n", err)
			globalData.Store("SessionDataUsage", nil)
		} else {
			sessionDataUsage_1digit := fmt.Sprintf("%0.1f", sessionDataUsage)
			globalData.Store("SessionDataUsage", sessionDataUsage_1digit)
		}

		if monthlyDataUsage, err := getDataUsageMonthlyGB(wanInterface); err != nil {
			fmt.Printf("Could not get monthly data usage: %v\n", err)
			globalData.Store("MonthlyDataUsage", nil)
		} else {
			monthlyDataUsage_1digit := fmt.Sprintf("%0.1f", monthlyDataUsage)
			globalData.Store("MonthlyDataUsage", monthlyDataUsage_1digit)
		}
	}

	// Local IP address.
	if localIP, err := getLocalIPv4(); err != nil {
		fmt.Printf("Could not get local IP: %v\n", err)
		globalData.Store("LAN_IP", "N/A")
	} else {
		globalData.Store("LAN_IP", localIP)
	}

	// WAN IP address (local WAN interface IP)
	if wanIP, err := getWanIPv4(); err != nil {
		fmt.Printf("Could not get WAN IP: %v\n", err)
		globalData.Store("WAN_IP", "N/A")
	} else {
		globalData.Store("WAN_IP", wanIP)
	}

	// Public IP address.
	if publicIP, err := getPublicIPv4(); err != nil {
		fmt.Printf("Could not get public IP: %v\n", err)
		globalData.Store("PUBLIC_IP", "N/A")
	} else {
		globalData.Store("PUBLIC_IP", publicIP)
	}

	// SSID.
	if ssid, err := getSSID(); err != nil {
		//fmt.Printf("Could not get SSID: %v\n", err)
		globalData.Store("SSID", "N/A")
	} else {
		globalData.Store("SSID", ssid)
	}

	// SSID.
	if ssid2, err := getSSID2(); err != nil {
		//fmt.Printf("Could not get SSID: %v\n", err)
		globalData.Store("SSID2", "N/A")
	} else {
		globalData.Store("SSID2", ssid2)
	}

	// DHCP clients (OpenWrt).
	if dhcpClients, err := getDHCPClients(); err != nil {
		fmt.Printf("Could not get DHCP clients: %v\n", err)
		globalData.Store("DHCPClients", nil)
	} else {
		globalData.Store("DHCPClients", dhcpClients)
	}

	// WiFi clients (OpenWrt).
	if wifiClients, err := getWifiClients(); err != nil {
		fmt.Printf("Could not get WiFi clients: %v\n", err)
		globalData.Store("WifiClients", nil)
	} else {
		globalData.Store("WifiClients", wifiClients)
	}

	// ICMP ping now runs in its own goroutine (collectPingData) on a slower,
	// independently-throttled cadence, so a dead host can't block this function.

	/*
		// Country based on public IP geolocation.
		if country, err := getCountry(); err != nil {
			fmt.Printf("Could not get country: %v\n", err)
			globalData.Store("Country", "Unknown")
		} else {
			globalData.Store("Country", country)
		}*/

	// IPv6 public IP.
	if ipv6, err := getIPv6Public(); err != nil {
		//fmt.Printf("Could not get IPv6 public IP: %v\n", err)
		globalData.Store("PublicIPv6", "0.0.0.0")
	} else {
		globalData.Store("PublicIPv6", ipv6)
	}
}

// recordPingResult updates the running ping statistics for one site and stores
// the value/success-rate the UI reads. total/successful/lastSuccess are pointers
// into the per-site stats struct; mu guards them. pingVal/pingErr are the result
// of a single pingICMP() call (pingVal == -2 means timeout).
func recordPingResult(mu *sync.RWMutex, total, successful *int, lastSuccess *int64,
	valKey, rateKey string, pingVal int64, pingErr error) {
	mu.Lock()
	defer mu.Unlock()

	*total++
	switch {
	case pingErr != nil:
		// Keep showing the last good value, or -1 if we never succeeded.
		if *lastSuccess > 0 {
			globalData.Store(valKey, *lastSuccess)
		} else {
			globalData.Store(valKey, int64(-1))
		}
	case pingVal == -2:
		globalData.Store(valKey, int64(-2)) // timeout -> red X
	case pingVal > 0:
		*successful++
		*lastSuccess = pingVal
		globalData.Store(valKey, pingVal)
	default:
		if *lastSuccess > 0 {
			globalData.Store(valKey, *lastSuccess)
		} else {
			globalData.Store(valKey, int64(-1))
		}
	}

	rate := float64(*successful) / float64(*total) * 100
	globalData.Store(rateKey, fmt.Sprintf("%.0f", rate))
}

// collectPingData pings both configured sites concurrently. It runs on its own
// (slow, idle-throttled) ticker so a dead host blocks only this goroutine for at
// most pingICMP's timeout, never the rest of network collection.
func collectPingData(cfg Config) {
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		ping0, err := pingICMP(cfg.PingSite0)
		recordPingResult(&ping0Stats.mu, &ping0Stats.total, &ping0Stats.successful,
			&ping0Stats.lastSuccess, "Ping0", "Ping0Rate", ping0, err)
	}()

	go func() {
		defer wg.Done()
		ping1, err := pingICMP(cfg.PingSite1)
		recordPingResult(&ping1Stats.mu, &ping1Stats.total, &ping1Stats.successful,
			&ping1Stats.lastSuccess, "Ping1", "Ping1Rate", ping1, err)
	}()

	wg.Wait()
}

func getSN() (string, error) {
	// Read first 500 bytes
	out, err := secureExecCommand("head", "-c", "10000", "/dev/mmcblk0boot1")
	if err != nil {
		return "", fmt.Errorf("read partition: %w", err)
	}

	// Truncate at first 0 byte
	if idx := bytes.IndexByte(out, 0); idx != -1 {
		out = out[:idx]
	}

	// Parse JSON
	var payload map[string]interface{}
	if err := secureUnmarshal(out, &payload); err != nil {
		return "", fmt.Errorf("unmarshal JSON: %w", err)
	}

	// Extract "sn" or fallback to "machine_sn"
	var sn string
	if v, ok := payload["sn"]; ok {
		if s, ok2 := v.(string); ok2 && s != "" {
			sn = s
		}
	}
	if sn == "" {
		if v, ok := payload["machine_sn"]; ok {
			if s, ok2 := v.(string); ok2 && s != "" {
				sn = s
			}
		}
	}
	if sn == "" {
		return "", fmt.Errorf(`key "sn" or "machine_sn" not found or not a non-empty string`)
	}

	return sn, nil
}

// getUptimeSeconds returns system uptime in seconds
func getUptimeSeconds() (float64, error) {
	// Read /proc/uptime
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0, fmt.Errorf("error reading /proc/uptime: %v", err)
	}

	// Parse the first value (uptime in seconds)
	fields := strings.Fields(string(data))
	if len(fields) < 1 {
		return 0, fmt.Errorf("invalid uptime data")
	}

	seconds, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, fmt.Errorf("error parsing uptime: %v", err)
	}

	return seconds, nil
}

func getUptime() (string, error) {
	seconds, err := getUptimeSeconds()
	if err != nil {
		return "", err
	}

	// Convert seconds to time.Duration
	uptime := time.Duration(seconds) * time.Second

	// Calculate days, hours, minutes, and seconds
	days := int(uptime.Hours()) / 24
	hours := int(uptime.Hours()) % 24
	minutes := int(uptime.Minutes()) % 60
	secs := int(uptime.Seconds()) % 60

	// Build human-readable string, omitting zero values
	var parts []string
	if days > 0 {
		parts = append(parts, fmt.Sprintf("%dd", days))
	}
	if hours > 0 {
		parts = append(parts, fmt.Sprintf("%dh", hours))
	}
	if minutes > 0 {
		parts = append(parts, fmt.Sprintf("%dm", minutes))
	}
	if secs > 0 || len(parts) == 0 { // Include seconds if zero to avoid empty string
		parts = append(parts, fmt.Sprintf("%ds", secs))
	}

	return strings.Join(parts, " "), nil
}

func getKernelDate() (string, error) {
	// get kernel version (release)
	buildOut, err := secureExecCommand("uname", "-v")
	display_date_str := "unknown-date"
	if err == nil {
		raw := strings.TrimSpace(string(buildOut))
		parts := strings.Split(raw, " ")
		if len(parts) >= 9 { //#15 SMP PREEMPT Wed Apr 30 17:23:30 JST 2025 //debian
			display_date_str = fmt.Sprintf("%s-%s-%s", parts[8], parts[4], parts[5])
		} else if len(parts) >= 8 { //#0 SMP PREEMPT Wed May 14 09:34:38 2025 //openwrt
			display_date_str = fmt.Sprintf("%s-%s-%s", parts[7], parts[4], parts[5])
		}
	}

	return fmt.Sprintf("%s", display_date_str), nil
}

// getDCVoltageUV reads DC voltage from the system.
func getDCVoltageUV() (float64, error) {
	file, err := os.Open("/sys/class/power_supply/charger/voltage_now")
	if err != nil {
		return 0, err
	}
	defer file.Close()
	content, err := io.ReadAll(file)
	rawUV, err := strconv.ParseFloat(strings.TrimSpace(string(content)), 64)
	if err != nil {
		return 0.0, err
	}
	if rawUV < 1*1000*1000 {
		return 0.0, nil
	}
	return rawUV, nil
}

// getInterfaceBytes reads rx and tx bytes for a given interface.
func getInterfaceBytes(iface string) (rxBytes, txBytes uint64, err error) {
	basePath := "/sys/class/net/" + iface + "/statistics/"
	rxPath := basePath + "rx_bytes"
	txPath := basePath + "tx_bytes"

	readBytes := func(path string) (uint64, error) {
		data, err := os.ReadFile(path)
		if err != nil {
			return 0, err
		}
		val, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
		return val, err
	}

	rxBytes, err = readBytes(rxPath)
	if err != nil {
		return
	}
	txBytes, err = readBytes(txPath)
	return
}

func isOpenWRT() bool {
	openWrtOnce.Do(func() {
		_, err := os.Stat("/etc/openwrt_release")
		openWrtVal = err == nil
	})
	return openWrtVal
}

// wirelessConfig holds the SSIDs parsed from a single `uci show wireless` dump.
// SSIDs change very rarely, but collectNetworkData used to shell out to uci 2-3
// times every cycle (4s) to read them. We now parse one dump and cache it, so
// the common case costs zero forks. staSSID is the upstream hotspot SSID in
// Smart WAN (station) mode; ssid0/ssid1 are wifi-iface[0]/[1].
type wirelessConfig struct {
	staSSID string
	ssid0   string
	ssid1   string
}

var (
	wirelessCacheMu  sync.Mutex
	wirelessCacheVal wirelessConfig
	wirelessCacheAt  time.Time
	wirelessCacheOK  bool
	// SSIDs essentially never change at runtime; refresh at most this often.
	wirelessCacheTTL = 30 * time.Second
)

// getWirelessConfig returns the cached wireless config, refreshing it from a
// single `uci show wireless` call when the cache is empty or older than the TTL.
func getWirelessConfig() (wirelessConfig, bool) {
	wirelessCacheMu.Lock()
	defer wirelessCacheMu.Unlock()

	if wirelessCacheOK && time.Since(wirelessCacheAt) < wirelessCacheTTL {
		return wirelessCacheVal, true
	}

	out, err := secureExecCommand("uci", "-q", "show", "wireless")
	if err != nil {
		// Keep serving the last good value if we have one.
		return wirelessCacheVal, wirelessCacheOK
	}

	cfg := parseWirelessShow(string(out))
	wirelessCacheVal = cfg
	wirelessCacheAt = time.Now()
	wirelessCacheOK = true
	return cfg, true
}

// parseWirelessShow extracts the STA-mode SSID and the SSIDs of wifi-iface[0]
// and wifi-iface[1] from `uci show wireless` output. Split out for testability.
// Lines look like:
//	wireless.cfg0a2b63.mode='sta'
//	wireless.cfg0a2b63.ssid='Shanghai Novotech WiFi7_5G'
//	wireless.default_radio0.ssid='photonicat2'
func parseWirelessShow(out string) wirelessConfig {
	var cfg wirelessConfig
	staSection := ""
	// section name -> ssid, plus ordered list of wifi-iface section names
	ssidBySection := map[string]string{}
	var ifaceSections []string

	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if m := reStaMode.FindStringSubmatch(line); m != nil {
			staSection = m[1]
			continue
		}
		// wireless.<section>.ssid='...'
		if i := strings.Index(line, ".ssid="); i > len("wireless.") && strings.HasPrefix(line, "wireless.") {
			section := line[len("wireless.") : i]
			val := strings.TrimSpace(line[i+len(".ssid="):])
			val = strings.Trim(val, "'\"")
			ssidBySection[section] = val
			ifaceSections = append(ifaceSections, section)
		}
	}

	if staSection != "" {
		cfg.staSSID = ssidBySection[staSection]
	}
	// Preserve previous behaviour: wifi-iface[0]/[1] map to the 1st/2nd
	// sections that carry an ssid, in uci output order.
	if len(ifaceSections) > 0 {
		cfg.ssid0 = ssidBySection[ifaceSections[0]]
	}
	if len(ifaceSections) > 1 {
		cfg.ssid1 = ssidBySection[ifaceSections[1]]
	}
	return cfg
}

// getOpenWrtStaSSID returns the SSID of the first wifi-iface in station (sta)
// mode — i.e. the upstream hotspot we are connected to in Smart WAN mode. Empty
// string when no STA iface is configured.
func getOpenWrtStaSSID() string {
	cfg, ok := getWirelessConfig()
	if !ok {
		return ""
	}
	return cfg.staSSID
}

// getSSID returns connected SSID on Debian or broadcasting SSID on OpenWrt.
func getSSID() (string, error) {
	// OpenWrt detection
	if isOpenWRT() {
		// Smart WAN: when the onboard radio relays an upstream hotspot it runs
		// a station (sta) iface. Show the SSID we are actually connected to
		// rather than the parked AP's (often stale "OpenWrt") SSID.
		if sta := getOpenWrtStaSSID(); sta != "" {
			return sta, nil
		}
		if cfg, ok := getWirelessConfig(); ok {
			return cfg.ssid0, nil
		}
		return "", fmt.Errorf("failed to get OpenWrt SSID")
	}

	// Debian/Ubuntu: Try iwgetid first
	if out, err := secureExecCommand("iwgetid", "-r"); err == nil {
		ssid := strings.TrimSpace(string(out))
		if ssid != "" {
			return ssid, nil
		}
	}

	// Fallback 1: iwconfig
	if out, err := secureExecCommand("iwconfig"); err == nil {
		matches := reESSID.FindSubmatch(out)
		if len(matches) >= 2 {
			ssid := string(matches[1])
			if ssid != "" && ssid != "off/any" {
				return ssid, nil
			}
		}
	}

	// Fallback 2: nmcli (NetworkManager)
	if out, err := secureExecCommand("nmcli", "-t", "-f", "active,ssid", "dev", "wifi"); err == nil {
		lines := strings.Split(string(out), "\n")
		for _, line := range lines {
			fields := strings.Split(line, ":")
			if len(fields) == 2 && fields[0] == "yes" && fields[1] != "" {
				return fields[1], nil
			}
		}
	}

	return "", fmt.Errorf("SSID could not be determined")
}

// getSSID returns connected SSID on Debian or broadcasting SSID on OpenWrt.
func getSSID2() (string, error) {
	// OpenWrt detection
	if isOpenWRT() {
		// OpenWrt: serve the second wifi-iface SSID from the shared cache.
		if cfg, ok := getWirelessConfig(); ok {
			return cfg.ssid1, nil
		}
		return "", fmt.Errorf("failed to get OpenWrt SSID")
	}

	// Debian/Ubuntu: Try iwgetid first
	if out, err := secureExecCommand("iwgetid", "-r"); err == nil {
		ssid := strings.TrimSpace(string(out))
		if ssid != "" {
			return ssid, nil
		}
	}

	// Fallback 1: iwconfig
	if out, err := secureExecCommand("iwconfig"); err == nil {
		matches := reESSID.FindSubmatch(out)
		if len(matches) >= 2 {
			ssid := string(matches[1])
			if ssid != "" && ssid != "off/any" {
				return ssid, nil
			}
		}
	}

	// Fallback 2: nmcli (NetworkManager)
	if out, err := secureExecCommand("nmcli", "-t", "-f", "active,ssid", "dev", "wifi"); err == nil {
		lines := strings.Split(string(out), "\n")
		for _, line := range lines {
			fields := strings.Split(line, ":")
			if len(fields) == 2 && fields[0] == "yes" && fields[1] != "" {
				return fields[1], nil
			}
		}
	}

	return "", fmt.Errorf("SSID could not be determined")
}

// getNetworkSpeed calculates instant network speed for the specified interface.
func getNetworkSpeed(iface string) (NetworkSpeed, error) {
	rx1, tx1, err := getInterfaceBytes(iface)
	if err != nil {
		return NetworkSpeed{}, err
	}

	// Reduced sampling interval for better responsiveness
	time.Sleep(999 * time.Millisecond) // Reduced from 1999ms to 999ms

	rx2, tx2, err := getInterfaceBytes(iface)
	if err != nil {
		return NetworkSpeed{}, err
	}

	downloadMbps := float64(rx2-rx1) / 1024 / 128 / 1 // Adjusted for 1 second
	uploadMbps := float64(tx2-tx1) / 1024 / 128 / 1   // Adjusted for 1 second

	return NetworkSpeed{
		UploadMbps:   uploadMbps,
		DownloadMbps: downloadMbps,
	}, nil
}

func getSessionDataUsageGB(iface string) (float64, error) {
	stats := []string{"rx_bytes", "tx_bytes"}
	var totalBytes uint64

	for _, stat := range stats {
		// build path: /sys/class/net/<iface>/statistics/<stat>
		path := filepath.Join("/sys/class/net", iface, "statistics", stat)

		// read the file
		data, err := os.ReadFile(path)
		if err != nil {
			return 0, fmt.Errorf("failed to read %s: %w", path, err)
		}

		// parse it as uint64
		s := strings.TrimSpace(string(data))
		val, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("failed to parse %s: %w", path, err)
		}

		totalBytes += val
	}

	// convert bytes → MiB
	return float64(totalBytes) / 1024.0 / 1024.0 / 1024.0, nil
}

type vnstatJSON struct {
	Interfaces []struct {
		Name    string `json:"name"`
		Traffic struct {
			// 对应 JSON 中 "traffic":"month":[…]
			Month []struct {
				Date struct {
					Year  int `json:"year"`
					Month int `json:"month"`
				} `json:"date"`
				Rx uint64 `json:"rx"`
				Tx uint64 `json:"tx"`
			} `json:"month"`
		} `json:"traffic"`
	} `json:"interfaces"`
}

// getDataUsageMonthlyGB returns the total (rx+tx) traffic for the current calendar
// month on the given interface, as reported by vnStat, in GiB.
func getDataUsageMonthlyGB(iface string) (float64, error) {
	// 1. 调用 vnstat 获取 JSON
	out, err := secureExecCommand("vnstat", "-i", iface, "--json")
	if err != nil {
		fmt.Printf("failed to run vnstat with default interface: %s, %v", iface, err)
		iface = "wwan0"
		out, err = secureExecCommand("vnstat", "-i", iface, "--json")
		if err != nil {
			fmt.Printf("failed to run vnstat with default interface: %s, %v", iface, err)
			iface = "br-lan"
			out, err = secureExecCommand("vnstat", "-i", iface, "--json")
			if err != nil {
				fmt.Printf("failed to run vnstat with default interface: %s, %v", iface, err)
				return 0, fmt.Errorf("failed to run vnstat with iface: %s, %w", iface, err)
			}
		}
	}

	// 2. 解析 JSON
	var data vnstatJSON
	if err := secureUnmarshal(out, &data); err != nil {
		return 0, fmt.Errorf("failed to parse vnstat JSON: %w", err)
	}

	// 3. 找到对应接口
	var ifaceData *vnstatJSON
	var entryIdx int
	for i, entry := range data.Interfaces {
		if entry.Name == iface {
			ifaceData = &data
			entryIdx = i
			break
		}
	}
	if ifaceData == nil {
		return 0, fmt.Errorf("interface %q not found in vnstat output", iface)
	}

	// 4. 确定当前年/月
	now := time.Now()
	cy, cm := now.Year(), int(now.Month())
	cmStr := fmt.Sprintf("%02d", cm)

	// 5. 在 traffic.month 数组里找当月条目
	for _, m := range data.Interfaces[entryIdx].Traffic.Month {
		if m.Date.Year == cy && m.Date.Month == cm {
			usedBytes := m.Rx + m.Tx
			return float64(usedBytes) / (1 << 30), nil // GiB
		}
	}

	return 0, fmt.Errorf("no data for %04d-%s in vnstat output", cy, cmStr)
}

// CPUStats represents a CPU usage snapshot.
type CPUStats struct {
	User, Nice, System, Idle, Iowait, Irq, Softirq, Steal uint64
}

func readCPUStats() ([]CPUStats, error) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return nil, err
	}

	var stats []CPUStats
	lines := strings.Split(string(data), "\n")

	for _, line := range lines {
		if strings.HasPrefix(line, "cpu") && len(line) > 3 && line[3] >= '0' && line[3] <= '9' {
			fields := strings.Fields(line)
			if len(fields) < 8 {
				continue
			}
			var stat CPUStats
			stat.User, _ = strconv.ParseUint(fields[1], 10, 64)
			stat.Nice, _ = strconv.ParseUint(fields[2], 10, 64)
			stat.System, _ = strconv.ParseUint(fields[3], 10, 64)
			stat.Idle, _ = strconv.ParseUint(fields[4], 10, 64)
			stat.Iowait, _ = strconv.ParseUint(fields[5], 10, 64)
			stat.Irq, _ = strconv.ParseUint(fields[6], 10, 64)
			stat.Softirq, _ = strconv.ParseUint(fields[7], 10, 64)
			if len(fields) > 8 {
				stat.Steal, _ = strconv.ParseUint(fields[8], 10, 64)
			}
			stats = append(stats, stat)
		}
	}

	return stats, nil
}

func getCPUUsage() (float64, error) {
	cpus, err := getCpuUsages()
	if err != nil {
		return 0, err
	}
	total := 0.0
	for _, cpu := range cpus {
		total += cpu
	}
	return total / float64(len(cpus)), nil
}

func getCpuUsages() ([]float64, error) {
	stats1, err := readCPUStats()
	if err != nil {
		return nil, err
	}

	time.Sleep(500 * time.Millisecond)

	stats2, err := readCPUStats()
	if err != nil {
		return nil, err
	}

	var usages []float64
	for i := 0; i < len(stats1) && i < len(stats2); i++ {
		idle1 := stats1[i].Idle + stats1[i].Iowait
		idle2 := stats2[i].Idle + stats2[i].Iowait

		nonIdle1 := stats1[i].User + stats1[i].Nice + stats1[i].System +
			stats1[i].Irq + stats1[i].Softirq + stats1[i].Steal

		nonIdle2 := stats2[i].User + stats2[i].Nice + stats2[i].System +
			stats2[i].Irq + stats2[i].Softirq + stats2[i].Steal

		total1 := idle1 + nonIdle1
		total2 := idle2 + nonIdle2

		totalDelta := float64(total2 - total1)
		idleDelta := float64(idle2 - idle1)

		cpuPercentage := (totalDelta - idleDelta) / totalDelta * 100
		usages = append(usages, cpuPercentage)
	}

	return usages, nil
}

// pingICMP uses github.com/go-ping/ping to perform an ICMP ping.
// Note: raw ICMP ping usually requires root privileges.
// Returns -2 for timeouts >3 seconds, -1 for other errors, or ping time in ms.
func pingICMP(host string) (int64, error) {
	pinger, err := ping.NewPinger(host)
	if err != nil {
		return -1, err
	}
	// Set privileged mode if possible; otherwise, false will use UDP.
	pinger.SetPrivileged(true)
	pinger.Count = 1
	pinger.Timeout = 2 * time.Second // 2s is plenty; keeps a dead host from stalling long

	// Run the ping (blocking).
	err = pinger.Run()
	if err != nil {
		// Check if this is a timeout error
		if strings.Contains(err.Error(), "timeout") {
			return -2, nil // Special value for timeout
		}
		return -1, err
	}
	
	stats := pinger.Statistics()
	if stats.PacketsRecv == 0 {
		return -2, nil // No packets received = timeout
	}
	
	avgRtt := int64(stats.AvgRtt / time.Millisecond)
	
	// If ping took more than 3 seconds, treat as timeout
	if avgRtt > 3000 {
		return -2, nil
	}
	
	// Return average round-trip time in milliseconds.
	return avgRtt, nil
}

// getBatterySoc returns the battery soc from /sys/class/power_supply/battery/capacity.
func getBatterySoc() (int, error) {
	file, err := os.Open("/sys/class/power_supply/battery/capacity")
	if err != nil {
		return -1, err
	}
	defer file.Close()
	content, err := io.ReadAll(file)
	if err != nil {
		return -1, err
	}
	socInt, err := strconv.Atoi(strings.TrimSpace(string(content)))
	if err != nil {
		return -1, err
	}
	return socInt, nil
}

// getBatteryCharging returns the battery charging status from /sys/class/power_supply/battery/status.
func getBatteryCharging() (bool, error) {
	var determineChargingByCurrent bool = false
	if determineChargingByCurrent {
		current, err := getBatteryCurrentUA()
		if err != nil {
			return false, err
		}
		return current > 0, nil
	} else {
		file, err := os.Open("/sys/class/power_supply/battery/status")
		if err != nil {
			return false, err
		}
		defer file.Close()
		content, err := io.ReadAll(file)
		if err != nil {
			return false, err
		}

		battContent := strings.TrimSpace(string(content))

		if battContent == "Charging" || battContent == "Full" {
			return true, nil
		}
		return false, nil
	}
}

func getBatteryVoltageUV() (float64, error) {
	file, err := os.Open("/sys/class/power_supply/battery/voltage_now")
	if err != nil {
		return 0, err
	}
	defer file.Close()
	content, err := io.ReadAll(file)
	if err != nil {
		return 0, err
	}
	return strconv.ParseFloat(strings.TrimSpace(string(content)), 64)
}

func getBatteryCurrentUA() (float64, error) {
	file, err := os.Open("/sys/class/power_supply/battery/current_now")
	if err != nil {
		return 0, err
	}
	defer file.Close()
	content, err := io.ReadAll(file)
	if err != nil {
		return 0, err
	}
	return strconv.ParseFloat(strings.TrimSpace(string(content)), 64)
}

// getLocalIPv4 returns eth0 IP on OpenWrt or WAN IP (default route) on Debian.
func getLocalIPv4() (string, error) {
	candidates := []string{"eth1", "end1", "end0", "br-lan"}

	for _, name := range candidates {
		iface, err := net.InterfaceByName(name)
		if err != nil {
			// interface doesn't exist
			continue
		}
		// skip if interface is down
		if iface.Flags&net.FlagUp == 0 {
			continue
		}

		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			if ipnet, ok := addr.(*net.IPNet); ok {
				if ip4 := ipnet.IP.To4(); ip4 != nil {
					return ip4.String(), nil
				}
			}
		}
	}

	// none of the candidates had a usable IPv4
	return "LINK DOWN", nil
}

// getWanIPv4 returns WAN interface IP from OpenWrt or default route IP on Debian.
func getWanIPv4() (string, error) {
	if isOpenWRT() {
		// Try to get WAN IP from uci network.wan.ipaddr first
		out, err := secureExecCommand("uci", "get", "network.wan.ipaddr")
		if err == nil {
			wanIP := strings.TrimSpace(string(out))
			if net.ParseIP(wanIP) != nil && net.ParseIP(wanIP).To4() != nil {
				return wanIP, nil
			}
		}

		// Fallback: try to get IP from wan interface
		candidates := []string{"wan", "eth0", "wwan0"}
		for _, name := range candidates {
			iface, err := net.InterfaceByName(name)
			if err != nil {
				continue
			}
			if iface.Flags&net.FlagUp == 0 {
				continue
			}
			addrs, err := iface.Addrs()
			if err != nil {
				continue
			}
			for _, addr := range addrs {
				if ipnet, ok := addr.(*net.IPNet); ok {
					if ip4 := ipnet.IP.To4(); ip4 != nil {
						return ip4.String(), nil
					}
				}
			}
		}

		// Final fallback: use ip route to find WAN IP
		out, err = secureExecCommand("ip", "route", "get", "1.1.1.1")
		if err == nil {
			lines := strings.Split(string(out), "\n")
			for _, line := range lines {
				fields := strings.Fields(line)
				for i, field := range fields {
					if field == "src" && i+1 < len(fields) {
						return fields[i+1], nil
					}
				}
			}
		}
	} else {
		// Debian/Ubuntu: get source IP for default route
		out, err := secureExecCommand("ip", "route", "get", "1.1.1.1")
		if err != nil {
			return "", err
		}
		lines := strings.Split(string(out), "\n")
		for _, line := range lines {
			fields := strings.Fields(line)
			for i, field := range fields {
				if field == "src" && i+1 < len(fields) {
					return fields[i+1], nil
				}
			}
		}
	}

	return "N/A", fmt.Errorf("WAN IP not found")
}

// getPublicIPv4 makes an HTTP request to a public API to fetch the external IPv4 address.
// Public IP changes very rarely, but the network cycle asked for it every few
// seconds — an HTTP round-trip to an external host each time. Cache for an hour.
var (
	publicIPv4Cache = newStringCache(60 * time.Minute)
	publicIPv6Cache = newStringCache(60 * time.Minute)
)

func getPublicIPv4() (string, error) {
	return publicIPv4Cache.get(fetchPublicIPv4)
}

func fetchPublicIPv4() (string, error) {
	resp, err := secureHTTPClient.Get("https://4.photonicat.com/ip.php")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	ip, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	// Trim any whitespace or newlines.
	ipStr := strings.TrimSpace(string(ip))

	// Optional: Basic validation that it looks like an IPv4 address.
	if net.ParseIP(ipStr) == nil || net.ParseIP(ipStr).To4() == nil {
		return "", fmt.Errorf("invalid IPv4 address received: %s", ipStr)
	}

	return ipStr, nil
}

// getIPv6Public fetches the public IPv6 address.
func getIPv6Public() (string, error) {
	return publicIPv6Cache.get(fetchPublicIPv6)
}

func fetchPublicIPv6() (string, error) {
	resp, err := secureHTTPClient.Get("https://6.photonicat.com/ip.php")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	ip, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	// Trim any whitespace or newlines.
	ipStr := strings.TrimSpace(string(ip))

	// Optional: Basic validation that it looks like an IPv6 address.
	if net.ParseIP(ipStr) == nil || net.ParseIP(ipStr).To4() != nil {
		return "", fmt.Errorf("invalid IPv6 address received: %s", ipStr)
	}

	return ipStr, nil
}

// getCpuTemp returns CPU temperature from /sys/class/thermal/thermal_zone0/temp.
func getCpuTemp() (float64, error) {
	file, err := os.Open("/sys/class/thermal/thermal_zone0/temp")
	if err != nil {
		return 0, err
	}
	defer file.Close()
	content, err := io.ReadAll(file)
	if err != nil {
		return 0, err
	}
	return strconv.ParseFloat(strings.TrimSpace(string(content)), 64)
}

// getMemUsedAndTotalGB returns used and total memory in GB.
func getMemUsedAndTotalGB() (usedGB float64, totalGB float64, err error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0, err
	}

	var memTotal, memAvailable float64

	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		key, value := fields[0], fields[1]
		switch key {
		case "MemTotal:":
			memTotal, err = strconv.ParseFloat(value, 64)
			if err != nil {
				return 0, 0, err
			}
		case "MemAvailable:":
			memAvailable, err = strconv.ParseFloat(value, 64)
			if err != nil {
				return 0, 0, err
			}
		}
		if memTotal > 0 && memAvailable > 0 {
			break
		}
	}

	if memTotal == 0 {
		return 0, 0, fmt.Errorf("failed to read MemTotal")
	}

	usedKB := memTotal - memAvailable
	usedGB = usedKB / 1024 / 1024
	totalGB = memTotal / 1024 / 1024

	return usedGB, totalGB, nil
}

// getDiskUsage returns disk usage stats (total and free space in MB) for the current partition.
func getDiskUsage() (map[string]interface{}, error) {
	var stat syscall.Statfs_t
	err := syscall.Statfs("/", &stat)
	if err != nil {
		return nil, fmt.Errorf("failed to stat filesystem: %v", err)
	}

	totalMB := (uint64(stat.Bsize) * stat.Blocks) / (1024 * 1024)
	freeMB := (uint64(stat.Bsize) * stat.Bfree) / (1024 * 1024)

	data := map[string]interface{}{
		"Total": totalMB,
		"Free":  freeMB,
		"Used":  totalMB - freeMB,
	}

	return data, nil
}

// getCurrNetworkSpeedMbps returns current network speed in Mbps for all interfaces.
func getCurrNetworkSpeedMbps() (map[string]interface{}, error) {
	startStats, err := readNetworkStats()
	if err != nil {
		return nil, err
	}

	time.Sleep(1 * time.Second)

	endStats, err := readNetworkStats()
	if err != nil {
		return nil, err
	}

	data := make(map[string]interface{})
	for iface, end := range endStats {
		if start, ok := startStats[iface]; ok {
			rxBytes := end.rxBytes - start.rxBytes
			txBytes := end.txBytes - start.txBytes

			rxMbps := float64(rxBytes) * 8 / 1e6
			txMbps := float64(txBytes) * 8 / 1e6

			data[iface] = map[string]float64{
				"download": rxMbps,
				"upload":   txMbps,
			}
		}
	}

	return data, nil
}

// networkStats holds RX and TX bytes for an interface.
type networkStats struct {
	rxBytes uint64
	txBytes uint64
}

// readNetworkStats reads current network stats from /proc/net/dev.
func readNetworkStats() (map[string]networkStats, error) {
	file, err := os.Open("/proc/net/dev")
	if err != nil {
		return nil, fmt.Errorf("failed to open /proc/net/dev: %v", err)
	}
	defer file.Close()

	stats := make(map[string]networkStats)
	scanner := bufio.NewScanner(file)

	// Skip header lines.
	for i := 0; i < 2 && scanner.Scan(); i++ {
	}

	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) < 10 {
			continue
		}

		iface := strings.TrimSuffix(fields[0], ":")
		rxBytes, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		txBytes, err := strconv.ParseUint(fields[9], 10, 64)
		if err != nil {
			continue
		}

		stats[iface] = networkStats{
			rxBytes: rxBytes,
			txBytes: txBytes,
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error reading /proc/net/dev: %v", err)
	}

	return stats, nil
}

// getDHCPClients returns DHCP clients for OpenWRT.
func getDHCPClients() ([]string, error) {
	// Try OpenWRT method first - read DHCP lease file
	if clients, err := getOpenWrtDHCPClients(); err == nil {
		return clients, nil
	}

	// Fallback for Debian/other systems
	return getDebianDHCPClients()
}

// getWifiClients returns WiFi client MAC addresses for OpenWRT.
// WiFi client enumeration shells out to `iwinfo <iface> assoclist` for up to 6
// candidate interfaces every call — the single biggest source of forks in the
// 4s network cycle. The associated-client set changes slowly, so cache it.
var wifiClientsCache = newStringCache(30 * time.Second)

func getWifiClients() (string, error) {
	return wifiClientsCache.get(func() (string, error) {
		// Try OpenWRT method first.
		if clients, err := getOpenWrtWifiClients(); err == nil {
			return clients, nil
		} else if isOpenWRT() {
			// On OpenWrt a "no clients" error is the normal empty case — cache
			// the empty result so we don't re-fork iwinfo every cycle while no
			// device is connected.
			return "", nil
		}
		// Fallback for Debian/other systems.
		return getDebianWifiClients()
	})
}

// getOpenWrtDHCPClients reads DHCP clients from OpenWRT lease file
func getOpenWrtDHCPClients() ([]string, error) {
	// OpenWRT typically stores DHCP leases in /tmp/dhcp.leases
	out, err := secureExecCommand("cat", "/tmp/dhcp.leases")
	if err != nil {
		return nil, fmt.Errorf("failed to read DHCP leases: %v", err)
	}

	var clients []string
	lines := strings.Split(string(out), "\n")
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		// DHCP lease format: timestamp mac ip hostname client_id
		fields := strings.Fields(line)
		if len(fields) >= 3 {
			ip := fields[2]
			if ip != "*" && ip != "" {
				clients = append(clients, ip)
			}
		}
	}

	return clients, nil
}

// getOpenWrtWifiClients gets WiFi clients using OpenWRT's iwinfo
func getOpenWrtWifiClients() (string, error) {
	// WiFi interfaces to check (up to 3 max)
	interfaces := []string{
		"wlan0", "wlan1", "wlan2",  // Standard wlan interfaces
		"radio0", "radio1", "radio2", // Radio interfaces
	}
	
	var allMacs []string
	
	// Try each interface
	for _, iface := range interfaces {
		out, err := secureExecCommand("iwinfo", iface, "assoclist")
		if err != nil {
			// Interface doesn't exist or no clients, continue to next
			continue
		}
		
		// Parse iwinfo output to extract MAC addresses
		lines := strings.Split(string(out), "\n")
		for _, line := range lines {
			line = strings.TrimSpace(line)
			// Look for MAC addresses in format XX:XX:XX:XX:XX:XX
			if len(line) >= 17 && strings.Count(line, ":") >= 5 {
				fields := strings.Fields(line)
				if len(fields) > 0 {
					mac := fields[0]
					if strings.Count(mac, ":") == 5 && len(mac) == 17 {
						// Avoid duplicates
						found := false
						for _, existingMac := range allMacs {
							if existingMac == mac {
								found = true
								break
							}
						}
						if !found {
							allMacs = append(allMacs, mac)
						}
					}
				}
			}
		}
	}
	
	if len(allMacs) == 0 {
		return "", fmt.Errorf("no WiFi clients found on any interface")
	}

	return strings.Join(allMacs, ","), nil
}

// getDebianDHCPClients fallback for Debian systems
func getDebianDHCPClients() ([]string, error) {
	// Try to read from common DHCP lease locations
	leaseFiles := []string{
		"/var/lib/dhcp/dhcpd.leases",
		"/var/lib/dhcpcd5/dhcpcd.leases",
	}

	for _, file := range leaseFiles {
		if out, err := secureExecCommand("cat", file); err == nil {
			var clients []string
			lines := strings.Split(string(out), "\n")
			for _, line := range lines {
				if strings.Contains(line, "lease ") && strings.Contains(line, "{") {
					// Extract IP from lease line
					parts := strings.Fields(line)
					if len(parts) >= 2 {
						ip := strings.TrimSpace(parts[1])
						if ip != "" {
							clients = append(clients, ip)
						}
					}
				}
			}
			if len(clients) > 0 {
				return clients, nil
			}
		}
	}

	// Fallback to dummy data
	return []string{"192.168.1.100", "192.168.1.101"}, nil
}

// getDebianWifiClients fallback for Debian systems
func getDebianWifiClients() (string, error) {
	// Try iwconfig first
	if out, err := secureExecCommand("iwconfig"); err == nil {
		// Parse iwconfig output for connected clients (limited info)
		if strings.Contains(string(out), "Access Point:") {
			return "DEBIAN_WIFI_CLIENT", nil
		}
	}

	// Try nmcli if available
	if out, err := secureExecCommand("nmcli", "device", "wifi"); err == nil {
		lines := strings.Split(string(out), "\n")
		for _, line := range lines {
			if strings.Contains(line, "*") {
				return "NMCLI_CONNECTED", nil
			}
		}
	}

	// Fallback to dummy data
	return "DEBIAN_FALLBACK", nil
}
