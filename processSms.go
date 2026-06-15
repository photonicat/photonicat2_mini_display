package main

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	//"os/exec"
	"io"
	"log"
	"net/http"

	"github.com/golang/freetype"
	"github.com/golang/freetype/truetype"
	"golang.org/x/image/font"
	"golang.org/x/image/math/fixed"
)

type SMS struct {
	Index     int    `json:"index"`
	Sender    string `json:"sender"`
	Timestamp string `json:"timestamp"`
	Reference int    `json:"reference,omitempty"`
	Part      int    `json:"part,omitempty"`
	Total     int    `json:"total,omitempty"`
	Content   string `json:"content"`
	Status    string `json:"status,omitempty"`
	Direction int    `json:"direction,omitempty"`
	To        string `json:"to,omitempty"`
}

var (
	lastSmsJsonContent string
	lastNumPages       int
	lastSuccessfulSmsJsonContent string
	
	// Memory pool for SMS images to reduce allocations
	smsImagePool = sync.Pool{
		New: func() interface{} {
			return image.NewRGBA(image.Rect(0, 0, 172, 270))
		},
	}

	smsFontOnce sync.Once
	smsFont     *truetype.Font
)

// getSmsFont parses the SMS font once and reuses it for every SMS redraw.
func getSmsFont() *truetype.Font {
	smsFontOnce.Do(func() {
		fontPath := assetsPrefix + "/assets/fonts/NotoSansMonoCJK-VF.ttf.ttc"
		log.Println("sms using font:", fontPath)
		fontBytes, err := os.ReadFile(fontPath)
		if err != nil {
			log.Printf("Error loading SMS font: %v", err)
			return
		}
		fnt, err := truetype.Parse(fontBytes)
		if err != nil {
			log.Printf("Error parsing SMS font: %v", err)
			return
		}
		smsFont = fnt
	})
	return smsFont
}

func collectAndDrawSms(cfg *Config) int {
	jsonContent := getJsonContent(cfg)

	if len(jsonContent) < 50 { //dummy message
		jsonContent = fmt.Sprintf("{\"msg\":[{\"sender\":\"System\",\"timestamp\":\"%s\",\"content\":\"No SMS - 无消息\"}]}", time.Now().Format("2006-01-02 15:04:05"))
	}

	if jsonContent == lastSmsJsonContent {
		log.Println("collectAndDrawSms: No new SMS, lastNumPages:", lastNumPages)
		return lastNumPages
	}

	lastSmsJsonContent = jsonContent
	lastNumPages = 0

	rawImgs, err := drawSmsFrJson(jsonContent, false, false)
	if err != nil {
		log.Println("Error drawing SMS:", err)
		return 0
	}

	// Return old SMS images to pool to prevent memory leaks
	for _, oldImg := range smsPagesImages {
		if oldImg != nil {
			// Clear the image before returning to pool
			draw.Draw(oldImg, oldImg.Bounds(), &image.Uniform{color.Black}, image.Point{}, draw.Src)
			smsImagePool.Put(oldImg)
		}
	}

	// prepare the global slice
	smsPagesImages = make([]*image.RGBA, len(rawImgs))
	for i, img := range rawImgs {
		// try a direct cast
		rgba, ok := img.(*image.RGBA)
		if !ok {
			log.Printf("Image %d is not *image.RGBA, converting…", i)
			// convert by drawing into a new RGBA
			b := img.Bounds()
			r := image.NewRGBA(b)
			draw.Draw(r, b, img, b.Min, draw.Src)
			rgba = r
		}
		smsPagesImages[i] = rgba
	}
	numPages := len(smsPagesImages)
	lastNumPages = numPages

	bumpDataEpoch() // SMS page images changed -> let the render loop refresh
	return numPages
}

func getJsonContent(cfg *Config) string {
	// Use config value or default to 10 if not specified
	smsLimit := 10
	if cfg != nil && cfg.SmsLimitForScreen > 0 {
		smsLimit = cfg.SmsLimitForScreen
	}

	// 1. Make the request
	url := fmt.Sprintf("http://localhost/api/v2/sms/list.json?n=%d", smsLimit)
	resp, err := localHTTPClient.Get(url)
	if err != nil {
		log.Printf("GET /sms/list.json failed: %v", err)
		// Return last successful result if we have one, otherwise return empty
		if lastSuccessfulSmsJsonContent != "" {
			log.Printf("Using cached SMS data due to request failure")
			return lastSuccessfulSmsJsonContent
		}
		return ""
	}
	defer resp.Body.Close()

	// 2. Check HTTP status
	if resp.StatusCode != http.StatusOK {
		log.Printf("unexpected HTTP status: %s", resp.Status)
		// Return last successful result if we have one, otherwise return empty
		if lastSuccessfulSmsJsonContent != "" {
			log.Printf("Using cached SMS data due to HTTP error")
			return lastSuccessfulSmsJsonContent
		}
		return ""
	}

	// 3. Read the body
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("reading response body failed: %v", err)
		// Return last successful result if we have one, otherwise return empty
		if lastSuccessfulSmsJsonContent != "" {
			log.Printf("Using cached SMS data due to read error")
			return lastSuccessfulSmsJsonContent
		}
		return ""
	}

	result := string(data)
	// Cache successful result
	if len(result) > 10 { // Basic validation that we got actual data
		lastSuccessfulSmsJsonContent = result
	}
	
	return result
}

func drawSmsFrJson(jsonContent string, savePng bool, drawPageNum bool) (imgs []image.Image, err error) {

	var smsData struct {
		Msg []SMS `json:"msg"`
	}
	if err := secureUnmarshal([]byte(jsonContent), &smsData); err != nil {
		log.Printf("Error parsing JSON: %v\n", err)
		return nil, err
	}

	// Load font (parsed once per process; the CJK collection is ~37MB)
	fnt := getSmsFont()
	if fnt == nil {
		return nil, fmt.Errorf("SMS font unavailable")
	}

	// Setup constants
	width, height := 172, 270
	fontSize := 12.0
	fontSizeTitle := 11.0
	lineSpacing := 1.2
	maxWidth := width - 8   // Adjusted for padding
	maxHeight := height - 8 // Adjusted for padding
	topPadding := 3.0
	xStart := 4
	layout := "2006-01-02 15:04:05"

	// Create font context
	fc := freetype.NewContext()
	fc.SetDPI(72)
	fc.SetFont(fnt)
	fc.SetFontSize(fontSize)
	// Color will be set dynamically during rendering based on sender
	fc.SetHinting(font.HintingFull)

	fcTitle := freetype.NewContext()
	fcTitle.SetDPI(72)
	fcTitle.SetFont(fnt)
	fcTitle.SetFontSize(fontSizeTitle)
	// Color will be set dynamically during rendering based on sender
	fcTitle.SetHinting(font.HintingFull)

	// Sort messages by timestamp descending
	sort.Slice(smsData.Msg, func(i, j int) bool {
		ti, err1 := time.Parse(layout, smsData.Msg[i].Timestamp)
		tj, err2 := time.Parse(layout, smsData.Msg[j].Timestamp)
		if err1 != nil || err2 != nil {
			return smsData.Msg[i].Timestamp > smsData.Msg[j].Timestamp
		}
		return ti.After(tj)
	})

	// Prepare pagination
	type Line struct {
		Text        string
		IsTitle     bool
		IsSentByMe  bool
		MessageSender string
	}

	type Page struct {
		Lines []Line
	}

	var pages []Page
	currentPage := Page{}
	currentHeight := topPadding

	faceMeasure := truetype.NewFace(fnt, &truetype.Options{
		Size:    fontSize,
		DPI:     72,
		Hinting: font.HintingFull,
	})

	for _, msg := range smsData.Msg {
		timestamp, _ := time.Parse(layout, msg.Timestamp)
		formattedTime := timestamp.Format("2006-01-02 15:04")
		
		// Change display: show "> number" instead of "me" for sent messages
		var displaySender string
		if msg.Sender == "me" && msg.To != "" {
			displaySender = "> " + msg.To
		} else {
			displaySender = msg.Sender
		}
		
		title := fmt.Sprintf("%s|%s", displaySender, formattedTime)
		message := msg.Content
		
		// Check if this is a sent message from "me" with status "SENT"
		isSentByMe := msg.Sender == "me" && msg.Status == "SENT"

		lines := wrapText(message, maxWidth, faceMeasure)

		// Add title
		titleHeight := fontSizeTitle * lineSpacing
		if currentHeight+titleHeight > float64(maxHeight) {
			// Save current page
			pages = append(pages, currentPage)
			// Reset for new page
			currentPage = Page{}
			currentHeight = topPadding
		}
		currentPage.Lines = append(currentPage.Lines, Line{Text: title, IsTitle: true, IsSentByMe: isSentByMe, MessageSender: msg.Sender})
		currentHeight += titleHeight

		// Add message lines
		for _, line := range lines {
			lineHeight := fontSize * lineSpacing
			if currentHeight+lineHeight > float64(maxHeight) {
				// Save current page
				pages = append(pages, currentPage)
				// Reset for new page
				currentPage = Page{}
				currentHeight = topPadding
				// Repeat title on new page
				currentPage.Lines = append(currentPage.Lines, Line{Text: title, IsTitle: true, IsSentByMe: isSentByMe, MessageSender: msg.Sender})
				currentHeight += titleHeight
			}
			currentPage.Lines = append(currentPage.Lines, Line{Text: line, IsTitle: false, IsSentByMe: isSentByMe, MessageSender: msg.Sender})
			currentHeight += lineHeight
		}
		// Add spacing after message
		spacingHeight := fontSize * lineSpacing
		if currentHeight+spacingHeight <= float64(maxHeight) {
			currentPage.Lines = append(currentPage.Lines, Line{Text: "", IsTitle: false, IsSentByMe: false, MessageSender: ""})
			currentHeight += spacingHeight
		}
	}

	// Add the last page if it has content
	if len(currentPage.Lines) > 0 {
		pages = append(pages, currentPage)
	}

	// Render pages to PNG
	for i, page := range pages {
		fmt.Printf("Rendering page %d\n", i)
		// Use pooled image buffer
		img := smsImagePool.Get().(*image.RGBA)
		// Clear the image before use
		draw.Draw(img, img.Bounds(), &image.Uniform{color.Black}, image.Point{}, draw.Src)

		fc.SetDst(img)
		fc.SetClip(img.Bounds())
		fcTitle.SetDst(img)
		fcTitle.SetClip(img.Bounds())

		y := topPadding
		for _, line := range page.Lines {
			if line.Text == "" {
				y += fontSize * lineSpacing
				continue
			}
			if line.IsTitle {
				timePrefix := ""
				lineTitle := strings.Split(line.Text, "|")
				sender := lineTitle[0]
				dateStr := strings.Split(lineTitle[1], " ")[0]
				timeStr := strings.Split(lineTitle[1], " ")[1]
				today := time.Now()
				yesterday := today.AddDate(0, 0, -1)

				if dateStr == today.Format("2006-01-02") {
					timePrefix = ""  // For today, show only time
				} else if dateStr == yesterday.Format("2006-01-02") {
					timePrefix = "Y-day"
				} else {
					// Parse the date string
					t, err := time.Parse("2006-01-02", dateStr)
					if err == nil {
						if t.Year() == today.Year() {
							timePrefix = t.Format("01-02") // MM DD
						} else {
							timePrefix = t.Format("2006-01-02")
						}
					} else {
						// fallback: just use the original string
						//timePrefix = dateStr
					}
				}
				var timeDisplay string
				if timePrefix == "" {
					timeDisplay = timeStr  // For today, show only time
				} else {
					timeDisplay = timePrefix + " " + timeStr
				}
				if len(sender) > 15 {
					sender = sender[:8] + "**" + sender[len(sender)-2:]
				}
				
				// Set color for sender based on whether message is sent by me
				if line.IsSentByMe {
					// Light green color for sent messages
					fcTitle.SetSrc(image.NewUniform(color.RGBA{144, 238, 144, 255}))
				} else {
					// Default white color for received messages
					fcTitle.SetSrc(image.NewUniform(color.RGBA{255, 255, 255, 255}))
				}
				
				// Draw sender left-aligned
				ptSender := freetype.Pt(xStart, int(y+fontSizeTitle))
				_, err := fcTitle.DrawString(sender, ptSender)
				if err != nil {
					fmt.Printf("Error drawing sender: %v\n", err)
					return nil, err
				}
				// Draw timeDisplay right-aligned
				faceTitle := truetype.NewFace(fnt, &truetype.Options{
					Size:    fontSizeTitle,
					DPI:     72,
					Hinting: font.HintingFull,
				})
				drawer := &font.Drawer{Face: faceTitle}
				adv := drawer.MeasureString(timeDisplay)
				timeWidth := int(adv >> 6)
				rightMargin := width - xStart - 5
				timeX := rightMargin - timeWidth
				ptTime := freetype.Pt(timeX, int(y+fontSizeTitle))
				_, err = fcTitle.DrawString(timeDisplay, ptTime)
				if err != nil {
					fmt.Printf("Error drawing time: %v\n", err)
					return nil, err
				}
				y += fontSizeTitle * lineSpacing
			} else {
				// Set color based on whether message is sent by me
				if line.IsSentByMe {
					// Light green color for sent messages
					fc.SetSrc(image.NewUniform(color.RGBA{144, 238, 144, 255}))
				} else {
					// Default yellow color for received messages
					fc.SetSrc(image.NewUniform(color.RGBA{255, 255, 0, 255}))
				}
				
				pt := freetype.Pt(xStart, int(y+fontSize))
				_, err := fc.DrawString(line.Text, pt)
				if err != nil {
					fmt.Printf("Error drawing string: %v\n", err)
					return nil, err
				}
				y += fontSize * lineSpacing
			}
		}
		imgs = append(imgs, img)

	}
	margin := 4 // margin from the right and bottom
	pageNumFontSize := 10.0
	total := len(imgs)
	for i, im := range imgs {
		if drawPageNum {
			pageStr := fmt.Sprintf("%d/%d", i+1, total)
			facePN := truetype.NewFace(fnt, &truetype.Options{Size: pageNumFontSize, DPI: 72, Hinting: font.HintingFull})
			dr := &font.Drawer{
				Dst:  im.(draw.Image), // assert back to draw.Image
				Src:  image.NewUniform(color.RGBA{200, 200, 200, 255}),
				Face: facePN,
			}
			w := int(dr.MeasureString(pageStr) >> 6)
			x := width/2 - w/2 //center
			y := height - margin/2
			dr.Dot = fixed.P(x, y)
			dr.DrawString(pageStr)
		}

		if savePng {
			fname := fmt.Sprintf("page_%d.png", i)
			f, err := os.Create(fname)
			if err != nil {
				fmt.Printf("Error creating file: %v\n", err)
				continue
			}
			if err := png.Encode(f, im); err != nil {
				fmt.Printf("Error encoding PNG: %v\n", err)
			}
			f.Close()
			fmt.Printf("Generated %s\n", fname)
		}
	}

	return imgs, nil
}

// isCJK reports whether r belongs to a CJK script.
func isCJK(r rune) bool {
	return unicode.In(r,
		unicode.Han,      // Chinese characters
		unicode.Hiragana, // Japanese hiragana
		unicode.Katakana, // Japanese katakana
		unicode.Hangul)   // Korean hangul
}

// wrapText splits text into lines that fit within maxWidth.
// - English/Latin words only break at spaces.
// - A single word that is too wide gets hyphenated.
// - CJK characters may break anywhere, and never get spaces around them.
func wrapText(text string, maxWidth int, face font.Face) []string {
	// Helper to detect CJK runes
	isCJK := func(r rune) bool {
		return unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul)
	}

	// 1) Tokenize into either:
	//    - runs of non-CJK (i.e. potential words)
	//    - single CJK runes
	var tokens []string
	var buf []rune
	flush := func() {
		if len(buf) > 0 {
			tokens = append(tokens, string(buf))
			buf = buf[:0]
		}
	}

	for _, r := range text {
		if unicode.IsSpace(r) {
			flush()
		} else if isCJK(r) {
			flush()
			tokens = append(tokens, string(r))
		} else {
			buf = append(buf, r)
		}
	}
	flush()

	// 2) Build lines
	var lines []string
	drawer := &font.Drawer{Face: face}
	current := ""

	for _, tok := range tokens {
		// decide separator: only a space if both neighbors are non-CJK
		sep := ""
		if current != "" {
			first := []rune(tok)[0]
			last := []rune(current)[len([]rune(current))-1]
			if !isCJK(first) && !isCJK(last) {
				sep = " "
			}
		}

		candidate := current + sep + tok
		if int(drawer.MeasureString(candidate)>>6) <= maxWidth {
			current = candidate
			continue
		}

		// if overflow
		if current != "" {
			lines = append(lines, current)
			current = ""
		}

		// tok alone too wide?
		if int(drawer.MeasureString(tok)>>6) <= maxWidth {
			current = tok
		} else {
			// hyphenate non-CJK words, else break CJK one rune at a time
			runes := []rune(tok)
			if !isCJK(runes[0]) && len(runes) > 1 {
				// hyphenate
				for i := 1; i < len(runes); i++ {
					part := string(runes[:i]) + "-"
					if int(drawer.MeasureString(part)>>6) > maxWidth {
						lines = append(lines, string(runes[:i-1])+"-")
						current = string(runes[i-1:])
						break
					}
				}
			} else {
				// CJK or single rune: char-by-char
				for _, r := range runes {
					s := string(r)
					if current == "" {
						current = s
					} else if int(drawer.MeasureString(current+s)>>6) <= maxWidth {
						current += s
					} else {
						lines = append(lines, current)
						current = s
					}
				}
			}
		}
	}

	if current != "" {
		lines = append(lines, current)
	}
	return lines
}

func getSmsPages() {
	firstFetchComplete := false
	startupRetryInterval := 3 * time.Second

	getSmsInterval := func() time.Duration {
		if !firstFetchComplete {
			return startupRetryInterval
		}
		if idleState == STATE_IDLE {
			return baseSmsInterval * time.Duration(idleMultiplier)
		}
		return baseSmsInterval
	}

	// Try to collect SMS immediately on startup
	if cfg.ShowSms {
		log.Println("Startup: attempting immediate SMS fetch...")
		lenSmsPagesImages = collectAndDrawSms(&cfg)
		if lenSmsPagesImages == 0 {
			lenSmsPagesImages = 1
		}
		if lenSmsPagesImages > 0 && lastSuccessfulSmsJsonContent != "" {
			firstFetchComplete = true
			log.Println("Startup: SMS fetch successful on first try, lenSmsPagesImages:", lenSmsPagesImages)
		} else {
			log.Println("Startup: SMS fetch failed, will retry every 3 seconds...")
		}
		totalNumPages = cfgNumPages + lenSmsPagesImages
	}

	ticker := time.NewTicker(getSmsInterval())
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if cfg.ShowSms {
				if !firstFetchComplete {
					log.Println("Startup retry: attempting SMS fetch...")
				}
				lenSmsPagesImages = collectAndDrawSms(&cfg)
				if lenSmsPagesImages == 0 {
					lenSmsPagesImages = 1
				}

				// Check if this is the first successful fetch
				if !firstFetchComplete && lastSuccessfulSmsJsonContent != "" {
					firstFetchComplete = true
					log.Println("First successful SMS fetch! Switching to normal interval. lenSmsPagesImages:", lenSmsPagesImages)
					ticker.Stop()
					ticker = time.NewTicker(getSmsInterval())
				} else {
					log.Println("collect lenSmsPagesImages:", lenSmsPagesImages)
				}

				totalNumPages = cfgNumPages + lenSmsPagesImages
			} else {
				// SMS disabled - only JSON config pages
				lenSmsPagesImages = 0
				totalNumPages = cfgNumPages
			}
		case <-intervalUpdateChan:
			ticker.Stop()
			ticker = time.NewTicker(getSmsInterval())
		}
	}
}
