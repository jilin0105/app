package main

import (
	"bufio"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	// 必须添加移动端适配包
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
	MinLatency int // 毫秒
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

// ----------------------- 入口函数 (适配 Gomobile + 完整功能) -----------------------

func main() {
	app.Main(func(a app.App) {
		// 在后台协程中启动你的所有业务逻辑
		go func() {
			port := 8080
			defaultSpeedURL := "https://speed.cloudflare.com/__down?bytes=100000000"

			// Android 环境路径适配
			if os.Getenv("FILESDIR") != "" {
				dataDir = os.Getenv("FILESDIR")
			}

			fmt.Printf("服务启动中...\n")
			if err := StartServer(port, defaultSpeedURL); err != nil {
				fmt.Printf("服务错误: %v\n", err)
			}
		}()

		// 监听系统事件，确保 App 不被系统立即回收
		for e := range a.Events() {
			switch e := a.Filter(e).(type) {
			case lifecycle.Event:
				// 处理前后台切换逻辑（此处仅打印日志）
				fmt.Printf("App 生命周期状态: %v\n", e.To)
			}
		}
	})
}

// ----------------------- 以下是你原有的全部完整业务函数 -----------------------

func SetSpeedTestURL(u string) {
	speedTestURL = u
}

func SetDataDir(dir string) {
	dataDir = dir
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
			http.Error(w, "无法加载页面", http.StatusInternalServerError)
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
		fmt.Println("WebSocket 升级失败:", err)
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
				Port     int    `json:"port"`
				Delay    int    `json:"delay"`
				SpeedURL string `json:"speedUrl"`
			}
			json.Unmarshal(request.Data, &params)
			if params.SpeedURL != "" {
				SetSpeedTestURL(params.SpeedURL)
			}
			go runUnifiedTask(ws, params.IPType, params.Threads)

		case "start_test":
			var params struct {
				DC    string `json:"dc"`
				Port  int    `json:"port"`
				Delay int    `json:"delay"`
			}
			json.Unmarshal(request.Data, &params)
			go runDetailedTest(ws, params.DC, params.Port, params.Delay)

		case "start_speed_test":
			var params struct {
				IP       string `json:"ip"`
				Port     int    `json:"port"`
				SpeedURL string `json:"speedUrl"`
			}
			json.Unmarshal(request.Data, &params)
			if params.SpeedURL != "" {
				SetSpeedTestURL(params.SpeedURL)
			}
			go runSpeedTest(ws, params.IP, params.Port)
		}
	}
}

func sendWSMessage(ws *websocket.Conn, msgType string, data interface{}) {
	wsMutex.Lock()
	defer wsMutex.Unlock()
	msg := map[string]interface{}{
		"type": msgType,
		"data": data,
	}
	ws.WriteJSON(msg)
}

func initLocations() {
	filename := dataPath("locations.json")
	url := "https://www.baipiao.eu.org/cloudflare/locations"
	var locations []location
	var body []byte
	var err error

	if _, err = os.Stat(filename); os.IsNotExist(err) {
		resp, err := http.Get(url)
		if err != nil { return }
		defer resp.Body.Close()
		body, _ = io.ReadAll(resp.Body)
		saveToFile(filename, string(body))
	} else {
		body, _ = os.ReadFile(filename)
	}

	if err := json.Unmarshal(body, &locations); err != nil { return }
	locationMap = make(map[string]location)
	for _, loc := range locations {
		locationMap[loc.Iata] = loc
	}
}

func runUnifiedTask(ws *websocket.Conn, ipType int, scanMaxThreads int) {
	taskMutex.Lock()
	if isTaskRunning {
		taskMutex.Unlock()
		sendWSMessage(ws, "error", "已有任务正在运行")
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
	if _, err := os.Stat(filename); os.IsNotExist(err) {
		content, _ = getURLContent(apiURL)
		saveToFile(filename, content)
	} else {
		data, _ := os.ReadFile(filename)
		content = string(data)
	}

	ipList := parseIPList(content)
	if ipType == 6 {
		ipList = getRandomIPv6s(ipList)
	} else {
		ipList = getRandomIPv4s(ipList)
	}

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
			defer func() {
				<-thread
				wg.Done()
				scanMutex.Lock()
				count++
				curr := count
				scanMutex.Unlock()
				if curr%10 == 0 || curr == total {
					sendWSMessage(ws, "scan_progress", map[string]int{"current": curr, "total": total})
				}
			}()

			dialer := &net.Dialer{Timeout: 1 * time.Second}
			start := time.Now()
			conn, err := dialer.Dial("tcp", net.JoinHostPort(ip, "80"))
			if err != nil { return }
			defer conn.Close()
			tcpDuration := time.Since(start)

			client := http.Client{
				Transport: &http.Transport{Dial: func(n, a string) (net.Conn, error) { return conn, nil }},
				Timeout:   1 * time.Second,
			}

			req, _ := http.NewRequest("GET", "http://"+net.JoinHostPort(ip, "80")+"/cdn-cgi/trace", nil)
			req.Header.Set("User-Agent", "Mozilla/5.0")
			resp, err := client.Do(req)
			if err != nil { return }
			bodyBytes, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			bodyStr := string(bodyBytes)

			if strings.Contains(bodyStr, "uag=Mozilla/5.0") {
				regex := regexp.MustCompile(`colo=([A-Z]+)`)
				matches := regex.FindStringSubmatch(bodyStr)
				if len(matches) > 1 {
					dataCenter := matches[1]
					loc := locationMap[dataCenter]
					res := ScanResult{
						IP: ip, DataCenter: dataCenter, Region: loc.Region, City: loc.City,
						LatencyStr: fmt.Sprintf("%d ms", tcpDuration.Milliseconds()),
						TCPDuration: tcpDuration,
					}
					scanMutex.Lock()
					scanResults = append(scanResults, res)
					scanMutex.Unlock()
					sendWSMessage(ws, "scan_result", res)
				}
			}
		}(ip)
	}
	wg.Wait()

	scanMutex.Lock()
	sort.Slice(scanResults, func(i, j int) bool { return scanResults[i].TCPDuration < scanResults[j].TCPDuration })
	
	dcMap := make(map[string]*DataCenterInfo)
	for _, res := range scanResults {
		if _, ok := dcMap[res.DataCenter]; !ok {
			dcMap[res.DataCenter] = &DataCenterInfo{
				DataCenter: res.DataCenter, Region: res.Region, City: res.City, IPCount: 0, MinLatency: 999999,
			}
		}
		info := dcMap[res.DataCenter]
		info.IPCount++
		lat := int(res.TCPDuration.Milliseconds())
		if lat < info.MinLatency { info.MinLatency = lat }
	}
	scanMutex.Unlock()

	var dcList []DataCenterInfo
	for _, info := range dcMap { dcList = append(dcList, *info) }
	sort.Slice(dcList, func(i, j int) bool { return dcList[i].MinLatency < dcList[j].MinLatency })

	sendWSMessage(ws, "scan_complete_wait_dc", dcList)
}

func runDetailedTest(ws *websocket.Conn, selectedDC string, port int, delay int) {
	var testIPList []string
	scanMutex.Lock()
	for _, res := range scanResults {
		if selectedDC == "" || res.DataCenter == selectedDC {
			testIPList = append(testIPList, res.IP)
		}
	}
	scanMutex.Unlock()

	var results []TestResult
	var resMutex sync.Mutex
	var wg sync.WaitGroup
	wg.Add(len(testIPList))
	thread := make(chan struct{}, 50)
	var count int
	total := len(testIPList)

	for _, ip := range testIPList {
		thread <- struct{}{}
		go func(ip string) {
			defer func() { <-thread; wg.Done() }()
			
			dialer := &net.Dialer{Timeout: time.Duration(delay) * time.Millisecond}
			success := 0
			var totalLat time.Duration
			minL, maxL := time.Duration(math.MaxInt64), time.Duration(0)

			for i := 0; i < 10; i++ {
				start := time.Now()
				conn, err := dialer.Dial("tcp", net.JoinHostPort(ip, strconv.Itoa(port)))
				if err == nil {
					lat := time.Since(start)
					success++
					totalLat += lat
					if lat < minL { minL = lat }
					if lat > maxL { maxL = lat }
					conn.Close()
				}
			}
			if success > 0 {
				res := TestResult{IP: ip, MinLatency: minL, MaxLatency: maxL, AvgLatency: totalLat / time.Duration(success), LossRate: float64(10-success) / 10.0}
				resMutex.Lock()
				results = append(results, res)
				resMutex.Unlock()
				sendWSMessage(ws, "test_result", res)
			}
			
			scanMutex.Lock()
			count++
			curr := count
			scanMutex.Unlock()
			if curr%5 == 0 || curr == total {
				sendWSMessage(ws, "test_progress", map[string]int{"current": curr, "total": total})
			}
		}(ip)
	}
	wg.Wait()

	sort.Slice(results, func(i, j int) bool {
		if results[i].LossRate != results[j].LossRate { return results[i].LossRate < results[j].LossRate }
		if results[i].MinLatency/time.Millisecond != results[j].MinLatency/time.Millisecond {
			return results[i].MinLatency < results[j].MinLatency
		}
		return results[i].AvgLatency < results[j].AvgLatency
	})
	sendWSMessage(ws, "test_complete", results)
}

func runSpeedTest(ws *websocket.Conn, ip string, port int) {
	sendWSMessage(ws, "log", "开始测速...")
	scheme := "http"
	if port == 443 || port == 2053 || port == 2083 || port == 2087 || port == 2096 || port == 8443 {
		scheme = "https"
	}
	testURL := speedTestURL
	if !strings.HasPrefix(testURL, "http") { testURL = scheme + "://" + testURL }

	parsed, _ := url.Parse(testURL)
	client := http.Client{
		Transport: &http.Transport{Dial: func(n, a string) (net.Conn, error) {
			return net.Dial("tcp", net.JoinHostPort(ip, strconv.Itoa(port)))
		}},
		Timeout: 15 * time.Second,
	}

	resp, err := client.Get(testURL)
	if err != nil {
		sendWSMessage(ws, "speed_test_result", map[string]string{"ip": ip, "speed": "连接错误"})
		return
	}
	defer resp.Body.Close()

	buf := make([]byte, 32*1024)
	var total int64
	var maxS float64
	start := time.Now()
	timeout := time.After(5 * time.Second)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	lastB, lastT := int64(0), start
	for {
		select {
		case <-timeout: goto DONE
		case <-ticker.C:
			now := time.Now()
			diff := total - lastB
			curS := float64(diff) / now.Sub(lastT).Seconds() / 1024 / 1024
			if curS > maxS { maxS = curS }
			lastB, lastT = total, now
		default:
			n, err := resp.Body.Read(buf)
			total += int64(n)
			if err != nil { goto DONE }
		}
	}
DONE:
	sendWSMessage(ws, "speed_test_result", map[string]string{"ip": ip, "speed": fmt.Sprintf("%.2f MB/s", maxS)})
}

func getURLContent(u string) (string, error) {
	resp, err := http.Get(u)
	if err != nil { return "", err }
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), nil
}

func saveToFile(f, c string) error {
	os.MkdirAll(filepath.Dir(f), 0755)
	return os.WriteFile(f, []byte(c), 0644)
}

func parseIPList(c string) []string {
	scanner := bufio.NewScanner(strings.NewReader(c))
	var res []string
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" { res = append(res, line) }
	}
	return res
}

func getRandomIPv4s(list []string) []string {
	var res []string
	for _, s := range list {
		oct := strings.Split(strings.TrimSuffix(s, "/24"), ".")
		if len(oct) == 4 {
			oct[3] = fmt.Sprintf("%d", rand.Intn(256))
			res = append(res, strings.Join(oct, "."))
		}
	}
	return res
}

func getRandomIPv6s(list []string) []string {
	var res []string
	for _, s := range list {
		sec := strings.Split(strings.TrimSuffix(s, "/48"), ":")
		if len(sec) >= 3 {
			sec = sec[:3]
			for i := 0; i < 5; i++ { sec = append(sec, fmt.Sprintf("%x", rand.Intn(65536))) }
			res = append(res, strings.Join(sec, ":"))
		}
	}
	return res
}
