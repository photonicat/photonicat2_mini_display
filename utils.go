package main

import (
	"encoding/json"
	"fmt"
	"image"
	"log"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	evdev "github.com/holoplot/go-evdev"

	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
)

var (
	fadeMu               sync.Mutex
	fadeCancel           chan struct{}
	swippingScreen       bool
	wasScreenIdle        bool // Track if screen was idle when key was pressed
	wasConsoleScreenIdle bool // Track if screen was idle for console input
)

// loadConfig reads and unmarshals the config file.
func loadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var tempCfg Config
	err = secureUnmarshal(data, &tempCfg)
	return tempCfg, err
}

var (
	fontCache = make(map[string]struct {
		face       font.Face
		fontHeight int
	})
	fontCacheMu sync.Mutex
)

// getFontFace loads (or returns cached) font.Face + its height.
func getFontFace(fontName string) (font.Face, int, error) {
	// 1) Check cache
	fontCacheMu.Lock()
	if entry, ok := fontCache[fontName]; ok {
		fontCacheMu.Unlock()
		return entry.face, entry.fontHeight, nil
	}
	fontCacheMu.Unlock()

	// 2) Not cached: load config
	cfg, ok := fonts[fontName]
	if !ok {
		return nil, 0, fmt.Errorf("font %s not found in mapping", fontName)
	}

	// 3) Read & parse the TTF/TTC
	fontBytes, err := os.ReadFile(cfg.FontPath)
	if err != nil {
		return nil, 0, fmt.Errorf("error reading font file: %v", err)
	}

	var ttfFont *opentype.Font
	// Handle TrueType Collections (.ttc files)
	if strings.HasSuffix(cfg.FontPath, ".ttc") {
		collection, err := opentype.ParseCollection(fontBytes)
		if err != nil {
			return nil, 0, fmt.Errorf("error parsing font collection: %v", err)
		}
		// Get the first font from the collection
		ttfFont, err = collection.Font(0)
		if err != nil {
			return nil, 0, fmt.Errorf("error getting font from collection: %v", err)
		}
	} else {
		// Handle single font files (.ttf, .otf)
		ttfFont, err = opentype.Parse(fontBytes)
		if err != nil {
			return nil, 0, fmt.Errorf("error parsing font: %v", err)
		}
	}

	// 4) Create the face
	face, err := opentype.NewFace(ttfFont, &opentype.FaceOptions{
		Size:    cfg.FontSize,
		DPI:     72,
		Hinting: font.HintingFull,
	})
	if err != nil {
		return nil, 0, err
	}

	// 5) Measure height
	metrics := face.Metrics()
	fontHeight := metrics.Ascent.Round() + metrics.Descent.Round()

	// 6) Store in cache
	fontCacheMu.Lock()
	fontCache[fontName] = struct {
		face       font.Face
		fontHeight int
	}{face: face, fontHeight: fontHeight}
	fontCacheMu.Unlock()

	return face, fontHeight, nil
}

// containsChinese checks if a string contains Chinese characters
func containsChinese(text string) bool {
	for _, r := range text {
		if unicode.In(r, unicode.Han) {
			return true
		}
	}
	return false
}

// getFontFaceForText returns the appropriate font face based on text content
func getFontFaceForText(baseFontName string, text string) (font.Face, int, error) {
	if containsChinese(text) {
		// Use Chinese font variant if available
		cjkFontName := baseFontName + "_cjk"
		return getFontFace(cjkFontName)
	}
	// Use regular font for non-Chinese text
	return getFontFace(baseFontName)
}

// Pre-allocated clear buffer for efficient frame clearing
var clearBuffer []uint8

func clearFrame(frame *image.RGBA, width int, height int) {
	pixelsNeeded := width * height * 4

	// Initialize clear buffer once with optimal size
	if len(clearBuffer) < pixelsNeeded {
		// Allocate larger buffer to handle future larger frames
		bufferSize := max(pixelsNeeded, PCAT2_LCD_WIDTH*PCAT2_LCD_HEIGHT*4)
		clearBuffer = make([]uint8, bufferSize)
		// Pre-fill with opaque black pixels using efficient pattern
		pattern := []uint8{0, 0, 0, 255} // R, G, B, A
		for i := 0; i < len(clearBuffer); i += 4 {
			copy(clearBuffer[i:i+4], pattern)
		}
	}

	// Ensure frame buffer is correct size
	if len(frame.Pix) < pixelsNeeded {
		frame.Pix = make([]uint8, pixelsNeeded)
	}

	// Fast bulk copy instead of pixel-by-pixel clearing
	copy(frame.Pix[:pixelsNeeded], clearBuffer[:pixelsNeeded])
}

// Helper function for max since Go doesn't have built-in max for int

// preCalculateEasing pre-computes easing values to avoid math.Pow during transitions
func preCalculateEasing(numFrames int, frameWidth int) []int {
	if len(easingLookup) != numFrames {
		easingLookup = make([]int, numFrames)
		for i := 0; i < numFrames; i++ {
			t := float64(i) / float64(numFrames)
			et3 := 1 - math.Pow(1-t, 4) // Quartic easing
			easingLookup[i] = int(et3 * float64(frameWidth))
		}
	}
	return easingLookup
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func setBacklight(brightness int) {
	mu.Lock()
	defer mu.Unlock()

	// Get effective max brightness (runtime override or config)
	effectiveMaxBrightness := getEffectiveMaxBrightness()

	// clamp into 0..effectiveMaxBrightness
	switch {
	case brightness < cfg.ScreenMinBrightness:
		brightness = cfg.ScreenMinBrightness
	case brightness > effectiveMaxBrightness:
		brightness = effectiveMaxBrightness
	}

	if brightness == lastLogical {
		return
	}
	lastLogical = brightness

	// cancel any pending off-timer if we're going to >0
	if brightness > 0 && offTimer != nil {
		offTimer.Stop()
		offTimer = nil
	}

	// choose what to write right now:
	phys := brightness
	if brightness == 0 {
		phys = 1
	}

	// perform the write
	if err := os.WriteFile("/sys/class/backlight/backlight/brightness", []byte(strconv.Itoa(phys)), 0644); err != nil {
		log.Printf("backlight write error: %v", err)
	} else {
		//log.Printf("→ physical backlight %d", phys)
	}

	// if we just handled a logical “0”, schedule the real off in ZERO_BACKLIGHT_DELAY s
	if brightness == 0 {
		offTimer = time.AfterFunc(ZERO_BACKLIGHT_DELAY, func() {
			mu.Lock()
			defer mu.Unlock()
			if lastLogical == 0 {
				// still supposed to be off, so write 0 now
				if err := os.WriteFile("/sys/class/backlight/backlight/brightness", []byte("1"), 0644); err != nil {
					log.Printf("backlight final-off error: %v", err)
				} else {
					log.Println("→ physical backlight OFF")
				}
			}
		})
	}
}

func monitorKeyboard(changePageTriggered *bool) {
	// 1) find the "rk805 pwrkey" device by name
	paths, err := evdev.ListDevicePaths()
	if err != nil {
		log.Printf("ListDevicePaths error: %v", err)
		return
	}

	var devPath string
	for _, ip := range paths {
		if ip.Name == "rk805 pwrkey" {
			devPath = ip.Path
			break
		}
	}
	if devPath == "" {
		log.Println("no EV_KEY device found")
		return
	}

	// 2) open it
	keyboard, err := evdev.Open(devPath)
	if err != nil {
		log.Printf("Open(%s) error: %v", devPath, err)
		return
	}
	defer keyboard.Ungrab()

	// 3) grab for exclusive access
	if err := keyboard.Grab(); err != nil {
		log.Printf("warning: failed to grab device: %v", err)
	}

	// 4) log what we opened
	name, _ := keyboard.Name()
	log.Printf("using input device: %s (%s)", devPath, name)

	for {
		ev, err := keyboard.ReadOne()
		if err != nil {
			log.Printf("read error: %v", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}

		now := time.Now()
		if ev.Type == evdev.EV_KEY && ev.Code == evdev.KEY_POWER {
			switch ev.Value {
			case 1: // key press
				buttonKeydownTime = now // Record button press timing
				if showDetailedTiming {
					log.Printf("⏱️  POWER pressed (key down) at +0.0ms, checking state = %s", stateName(idleState))
				}
				lastActivityMu.Lock()
				lastActivity = now
				lastActivityMu.Unlock()

				if idleState == STATE_IDLE || idleState == STATE_OFF || idleState == STATE_FADE_OUT {
					log.Println("Screen is idle/fading/off, preparing to wake up without changing page")
					wasScreenIdle = true
				} else if idleState == STATE_ACTIVE || idleState == STATE_FADE_IN {
					log.Println("Screen is active, preparing for page change")
					wasScreenIdle = false
					swippingScreen = true
					*changePageTriggered = true
					signalPageChange()
				}

			case 0: // key release
				buttonKeyupTime = now // Record button release timing
				var keyPressDurationMs float64
				if !buttonKeydownTime.IsZero() {
					keyPressDurationMs = durationToMs(now.Sub(buttonKeydownTime))
				}
				if showDetailedTiming {
					log.Printf("⏱️  POWER released (key up) +%.1fms after keydown, triggering animation if ready, state = %s", 
						keyPressDurationMs, stateName(idleState))
				}
				if wasScreenIdle {
					log.Println("Screen was idle when key was pressed, waking up without changing page")
					wasScreenIdle = false // Reset flag
				} else if idleState == STATE_ACTIVE || idleState == STATE_FADE_IN {
					//*changePageTriggered = true
				}
				// just update lastActivity
				lastActivityMu.Lock()
				lastActivity = now
				lastActivityMu.Unlock()

				/* //var lastKeyPress time.Time
				   case 1: // key press
				       log.Println("POWER pressed, state =", stateName(idleState))
				       if idleState == STATE_ACTIVE || idleState == STATE_FADE_IN {
				           swippingScreen = true
				           //*changePageTriggered = true
				       }
				       lastActivityMu.Lock()
				       lastActivity = now
				       lastActivityMu.Unlock()
				       lastKeyPress = now

				   case 0: // key release
				       // only trigger if it wasn't a quick tap (<500ms)
				       if now.Sub(lastKeyPress) > KEYBOARD_DEBOUNCE_TIME {
				           log.Println("POWER released, state =", stateName(idleState))
				           if idleState == STATE_ACTIVE{
				               swippingScreen = true
				               *changePageTriggered = true
				               signalPageChange()
				           }
				           lastActivityMu.Lock()
				           lastActivity = now
				           lastActivityMu.Unlock()
				       }*/
			}
		}
	}
}

func monitorConsoleInput(changePageTriggered *bool) {
	log.Println("Console input monitoring started. Press ENTER key to change screen.")

	for {
		var input string
		_, err := fmt.Scanln(&input)

		// Handle EOF or other input errors gracefully, but also treat empty input as valid
		if err != nil && err.Error() != "unexpected newline" {
			time.Sleep(100 * time.Millisecond)
			continue
		}

		// Trigger on any input (including empty/just Enter key)
		now := time.Now()
		log.Printf("⌨️  KEYBOARD ENTER HIT (state: %s)", stateName(idleState))
		
		if idleState == STATE_IDLE || idleState == STATE_OFF || idleState == STATE_FADE_OUT {
			log.Println("Screen waking up")
			wasConsoleScreenIdle = true
		} else if idleState == STATE_ACTIVE || idleState == STATE_FADE_IN {
			if wasConsoleScreenIdle {
				log.Println("Screen already active, not changing page")
				wasConsoleScreenIdle = false // Reset flag
			} else {
				log.Println("Triggering page change")
				swippingScreen = true
				*changePageTriggered = true
				signalPageChange()
			}
		}
		lastActivityMu.Lock()
		lastActivity = now
		lastActivityMu.Unlock()
	}
}

func getBacklight() int {
	data, err := os.ReadFile("/sys/class/backlight/backlight/brightness")
	if err != nil {
		log.Printf("getBacklight error: %v", err)
		return 0
	}
	brightness, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		log.Printf("getBacklight parse error: %v", err)
		return 0
	}
	return brightness
}

func fadeBacklight(wantValue int, timePeriod time.Duration) {
	// Grab a snapshot of the “current” cancel channel under the fadeMu lock:
	fadeMu.Lock()
	cancelChan := fadeCancel
	fadeMu.Unlock()

	initValue := getBacklight()
	if timePeriod <= 0 || initValue == wantValue {
		setBacklight(wantValue)
		return
	}

	const stepDuration = 40 * time.Millisecond
	steps := int(timePeriod / stepDuration)
	if steps < 1 {
		setBacklight(wantValue)
		return
	}

	diff := wantValue - initValue
	ticker := time.NewTicker(stepDuration)
	defer ticker.Stop()

	for i := 1; i <= steps; i++ {
		select {
		case <-cancelChan:
			// Someone closed fadeCancel → abort immediately
			log.Println("fadeBacklight: cancel requested")
			return
		case <-ticker.C:
			frac := float64(i) / float64(steps)
			b := initValue + int(math.Round(frac*float64(diff)))
			setBacklight(b)
			log.Printf("fadeBacklight: step %d/%d → brightness=%d", i, steps, b)
		}
	}
	// final guarantee
	setBacklight(wantValue)
}

func idleDimmer() {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	prevState := STATE_UNKNOWN

	for range ticker.C {
		// 1) Movement/keypress detection
		data, err := os.ReadFile("/sys/kernel/photonicat-pm/movement_trigger")
		if err == nil && strings.TrimSpace(string(data)) == "1" {
			// Reset idle timer, treat screen as already “on”
			now := time.Now()
			lastActivityMu.Lock()
			tempLastActivity := now.Add(-2 * time.Second)
			if tempLastActivity.After(lastActivity) {
				lastActivity = tempLastActivity
			}
			lastActivityMu.Unlock()
		}

		// 2) Compute idle time
		lastActivityMu.Lock()
		idle := time.Since(lastActivity)
		lastActivityMu.Unlock()

		var newState int

		switch {
		case weAreRunning == false:
			newState = STATE_OFF
		case idle < fadeInDur:
			if swippingScreen {
				newState = STATE_ACTIVE
			} else {
				newState = STATE_FADE_IN
			}
		case idle < idleTimeout:
			newState = STATE_ACTIVE
			swippingScreen = false
		case idle < idleTimeout+fadeDuration:
			newState = STATE_FADE_OUT
		default:
			newState = STATE_IDLE
		}

		if prevState != newState {
			log.Printf("STATE CHANGED: %s -> %s", stateName(prevState), stateName(newState))
			idleState = newState
			prevState = newState

			// Update intervals immediately when state changes
			updateIntervals()

			// ── Cancel any existing fade by closing fadeCancel ──────────
			fadeMu.Lock()
			if fadeCancel != nil {
				close(fadeCancel) // signal the currently running fadeBacklight (if any) to stop
				// allocate a brand‑new channel
			}
			fadeCancel = make(chan struct{})
			//myCancel := fadeCancel
			fadeMu.Unlock()

			switch newState {
			case STATE_OFF:
				fadeBacklight(10, OFF_TIMEOUT)
				os.Exit(0)

			case STATE_FADE_IN:
				if !swippingScreen {
					go fadeBacklight(maxBacklight, fadeInDur)
				}
			case STATE_FADE_OUT:
				go fadeBacklight(0, fadeDuration)

			case STATE_ACTIVE:
				setBacklight(maxBacklight)
				swippingScreen = false

			case STATE_IDLE:
				setBacklight(0)
			}
		}

	}
}

func stateName(s int) string {
	switch s {
	case STATE_FADE_IN:
		return "FADE_IN"
	case STATE_ACTIVE:
		return "ACTIVE"
	case STATE_FADE_OUT:
		return "FADE_OUT"
	case STATE_IDLE:
		return "IDLE"
	case STATE_OFF:
		return "OFF"
	default:
		return "UNKNOWN"
	}
}

// deepMergeJSON performs a deep merge of two JSON objects.
// For objects: recursively merge keys from src into dst
// For arrays: append src arrays to dst arrays
// For primitives: src overrides dst (unless src is zero value for numeric fields)
func deepMergeJSON(dst, src map[string]interface{}) map[string]interface{} {
	for key, srcVal := range src {
		// Skip zero/empty values from src for certain fields to preserve dst defaults
		if srcVal == nil {
			continue
		}

		// Skip zero numeric values for brightness/dimmer fields - they should be explicit
		if (key == "screen_max_brightness" || key == "screen_min_brightness" ||
			key == "screen_dimmer_time_on_battery_seconds" || key == "screen_dimmer_time_on_dc_seconds") {
			if numVal, ok := srcVal.(float64); ok && numVal == 0 {
				continue // Don't override with zero value
			}
		}

		// Preserve existing ping site if new value is empty string
		if key == "ping_site0" || key == "ping_site1" {
			if s, ok := srcVal.(string); ok && strings.TrimSpace(s) == "" {
				continue
			}
		}

		if dstVal, exists := dst[key]; exists {
			// Key exists in both - need to merge
			srcMap, srcIsMap := srcVal.(map[string]interface{})
			dstMap, dstIsMap := dstVal.(map[string]interface{})

			if srcIsMap && dstIsMap {
				// Both are objects - recursively merge
				dst[key] = deepMergeJSON(dstMap, srcMap)
			} else if srcSlice, srcIsSlice := srcVal.([]interface{}); srcIsSlice {
				if dstSlice, dstIsSlice := dstVal.([]interface{}); dstIsSlice {
					// Both are arrays - APPEND src to dst
					dst[key] = append(dstSlice, srcSlice...)
					log.Printf("JSON merge: Appended %d elements to array '%s' (total: %d)",
						len(srcSlice), key, len(dstSlice)+len(srcSlice))
				} else {
					// Type mismatch - src overrides
					dst[key] = srcVal
				}
			} else {
				// Primitive or type mismatch - src overrides
				dst[key] = srcVal
			}
		} else {
			// Key only in src - just copy
			dst[key] = srcVal
		}
	}
	return dst
}

// mergeConfigs rebuilds `cfg` by overlaying userCfg on top of dftCfg using JSON deep merge.
// It returns an error if any validation fails.
func mergeConfigs() error {
	configMutex.Lock()
	defer configMutex.Unlock()

	// Convert both configs to JSON
	dftJSON, err := json.Marshal(dftCfg)
	if err != nil {
		return fmt.Errorf("failed to marshal default config: %v", err)
	}

	userJSON, err := json.Marshal(userCfg)
	if err != nil {
		return fmt.Errorf("failed to marshal user config: %v", err)
	}

	// Parse to generic maps
	var dftMap map[string]interface{}
	var userMap map[string]interface{}

	if err := json.Unmarshal(dftJSON, &dftMap); err != nil {
		return fmt.Errorf("failed to unmarshal default config: %v", err)
	}

	if err := json.Unmarshal(userJSON, &userMap); err != nil {
		return fmt.Errorf("failed to unmarshal user config: %v", err)
	}

	// Deep merge user config into default config
	mergedMap := deepMergeJSON(dftMap, userMap)

	// Convert back to Config struct
	mergedJSON, err := json.Marshal(mergedMap)
	if err != nil {
		return fmt.Errorf("failed to marshal merged config: %v", err)
	}

	if err := json.Unmarshal(mergedJSON, &cfg); err != nil {
		return fmt.Errorf("failed to unmarshal merged config: %v", err)
	}

	log.Println("JSON deep merge completed successfully")

	// Default ping sites if missing or too short to be valid
	if len(cfg.PingSite0) <= 2 {
		cfg.PingSite0 = "1.1.1.1"
	}
	if len(cfg.PingSite1) <= 2 {
		cfg.PingSite1 = "photonicat.com"
	}

	// 5. Validation
	if cfg.ScreenDimmerTimeOnBatterySeconds < 0 {
		return fmt.Errorf("screen_dimmer_time_on_battery_seconds must be ≥ 0, got %d",
			cfg.ScreenDimmerTimeOnBatterySeconds)
	}
	if cfg.ScreenDimmerTimeOnDCSeconds < 0 {
		return fmt.Errorf("screen_dimmer_time_on_dc_seconds must be ≥ 0, got %d",
			cfg.ScreenDimmerTimeOnDCSeconds)
	}
	if cfg.ScreenMinBrightness < 0 || cfg.ScreenMinBrightness > 100 {
		return fmt.Errorf("screen_min_brightness must be in [0,100], got %d",
			cfg.ScreenMinBrightness)
	}
	if cfg.ScreenMaxBrightness < 0 || cfg.ScreenMaxBrightness > 100 {
		return fmt.Errorf("screen_max_brightness must be in [0,100], got %d",
			cfg.ScreenMaxBrightness)
	}
	if cfg.ScreenMinBrightness > cfg.ScreenMaxBrightness {
		return fmt.Errorf("screen_min_brightness (%d) cannot exceed screen_max_brightness (%d)",
			cfg.ScreenMinBrightness, cfg.ScreenMaxBrightness)
	}
	/*
	   for name, site := range map[string]string{"ping_site0": cfg.PingSite0, "ping_site1": cfg.PingSite1} {
	       if site != "" {
	           if u, err := url.ParseRequestURI(site); err != nil || u.Scheme == "" && u.Host == "" {
	               return fmt.Errorf("invalid %s: %q", name, site)
	           }
	       }
	   }*/

	cfgNumPages = len(cfg.DisplayTemplate.Elements)

	// Initialize totalNumPages based on ShowSms setting
	if cfg.ShowSms {
		// Will be updated by getSmsPages() goroutine
		totalNumPages = cfgNumPages + 1 // temporary, will be corrected when SMS data is loaded
	} else {
		totalNumPages = cfgNumPages
	}

	return nil
}

// hasShowSmsInUserConfig checks if the user config file explicitly contains show_sms field
func hasShowSmsInUserConfig() bool {
	var userConfigPath string
	localUserConfig := "user_config.json"

	// Determine which user config file to check
	if _, err := os.Stat(localUserConfig); err == nil {
		userConfigPath = localUserConfig
	} else {
		userConfigPath = ETC_USER_CONFIG_PATH
	}

	// Read the raw JSON
	raw, err := os.ReadFile(userConfigPath)
	if err != nil {
		return false
	}

	// Parse into a generic map to check for presence of show_sms
	var rawMap map[string]interface{}
	if err := secureUnmarshal(raw, &rawMap); err != nil {
		return false
	}

	_, exists := rawMap["show_sms"]
	return exists
}

func loadAllConfigsToVariables() {
	var err error
	localConfig := "config.json"
	userConfig := "user_config.json"

	if _, err = os.Stat(localConfig); err == nil {
		localConfigExists = true
		log.Println("Local config found at", localConfig)
	} else {
		log.Println("use", ETC_CONFIG_PATH)
	}

	if localConfigExists {
		cfg, err = loadConfig(localConfig)
		dftCfg, err = loadConfig(localConfig)
	} else {
		cfg, err = loadConfig(ETC_CONFIG_PATH)
		dftCfg, err = loadConfig(ETC_CONFIG_PATH)
	}

	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	} else {
		log.Println("CFG, DFTCFG: READ SUCCESS")
	}

	userConfigExists := false
	if _, err := os.Stat(userConfig); err == nil {
		userConfigExists = true
		log.Println("User config found at", userConfig)
	} else {
		log.Println("No user config found, try to use", ETC_USER_CONFIG_PATH)
	}

	if userConfigExists {
		userCfg, err = loadConfig(userConfig)
	} else {
		userCfg, err = loadConfig(ETC_USER_CONFIG_PATH)
	}

	if err != nil {
		//create a empty json file
		content := "{}"
		if err := os.WriteFile(ETC_USER_CONFIG_PATH, []byte(content), 0644); err != nil {
			log.Printf("could not write temp user config: %v", err)
		}
		log.Println("Created empty user config file at", ETC_USER_CONFIG_PATH)
		userCfg, err = loadConfig(ETC_USER_CONFIG_PATH)
	} else {
		log.Println("USER CFG: READ SUCCESS")
	}

	if userConfigExists && localConfigExists {
		err = mergeConfigs()
		if err != nil {
			log.Fatalf("Failed to merge configs: %v, using default config", err)
			cfg = dftCfg
		} else {
			log.Println("MERGE CFG: SUCCESS")
		}
	} else {
		cfg = dftCfg
		log.Println("NO USER CFG, Not Merging, using default config")
	}

	mergeConfigs()

}
