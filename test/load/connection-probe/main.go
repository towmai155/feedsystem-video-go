// connection-probe compares simultaneous new HTTP connections through the
// Windows published port and directly within the isolated Docker network.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"sync"
	"time"
)

func main() {
	base := flag.String("url", "http://127.0.0.1:18088", "isolated fixture URL")
	flag.Parse()
	for round := 0; round < 5; round++ {
		transport := &http.Transport{MaxConnsPerHost: 300, MaxIdleConnsPerHost: 300, DisableCompression: true}
		client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
		start := make(chan struct{})
		var wg sync.WaitGroup
		var mu sync.Mutex
		counts := map[string]int{}
		for i := 0; i < 300; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				key := "200 valid"
				resp, err := client.Post(*base+"/feed/listLatest", "application/json", bytes.NewBufferString(`{"limit":10}`))
				if err != nil {
					key = err.Error()
				} else {
					var data struct {
						Videos []json.RawMessage `json:"video_list"`
					}
					body, readErr := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
					resp.Body.Close()
					if readErr != nil {
						key = readErr.Error()
					} else if resp.StatusCode != 200 || json.Unmarshal(body, &data) != nil || len(data.Videos) != 10 {
						key = fmt.Sprintf("invalid response status=%d", resp.StatusCode)
					}
				}
				mu.Lock()
				counts[key]++
				mu.Unlock()
			}()
		}
		close(start)
		wg.Wait()
		transport.CloseIdleConnections()
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"os": runtime.GOOS, "url": *base, "round": round + 1, "requests": 300, "outcomes": counts})
	}
}
