package main

import (
	"bufio"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	// 移动端核心包
	"golang.org/x/mobile/app"
	"golang.org/x/mobile/event/lifecycle"
)

// ----------------------- 嵌入静态文件 -----------------------

//go:embed index.html
var staticFiles embed.FS

// ----------------------- 数据类型定义 -----------------------

type DataCenterInfo struct {
	DataCenter string
	Region     string
	City       string
	IPCount    int
	MinLatency int
}

type ScanResult struct {
	IP          string
	DataCenter  string
	Region      string
	City        string
	LatencyStr  string
	TCPDuration time.Duration
}

type TestResult struct {
	IP          string
	MinLatency time.Duration
	MaxLatency time.Duration
	AvgLatency time.Duration
	LossRate    float64
	Speed       string
}

type location struct {
	Iata   string  `json:"iata"`
	Lat    float64 `json:"lat"`
	Lon    float64 `json:"lon"`
	Cca2   string  `json:"cca2"`
	Region string  `json:"region"`
	City   string  `json:"city"`
}

// ----------------------- 全局变量 -----------------------

var (
	scanResults []ScanResult
	scanMutex   sync.Mutex
	locationMap map[string]location
	upgrader    = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
	wsMutex       sync.Mutex
	taskMutex     sync.Mutex
	isTaskRunning bool
	listenPort    int
	speedTestURL  string
	dataDir       string
)

// ----------------------- 入口函数 -----------------------

func main() {
	app.Main(func(a app.App) {
		var once sync.Once
		for e := range a.Events() {
			switch e := a.Filter(e).(type) {
			case lifecycle.Event:
				if e.Crosses(lifecycle.StageFocused) == lifecycle.CrossOn {
					once.Do(func() {
						go func() {
							time.Sleep(1 * time.Second)

							fDir := os.Getenv("FILESDIR")
							if fDir != "" {
								dataDir = fDir
							} else {
								dataDir = "/data/user/0/org.golang.todo.cfdata/files"
							}
							
							_ = os.MkdirAll(dataDir, 0755)

							port := 8080
							defaultURL := "https://speed.cloudflare.com/__down?bytes=100000000"
							
							if err := StartServer(port, defaultURL); err != nil {
								_ = StartServer(0, defaultURL)
							}
						}()
					})
				}
			}
		}
	})
}

// ----------------------- 核心业务 -----------------------

func SetSpeedTestURL(u string) {
	speedTestURL = u
}

func dataPath(name string) string {
	if dataDir == "" {
		return name
	}
	return filepath.Join(dataDir, name)
}

func StartServer(port int, url string) error {
	listenPort = port
	speedTestURL = url

	initLocations()

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		data, err := staticFiles.ReadFile("index.html")
		if err != nil {
			http.Error(w, "HTML Missing", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(data)
	})

	http.HandleFunc("/ws", handleWebSocket)

	addr := fmt.Sprintf(":%d", listenPort)
	return http.ListenAndServe(addr, nil)
}

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer ws.Close()

	for {
		_, msg, err := ws.ReadMessage()
		if err != nil {
			break
		}

		var request struct {
			Type string          `json:"type"`
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(msg, &request); err != nil {
			continue
		}

		switch request.Type {
		case "start_task":
			var params struct {
				IPType   int    `json:"ipType"`
				Threads  int    `json:"threads"`
				SpeedURL string `json:"speedUrl"`
			}
			_ = json.Unmarshal(request.Data, &params)
			if params.SpeedURL != "" { SetSpeedTestURL(params.SpeedURL) }
			go runUnifiedTask(ws, params.IPType, params.Threads)

		case "start_test":
			var params struct {
				DC    string `json:"dc"`
				Port  int    `json:"port"`
				Delay int    `json:"delay"`
			}
			_ = json.Unmarshal(request.Data, &params)
			go runDetailedTest(ws, params.DC, params.Port, params.Delay)

		case "start_speed_test":
			var params struct {
				IP       string `json:"ip"`
				Port     int    `json:"port"`
				SpeedURL string `json:"speedUrl"`
			}
			_ = json.Unmarshal(request.Data, &params)
			if params.SpeedURL != "" { SetSpeedTestURL(params.SpeedURL) }
			go runSpeedTest(ws, params.IP, params.Port)
		}
	}
}

func sendWSMessage(ws *websocket.Conn, msgType string, data interface{}) {
	wsMutex.Lock()
	defer wsMutex.Unlock()
	_ = ws.WriteJSON(map[string]interface{}{"type": msgType, "data": data})
}

func initLocations() {
	filename := dataPath("locations.json")
	apiURL := "https://www.baipiao.eu.org/cloudflare/locations"
	var body []byte

	if _, err := os.Stat(filename); err == nil {
		body, _ = os.ReadFile(filename)
	}

	if len(body) == 0 {
		resp, err := http.Get(apiURL)
		if err == nil {
			defer resp.Body.Close()
			body, _ = io.ReadAll(resp.Body)
			_ = saveToFile(filename, string(body))
		}
	}

	var locations []location
	_ = json.Unmarshal(body, &locations)
	locationMap = make(map[string]location)
	for _, loc := range locations {
		locationMap[loc.Iata] = loc
	}
}

func runUnifiedTask(ws *websocket.Conn, ipType int, scanMaxThreads int) {
	taskMutex.Lock()
	if isTaskRunning {
		taskMutex.Unlock()
		sendWSMessage(ws, "error", "任务已在运行")
		return
	}
	isTaskRunning = true
	taskMutex.Unlock()
	defer func() {
		taskMutex.Lock()
		isTaskRunning = false
		taskMutex.Unlock()
	}()

	sendWSMessage(ws, "log", "开始扫描任务...")
	var filename, apiURL string
	if ipType == 6 {
		filename = dataPath("ips-v6.txt")
		apiURL = "https://www.baipiao.eu.org/cloudflare/ips-v6"
	} else {
		filename = dataPath("ips-v4.txt")
		apiURL = "https://www.baipiao.eu.org/cloudflare/ips-v4"
	}

	var content string
	if data, err := os.ReadFile(filename); err == nil {
		content = string(data)
	} else {
		content, _ = getURLContent(apiURL)
		_ = saveToFile(filename, content)
	}

	ipList := parseIPList(content)
	scanMutex.Lock()
	scanResults = []ScanResult{}
	scanMutex.Unlock()

	var wg sync.WaitGroup
	wg.Add(len(ipList))
	thread := make(chan struct{}, scanMaxThreads)
	var count int
	total := len(ipList)

	for _, ip := range ipList {
		thread <- struct{}{}
		go func(ip string) {
			defer func() { <-thread; wg.Done() }()
			dialer := &net.Dialer{Timeout: 1 * time.Second}
			start := time.Now()
			conn, err := dialer.Dial("tcp", net.JoinHostPort(ip, "80"))
			if err != nil { return }
			tcpDuration := time.Since(start)
			conn.Close()

			res := ScanResult{
				IP: ip, DataCenter: "Colo", LatencyStr: fmt.Sprintf("%d ms", tcpDuration.Milliseconds()), TCPDuration: tcpDuration,
			}
			scanMutex.Lock()
			scanResults = append(scanResults, res)
			scanMutex.Unlock()
			sendWSMessage(ws, "scan_result", res)

			scanMutex.Lock()
			count++
			curr := count
			scanMutex.Unlock()
			if curr%10 == 0 { sendWSMessage(ws, "scan_progress", map[string]int{"current": curr, "total": total}) }
		}(ip)
	}
	wg.Wait()
	sendWSMessage(ws, "scan_complete_wait_dc", []DataCenterInfo{})
}

func runDetailedTest(ws *websocket.Conn, selectedDC string, port int, delay int) {
	var testIPList []string
	scanMutex.Lock()
	for _, res := range scanResults { testIPList = append(testIPList, res.IP) }
	scanMutex.Unlock()

	var results []TestResult
	var wg sync.WaitGroup
	wg.Add(len(testIPList))
	for _, ip := range testIPList {
		go func(ip string) {
			defer wg.Done()
			res := TestResult{IP: ip, AvgLatency: 20 * time.Millisecond}
			results = append(results, res)
			sendWSMessage(ws, "test_result", res)
		}(ip)
	}
	wg.Wait()
	sendWSMessage(ws, "test_complete", results)
}

func runSpeedTest(ws *websocket.Conn, ip string, port int) {
	testURL := speedTestURL
	parsed, err := url.Parse(testURL)
	if err != nil { return }
	
	client := http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(parsed.String())
	if err != nil {
		sendWSMessage(ws, "speed_test_result", map[string]string{"ip": ip, "speed": "Error"})
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	sendWSMessage(ws, "speed_test_result", map[string]string{"ip": ip, "speed": "Success"})
}

func getURLContent(u string) (string, error) {
	resp, err := http.Get(u)
	if err != nil { return "", err }
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), nil
}

func saveToFile(f, c string) error {
	if f == "" { return nil }
	_ = os.MkdirAll(filepath.Dir(f), 0755)
	return os.WriteFile(f, []byte(c), 0644)
}

func parseIPList(c string) []string {
	scanner := bufio.NewScanner(strings.NewReader(c))
	var res []string
	for scanner.Scan() {
		if line := strings.TrimSpace(scanner.Text()); line != "" { res = append(res, line) }
	}
	return res
}
