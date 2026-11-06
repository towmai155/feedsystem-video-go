// p0bench exercises the real API and Worker in an isolated local environment.
// It refuses to use any database/Redis/broker other than test/load/compose.p0.yml.
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"feedsystem_video_go/internal/account"
	"feedsystem_video_go/internal/auth"
	"feedsystem_video_go/internal/config"
	appdb "feedsystem_video_go/internal/db"
	"feedsystem_video_go/internal/events"
	"feedsystem_video_go/internal/video"

	redis "github.com/redis/go-redis/v9"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const (
	baseURL = "http://127.0.0.1:18088"
	users   = 6000
	videos  = 50000
	secret  = "p0-benchmark-isolated-local-signing-key"
)

type observation struct {
	Latency float64
	Status  int
	Valid   bool
	Error   string
	Index   int
	Started time.Time
}
type result struct {
	Name           string         `json:"name"`
	Concurrency    int            `json:"concurrency"`
	Requests       int            `json:"requests"`
	OK             int            `json:"ok"`
	Seconds        float64        `json:"seconds"`
	RPS            float64        `json:"rps"`
	GoodRPS        float64        `json:"good_rps"`
	P50            float64        `json:"p50_ms"`
	P95            float64        `json:"p95_ms"`
	P99            float64        `json:"p99_ms"`
	Status         map[int]int    `json:"status"`
	SemanticErrors int            `json:"semantic_errors"`
	Errors         []string       `json:"error_samples,omitempty"`
	Verification   map[string]any `json:"verification,omitempty"`
	Samples        []snapshot     `json:"samples"`
	E2EP95         float64        `json:"e2e_p95_ms,omitempty"`
	E2EP99         float64        `json:"e2e_p99_ms,omitempty"`
	observations   []observation
	firstSeen      map[int]time.Time
}
type snapshot struct {
	At                  string            `json:"at"`
	MySQL               map[string]string `json:"mysql"`
	Queues              []queue           `json:"queues"`
	Redis               string            `json:"redis"`
	APIGoroutines       string            `json:"api_goroutines"`
	WorkerGoroutines    string            `json:"worker_goroutines"`
	GeneratorGoroutines int               `json:"generator_goroutines"`
	Error               string            `json:"error,omitempty"`
	APIPool             *sql.DBStats      `json:"api_pool,omitempty"`
	WorkerPool          *sql.DBStats      `json:"worker_pool,omitempty"`
	OutboxPending       int64             `json:"outbox_pending"`
}
type queue struct {
	Name      string `json:"name"`
	Ready     int    `json:"messages_ready"`
	Unacked   int    `json:"messages_unacknowledged"`
	Consumers int    `json:"consumers"`
}
type report struct {
	Started     string         `json:"started"`
	Finished    string         `json:"finished"`
	Environment map[string]any `json:"environment"`
	Results     []*result      `json:"results"`
	Checks      map[string]any `json:"checks"`
}
type bench struct {
	db      *gorm.DB
	rdb     *redis.Client
	http    *http.Client
	admin   *http.Client
	tokens  []string
	created time.Time
	out     string
	report  report
	project string
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
func main() {
	seconds := flag.Int("seconds", 20, "duration per read stage")
	levelsText := flag.String("levels", "25,100,300", "closed-loop concurrent clients")
	writeRequests := flag.Int("write-requests", 2000, "unique actors per write stage; max 6000")
	out := flag.String("output", "../test/load/results/latest", "results directory")
	readOnly := flag.Bool("read-only", false, "run only read scenarios and pagination checks")
	faultChecks := flag.Bool("fault-checks", false, "run bounded outage/replay checks before load; only feedsystem-p0-reliable projects")
	externalProcesses := flag.Bool("external-processes", false, "use API/Worker already running in isolated test containers")
	casesText := flag.String("cases", "feed_anon_hot,feed_auth_hot,feed_anon_cold,detail_hot,detail_distributed,detail_expiring", "comma-separated read scenarios")
	project := flag.String("compose-project", "feedsystem-p0", "dedicated Compose project used for resource snapshots")
	flag.Parse()
	if *faultChecks && !strings.HasPrefix(*project, "feedsystem-p0-reliable") {
		log.Fatal("fault checks require a dedicated feedsystem-p0-reliable project")
	}
	if *faultChecks && (*readOnly || *externalProcesses) {
		log.Fatal("fault checks require managed processes and a writable isolated fixture")
	}
	cases := strings.Split(*casesText, ",")
	for _, name := range cases {
		switch name {
		case "feed_anon_hot", "feed_auth_hot", "feed_anon_cold", "detail_hot", "detail_distributed", "detail_expiring":
		default:
			log.Fatalf("unknown read scenario %q", name)
		}
	}
	if *seconds < 1 || *seconds > 120 || *writeRequests < 1 || *writeRequests > users {
		log.Fatal("invalid bounded test parameters")
	}
	var levels []int
	for _, s := range strings.Split(*levelsText, ",") {
		n, e := strconv.Atoi(s)
		must(e)
		if n < 1 || n > 500 {
			log.Fatal("concurrency must be 1..500")
		}
		levels = append(levels, n)
	}
	if len(levels) > 3 {
		log.Fatal("at most three levels (preserves per-account write limits)")
	}
	absOut, err := filepath.Abs(*out)
	must(err)
	must(os.MkdirAll(absOut, 0755))
	cfg, err := config.Load("../test/load/config.p0.yaml")
	must(err)
	if cfg.Database.Host != "127.0.0.1" || cfg.Database.Port != 13307 || cfg.Database.DBName != "p0_loadtest" || cfg.Redis.Port != 16379 || cfg.RabbitMQ.Port != 15673 || cfg.Server.Port != 18088 {
		log.Fatal("refusing non-isolated configuration; clear conflicting environment overrides")
	}
	db, err := appdb.NewDB(cfg.Database)
	must(err)
	db = db.Session(&gorm.Session{Logger: logger.Default.LogMode(logger.Silent)})
	defer appdb.CloseDB(db)
	sqlDB, err := db.DB()
	must(err)
	sqlDB.SetMaxOpenConns(8)
	sqlDB.SetMaxIdleConns(4)
	transport := &http.Transport{MaxIdleConns: 1200, MaxIdleConnsPerHost: 600, MaxConnsPerHost: 600, IdleConnTimeout: 90 * time.Second, DisableCompression: true}
	b := &bench{db: db, rdb: redis.NewClient(&redis.Options{Addr: "127.0.0.1:16379"}), http: &http.Client{Transport: transport, Timeout: 3 * time.Second}, admin: &http.Client{Timeout: 3 * time.Second}, out: absOut, project: *project}
	defer b.rdb.Close()
	defer transport.CloseIdleConnections()
	b.report = report{Started: time.Now().Format(time.RFC3339), Checks: map[string]any{}, Environment: map[string]any{"go": runtime.Version(), "os": runtime.GOOS, "arch": runtime.GOARCH, "logical_cpus": runtime.NumCPU(), "accounts": users, "videos": videos, "base_url": baseURL, "read_seconds": *seconds, "levels": levels, "write_requests_per_stage": *writeRequests, "model": "closed-loop, no think time; same-host generator and API; not an arrival-rate capacity/SLA certification", "api_request_timeout_seconds": 3, "e2e_poll_interval_ms": 250, "secret": "fixture tokens omitted from artifacts"}}
	b.report.Environment["read_only"] = *readOnly
	b.report.Environment["cases"] = cases
	b.report.Environment["compose_project"] = *project
	b.report.Environment["external_processes"] = *externalProcesses
	b.report.Environment["fault_checks"] = *faultChecks
	defer func() { b.report.Finished = time.Now().Format(time.RFC3339); b.save() }()
	must(os.Setenv("JWT_SECRET", secret))
	b.seed()
	if !*externalProcesses {
		stopAPI := b.start("api", filepath.Join(".run", "p0", "api.exe"))
		defer stopAPI()
	}
	b.ready()
	if !*externalProcesses {
		stopWorker := b.start("worker", filepath.Join(".run", "p0", "worker.exe"))
		defer stopWorker()
	}
	b.waitConsumers()
	b.report.Checks["pagination_before"] = b.pagination()
	if *faultChecks {
		b.faultChecks()
	}
	for _, c := range levels {
		for _, name := range cases {
			ctx, cancel := context.WithCancel(context.Background())
			if name == "detail_expiring" {
				must(b.rdb.Del(ctx, "v1:video:detail:id=50000").Err())
				go func() {
					t := time.NewTicker(time.Second)
					defer t.Stop()
					for {
						select {
						case <-ctx.Done():
							return
						case <-t.C:
							_ = b.rdb.Del(ctx, "v1:video:detail:id=50000").Err()
						}
					}
				}()
			}
			r := b.run(name, c, time.Duration(*seconds)*time.Second, 0, func(i int) observation { return b.read(name, i) })
			cancel()
			b.report.Results = append(b.report.Results, r)
			b.save()
			b.resources(name, c)
		}
	}
	if *readOnly {
		b.report.Checks["pagination_after"] = b.pagination()
		b.report.Checks["final_snapshot"] = b.snapshot()
		log.Printf("P0 read-only run complete: %s", b.out)
		return
	}
	for level, c := range levels {
		for _, kind := range []string{"like", "comment"} {
			id := uint(videos + 1 + level*2)
			if kind == "comment" {
				id++
			}
			name := fmt.Sprintf("%s_hot_%d", kind, id)
			v := video.Video{ID: id, AuthorID: users + 1, Username: "p0_author", Title: name, PlayURL: "/static/fixture.mp4", CoverURL: "/static/fixture.jpg", CreateTime: time.Now()}
			must(db.Create(&v).Error)
			r := b.run(name, c, 0, *writeRequests, func(i int) observation { return b.write(kind, id, i, i, name) }, b.observer(kind, id, name))
			b.verifyWrite(r, kind, id, *writeRequests, name, false)
			b.report.Results = append(b.report.Results, r)
			b.save()
			b.resources(name, c)
		}
	}
	// A bounded race test complements sustained load; repeated accepted requests
	// must not multiply the single user's like, popularity, or notification.
	id := uint(videos + 100)
	must(db.Create(&video.Video{ID: id, AuthorID: users + 1, Username: "p0_author", Title: "duplicate_like", PlayURL: "/static/f.mp4", CoverURL: "/static/f.jpg", CreateTime: time.Now()}).Error)
	r := b.run("duplicate_like_correctness", 20, 0, 20, func(i int) observation { return b.write("like", id, i, users-1, "duplicate") }, b.observer("like", id, "duplicate"))
	b.verifyWrite(r, "like", id, 1, "duplicate", true)
	b.report.Results = append(b.report.Results, r)
	b.report.Checks["pagination_after"] = b.pagination()
	b.report.Checks["final_snapshot"] = b.snapshot()
	log.Printf("P0 run complete: %s", b.out)
}

func (b *bench) seed() {
	log.Printf("Preparing isolated fixture: %d accounts, %d videos", users, videos)
	must(appdb.AutoMigrate(b.db))
	var existing int64
	must(b.db.Model(&account.Account{}).Count(&existing).Error)
	if existing != 0 {
		panic("isolated DB is not empty; create a fresh feedsystem-p0 stack; refusing to delete data")
	}
	ctx := context.Background()
	must(b.rdb.Ping(ctx).Err())
	if n, e := b.rdb.DBSize(ctx).Result(); e != nil || n != 0 {
		panic("isolated Redis must be empty")
	}
	b.tokens = make([]string, users)
	accounts := make([]account.Account, 0, users+1)
	for i := 0; i <= users; i++ {
		id := uint(i + 1)
		username := fmt.Sprintf("p0_user_%05d", id)
		sid, e := auth.GenerateSessionID()
		must(e)
		token, e := auth.GenerateToken(id, username, sid)
		must(e)
		if i < users {
			b.tokens[i] = token
		}
		exp := time.Now().Add(7 * 24 * time.Hour)
		accounts = append(accounts, account.Account{ID: id, Username: username, Password: "fixture-no-password-login", SessionID: sid, Token: token, RefreshExpiresAt: &exp})
	}
	must(b.db.CreateInBatches(accounts, 250).Error)
	b.created = time.Now().UTC().Truncate(time.Second).Add(-time.Duration(videos+10) * time.Second)
	for begin := 1; begin <= videos; begin += 500 {
		batch := make([]video.Video, 0, 500)
		for i := begin; i < begin+500 && i <= videos; i++ {
			batch = append(batch, video.Video{ID: uint(i), AuthorID: users + 1, Username: "p0_author", Title: fmt.Sprintf("fixture_%d", i), Description: strings.Repeat("fixture ", 20), PlayURL: "/static/fixture.mp4", CoverURL: "/static/fixture.jpg", CreateTime: b.created.Add(time.Duration(i) * time.Second)})
		}
		must(b.db.CreateInBatches(batch, 500).Error)
	}
	items := make([]redis.Z, 0, 1000)
	for i := videos - 999; i <= videos; i++ {
		items = append(items, redis.Z{Member: strconv.Itoa(i), Score: float64(b.created.Add(time.Duration(i) * time.Second).UnixMilli())})
	}
	must(b.rdb.ZAdd(ctx, "v1:feed:global_timeline", items...).Err())
}

func (b *bench) start(name, path string) func() {
	abs, err := filepath.Abs(path)
	must(err)
	if _, err = os.Stat(abs); err != nil {
		panic(fmt.Sprintf("build %s first: %v", name, err))
	}
	file, err := os.Create(filepath.Join(b.out, name+".log"))
	must(err)
	cmd := exec.Command(abs)
	cmd.Stdout = file
	cmd.Stderr = file
	// Pin every overridden config field so a developer's existing shell cannot
	// accidentally redirect these child processes to their regular database.
	env := map[string]string{"CONFIG_PATH": "../test/load/config.p0.yaml", "GIN_MODE": "release", "JWT_SECRET": secret, "SERVER_PORT": "18088", "MYSQL_HOST": "127.0.0.1", "MYSQL_PORT": "13307", "MYSQL_USER": "root", "MYSQL_PASSWORD": "p0-local-only", "MYSQL_ROOT_PASSWORD": "p0-local-only", "MYSQL_DATABASE": "p0_loadtest", "REDIS_HOST": "127.0.0.1", "REDIS_PORT": "16379", "REDIS_PASSWORD": "", "REDIS_DB": "0", "RABBITMQ_HOST": "127.0.0.1", "RABBITMQ_PORT": "15673", "RABBITMQ_USER": "p0", "RABBITMQ_PASS": "p0-local-only"}
	for _, item := range os.Environ() {
		k, _, _ := strings.Cut(item, "=")
		if _, override := env[k]; !override {
			cmd.Env = append(cmd.Env, item)
		}
	}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	must(cmd.Start())
	log.Printf("Started %s pid=%d", name, cmd.Process.Pid)
	return func() { _ = cmd.Process.Kill(); _ = cmd.Wait(); _ = file.Close() }
}
func (b *bench) ready() {
	for i := 0; i < 60; i++ {
		o := b.read("detail_hot", 0)
		if o.Valid {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	panic("API did not become ready; inspect api.log")
}
func (b *bench) queues() ([]queue, error) {
	req, e := http.NewRequest("GET", "http://127.0.0.1:15674/api/queues/%2F", nil)
	if e != nil {
		return nil, e
	}
	req.SetBasicAuth("p0", "p0-local-only")
	resp, e := b.admin.Do(req)
	if e != nil {
		return nil, e
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("queue API: %d", resp.StatusCode)
	}
	var q []queue
	e = json.NewDecoder(resp.Body).Decode(&q)
	return q, e
}
func (b *bench) waitConsumers() {
	for i := 0; i < 60; i++ {
		q, e := b.queues()
		if e == nil {
			found := 0
			for _, v := range q {
				if (v.Name == "like.events" || v.Name == "comment.events" || v.Name == "video.popularity.events") && v.Consumers > 0 {
					found++
				}
			}
			if found == 3 {
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	panic("Worker consumers not ready")
}
func (b *bench) request(path string, payload any, token string) (int, []byte, error) {
	body, e := json.Marshal(payload)
	if e != nil {
		return 0, nil, e
	}
	req, e := http.NewRequest("POST", baseURL+path, bytes.NewReader(body))
	if e != nil {
		return 0, nil, e
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, e := b.http.Do(req)
	if e != nil {
		return 0, nil, e
	}
	defer resp.Body.Close()
	data, e := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	return resp.StatusCode, data, e
}
func (b *bench) read(name string, i int) observation {
	path := "/video/getDetail"
	payload := map[string]any{"id": videos}
	token := ""
	if strings.HasPrefix(name, "feed_") {
		path = "/feed/listLatest"
		payload = map[string]any{"limit": 10}
		if name == "feed_auth_hot" {
			token = b.tokens[i%users]
		}
		if name == "feed_anon_cold" {
			payload["latest_time"] = b.created.Add(time.Duration(2000+i%45000) * time.Second).UnixMilli()
		}
	}
	if name == "detail_distributed" {
		payload["id"] = 1 + i%videos
	}
	status, data, e := b.request(path, payload, token)
	o := observation{Status: status, Valid: status == 200}
	if e != nil {
		o.Valid = false
		o.Error = e.Error()
		return o
	}
	if status != 200 {
		o.Error = string(data)
		return o
	}
	if strings.HasPrefix(name, "feed_") {
		var resp struct {
			VideoList []struct {
				ID uint `json:"id"`
			} `json:"video_list"`
			Next int64 `json:"next_time"`
		}
		e = json.Unmarshal(data, &resp)
		if e != nil || len(resp.VideoList) != 10 || resp.Next == 0 {
			o.Valid = false
			o.Error = "invalid feed length/cursor"
		} else {
			seen := map[uint]bool{}
			for _, v := range resp.VideoList {
				if v.ID == 0 || seen[v.ID] {
					o.Valid = false
					o.Error = "duplicate/zero feed id"
				}
				seen[v.ID] = true
			}
		}
	} else {
		var v video.Video
		e = json.Unmarshal(data, &v)
		expected := uint(videos)
		if name == "detail_distributed" {
			expected = uint(1 + i%videos)
		}
		if e != nil || v.ID != expected {
			o.Valid = false
			o.Error = "wrong detail id"
		}
	}
	return o
}
func (b *bench) write(kind string, id uint, i, actor int, marker string) observation {
	path := "/like/like"
	payload := map[string]any{"video_id": id}
	if kind == "comment" {
		path = "/comment/publish"
		payload["content"] = fmt.Sprintf("%s:%d", marker, i)
	}
	status, data, e := b.request(path, payload, b.tokens[actor%users])
	o := observation{Status: status, Valid: status == 200}
	if e != nil {
		o.Valid = false
		o.Error = e.Error()
	} else if status != 200 {
		o.Error = string(data)
	} else {
		var resp map[string]any
		if json.Unmarshal(data, &resp) != nil || resp["message"] == nil {
			o.Valid = false
			o.Error = "invalid write response"
		}
	}
	return o
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(math.Ceil(float64(len(sorted))*p)) - 1
	if i < 0 {
		i = 0
	}
	return sorted[i]
}
func (b *bench) observer(kind string, id uint, marker string) func() map[int]time.Time {
	return func() map[int]time.Time {
		seen := map[int]time.Time{}
		if kind == "like" {
			var rows []struct{ AccountID uint }
			if b.db.Table("likes").Select("account_id").Where("video_id = ?", id).Find(&rows).Error == nil {
				at := time.Now()
				for _, row := range rows {
					seen[int(row.AccountID)-1] = at
				}
			}
		} else {
			var rows []struct{ Content string }
			if b.db.Table("comments").Select("content").Where("video_id = ?", id).Find(&rows).Error == nil {
				at := time.Now()
				for _, row := range rows {
					key, e := strconv.Atoi(strings.TrimPrefix(row.Content, marker+":"))
					if e == nil {
						seen[key] = at
					}
				}
			}
		}
		return seen
	}
}
func (b *bench) run(name string, c int, d time.Duration, n int, fn func(int) observation, observer ...func() map[int]time.Time) *result {
	log.Printf("START %-27s concurrency=%d duration=%s requests=%d", name, c, d, n)
	r := &result{Name: name, Concurrency: c, Status: map[int]int{}, firstSeen: map[int]time.Time{}}
	r.Samples = append(r.Samples, b.snapshot())
	ctx, cancel := context.WithCancel(context.Background())
	var monitor sync.WaitGroup
	monitor.Add(1)
	if len(observer) > 0 {
		monitor.Add(1)
		go func() {
			defer monitor.Done()
			t := time.NewTicker(250 * time.Millisecond)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					for key, at := range observer[0]() {
						if _, ok := r.firstSeen[key]; !ok {
							r.firstSeen[key] = at
						}
					}
				}
			}
		}()
	}
	go func() {
		defer monitor.Done()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				r.Samples = append(r.Samples, b.snapshot())
			}
		}
	}()
	var index atomic.Int64
	start := time.Now()
	deadline := start.Add(d)
	var wg sync.WaitGroup
	var mu sync.Mutex
	for worker := 0; worker < c; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := make([]observation, 0, 1000)
			for {
				if n == 0 && time.Now().After(deadline) {
					break
				}
				i := int(index.Add(1)) - 1
				if n > 0 && i >= n {
					break
				}
				t := time.Now()
				o := fn(i)
				o.Latency = float64(time.Since(t).Microseconds()) / 1000
				o.Index = i
				o.Started = t
				local = append(local, o)
			}
			mu.Lock()
			r.observations = append(r.observations, local...)
			mu.Unlock()
		}()
	}
	wg.Wait()
	r.Seconds = time.Since(start).Seconds()
	cancel()
	monitor.Wait()
	r.Samples = append(r.Samples, b.snapshot())
	lat := make([]float64, 0, len(r.observations))
	for _, o := range r.observations {
		r.Requests++
		r.Status[o.Status]++
		lat = append(lat, o.Latency)
		if o.Valid {
			r.OK++
		} else {
			if o.Status == 200 {
				r.SemanticErrors++
			}
			if len(r.Errors) < 4 {
				r.Errors = append(r.Errors, fmt.Sprintf("status=%d %s", o.Status, o.Error))
			}
		}
	}
	sort.Float64s(lat)
	r.P50 = percentile(lat, .50)
	r.P95 = percentile(lat, .95)
	r.P99 = percentile(lat, .99)
	r.RPS = float64(r.Requests) / r.Seconds
	r.GoodRPS = float64(r.OK) / r.Seconds
	log.Printf("END   %-27s valid=%d/%d good_rps=%.1f p95=%.1fms p99=%.1fms status=%v", name, r.OK, r.Requests, r.GoodRPS, r.P95, r.P99, r.Status)
	if n == 0 {
		r.observations = nil
	}
	return r
}

func (b *bench) verifyWrite(r *result, kind string, id uint, expected int, marker string, duplicate bool) {
	starts := map[int]time.Time{}
	accepted := map[int]bool{}
	for _, o := range r.observations {
		if o.Valid {
			key := o.Index
			if duplicate {
				key = users - 1
			}
			accepted[key] = true
			if starts[key].IsZero() || o.Started.Before(starts[key]) {
				starts[key] = o.Started
			}
		}
	}
	observed := r.firstSeen
	lat := []float64{}
	deadline := time.Now().Add(60 * time.Second)
	settleStart := time.Now()
	stable := 0
	maxBacklog := 0
	var rows int64
	var v video.Video
	var notifications int64
	var score float64
	var queues []queue
	expectedRows := r.OK
	if duplicate {
		expectedRows = len(accepted)
	}
	for {
		if kind == "like" {
			var list []video.Like
			must(b.db.Where("video_id = ?", id).Find(&list).Error)
			rows = int64(len(list))
			for _, row := range list {
				key := int(row.AccountID) - 1
				if _, ok := observed[key]; !ok {
					observed[key] = time.Now()
					if t, ok := starts[key]; ok {
						lat = append(lat, float64(time.Since(t).Microseconds())/1000)
					}
				}
			}
		} else {
			var list []video.Comment
			must(b.db.Where("video_id = ?", id).Find(&list).Error)
			rows = int64(len(list))
			for _, row := range list {
				prefix := marker + ":"
				if !strings.HasPrefix(row.Content, prefix) {
					continue
				}
				key, e := strconv.Atoi(strings.TrimPrefix(row.Content, prefix))
				if e != nil {
					continue
				}
				if _, ok := observed[key]; !ok {
					observed[key] = time.Now()
					if t, ok := starts[key]; ok {
						lat = append(lat, float64(time.Since(t).Microseconds())/1000)
					}
				}
			}
		}
		must(b.db.First(&v, id).Error)
		must(b.db.Table("notifications").Where("target_id = ? AND type = ?", id, kind).Count(&notifications).Error)
		var queueErr error
		queues, queueErr = b.queues()
		var pending int64
		must(b.db.Model(&events.OutboxEvent{}).Where("published_at IS NULL").Count(&pending).Error)
		// Check the outbox and eventual effects as well as management statistics.
		score = 0
		keys, err := b.rdb.Keys(context.Background(), "v1:hot:video:1m:*").Result()
		must(err)
		for _, key := range keys {
			value, err := b.rdb.ZScore(context.Background(), key, strconv.Itoa(int(id))).Result()
			if err == nil {
				score += value
			}
		}
		backlog := 0
		for _, q := range queues {
			backlog += q.Ready + q.Unacked
		}
		if backlog > maxBacklog {
			maxBacklog = backlog
		}
		if queueErr == nil && pending == 0 && backlog == 0 && len(observed) >= len(accepted) && notifications >= int64(expectedRows) && score >= float64(expectedRows) {
			stable++
		} else {
			stable = 0
		}
		if stable >= 3 || time.Now().After(deadline) {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	missing := 0
	for key := range accepted {
		if _, ok := observed[key]; !ok {
			missing++
		}
	}
	wantLikes := int64(0)
	if kind == "like" {
		wantLikes = int64(expectedRows)
	}
	passed := missing == 0 && rows == int64(expectedRows) && v.LikesCount == wantLikes && v.Popularity == int64(expectedRows) && score == float64(expectedRows) && notifications == int64(expectedRows)
	r.Verification = map[string]any{"passed": passed, "configured_unique_requests": expected, "http_accepted": r.OK, "expected_unique_rows": expectedRows, "actual_rows": rows, "missing_accepted_rows": missing, "video_likes_count": v.LikesCount, "video_popularity": v.Popularity, "redis_popularity_sum": score, "notifications": notifications, "settle_seconds": time.Since(settleStart).Seconds(), "max_queue_backlog_during_settle": maxBacklog, "final_queues": queues, "e2e_latency_note": "request start to first database observation; polling runs during HTTP load and drain at 250ms intervals; includes polling and query delay; covers row visibility, while final counters/notifications/Redis are separately reconciled"}
	lat = nil
	for key, t := range starts {
		if at, ok := observed[key]; ok {
			lat = append(lat, float64(at.Sub(t).Microseconds())/1000)
		}
	}
	sort.Float64s(lat)
	r.E2EP95 = percentile(lat, .95)
	r.E2EP99 = percentile(lat, .99)
	r.observations = nil
	r.firstSeen = nil
	log.Printf("VERIFY %s passed=%t accepted=%d rows=%d likes=%d db_pop=%d redis_pop=%.0f notifications=%d missing=%d", r.Name, passed, r.OK, rows, v.LikesCount, v.Popularity, score, notifications, missing)
}

func (b *bench) pagination() map[string]any {
	cursor := int64(0)
	seen := map[uint]bool{}
	duplicates := 0
	missing := 0
	pages := 0
	last := uint(videos + 1)
	for page := 0; page < 120; page++ {
		status, data, e := b.request("/feed/listLatest", map[string]any{"limit": 10, "latest_time": cursor}, "")
		if e != nil || status != 200 {
			return map[string]any{"passed": false, "page": page, "status": status, "error": fmt.Sprint(e)}
		}
		var resp struct {
			Videos []struct {
				ID uint `json:"id"`
			} `json:"video_list"`
			Next int64 `json:"next_time"`
		}
		if e = json.Unmarshal(data, &resp); e != nil {
			return map[string]any{"passed": false, "error": e.Error()}
		}
		for _, v := range resp.Videos {
			if seen[v.ID] {
				duplicates++
			}
			seen[v.ID] = true
			if v.ID != last-1 {
				missing++
			}
			last = v.ID
		}
		cursor = resp.Next
		pages++
		if len(resp.Videos) != 10 {
			break
		}
	}
	return map[string]any{"passed": duplicates == 0 && missing == 0 && len(seen) == 1200, "pages": pages, "unique_videos": len(seen), "duplicates": duplicates, "unexpected_id_gaps": missing, "crosses_hot_cold_boundary": true}
}
func (b *bench) snapshot() snapshot {
	s := snapshot{At: time.Now().Format(time.RFC3339Nano), MySQL: map[string]string{}, GeneratorGoroutines: runtime.NumGoroutine()}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := b.db.WithContext(ctx).Model(&events.OutboxEvent{}).Where("published_at IS NULL").Count(&s.OutboxPending).Error; err != nil {
		s.Error = err.Error()
	}
	rows, e := b.db.WithContext(ctx).Raw("SHOW GLOBAL STATUS WHERE Variable_name IN ('Questions','Threads_connected','Threads_running','Max_used_connections','Slow_queries','Aborted_connects','Innodb_row_lock_waits','Innodb_row_lock_time')").Rows()
	if e == nil {
		for rows.Next() {
			var k, v string
			if rows.Scan(&k, &v) == nil {
				s.MySQL[k] = v
			}
		}
		_ = rows.Close()
	} else {
		s.Error = e.Error()
	}
	s.Queues, e = b.queues()
	if e != nil {
		s.Error += " " + e.Error()
	}
	s.Redis, _ = b.rdb.Info(ctx, "stats", "memory").Result()
	for _, port := range []int{16060, 16061} {
		statsResp, statsErr := b.admin.Get(fmt.Sprintf("http://127.0.0.1:%d/debug/dbstats", port))
		if statsErr == nil {
			var stats sql.DBStats
			if statsResp.StatusCode == http.StatusOK && json.NewDecoder(statsResp.Body).Decode(&stats) == nil {
				if port == 16060 {
					s.APIPool = &stats
				} else {
					s.WorkerPool = &stats
				}
			}
			statsResp.Body.Close()
		}
		resp, e := b.admin.Get(fmt.Sprintf("http://127.0.0.1:%d/debug/pprof/goroutine?debug=1", port))
		if e == nil {
			data, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
			resp.Body.Close()
			line, _, _ := strings.Cut(string(data), "\n")
			if port == 16060 {
				s.APIGoroutines = line
			} else {
				s.WorkerGoroutines = line
			}
		}
	}
	return s
}
func (b *bench) resources(name string, c int) {
	cmd := exec.Command("docker", "stats", "--no-stream", "--format", "{{json .}}", b.project+"-mysql-1", b.project+"-redis-1", b.project+"-rabbitmq-1")
	data, e := cmd.CombinedOutput()
	if e != nil {
		data = append(data, []byte(e.Error())...)
	}
	_ = os.WriteFile(filepath.Join(b.out, fmt.Sprintf("resources-%s-c%d.jsonl", name, c)), data, 0644)
}
func (b *bench) save() {
	data, e := json.MarshalIndent(b.report, "", "  ")
	if e == nil {
		_ = os.WriteFile(filepath.Join(b.out, "results.json"), data, 0644)
	}
	var s strings.Builder
	fmt.Fprintf(&s, "# P0 load-test baseline\n\nStarted: %s. Finished: %s.\n\n", b.report.Started, b.report.Finished)
	s.WriteString("Real compiled API + Worker, dedicated MySQL 8 / Redis 7 / RabbitMQ 3.12. 6,000 fixture accounts, 50,000 videos with distinct one-second timestamps. No password hashing or login in measured paths: valid fixture sessions are pre-created through the existing signing code. Existing authentication and rate limits remain enabled.\n\nClosed-loop concurrency, same Windows machine for generator/API/Worker; dependencies run in Docker Desktop. Results include local virtualization/transport overhead. This is a bounded baseline, not a production capacity certificate; no SLA was supplied. HTTP latency covers the whole request, all responses included. Success throughput excludes non-200 and invalid response bodies. Status 0 is a transport error. Each read stage includes any in-flight completion tail. E2E row-visibility latency is observed by polling during load and drain every 250ms, and includes polling and query delay. Final counters, Redis and notifications are checked separately. Docker resource files are post-stage snapshots, not resource peaks; sampled MySQL/Redis/MQ/goroutine data are in results.json.\n\n")
	s.WriteString("| Scenario | Concurrent | Requests | Valid | Valid RPS | P50 ms | P95 ms | P99 ms | Status | Data check |\n|---|---:|---:|---:|---:|---:|---:|---:|---|---|\n")
	for _, r := range b.report.Results {
		check := "n/a"
		if r.Verification != nil {
			check = fmt.Sprint(r.Verification["passed"])
		}
		fmt.Fprintf(&s, "| %s | %d | %d | %d | %.1f | %.1f | %.1f | %.1f | %v | %s |\n", r.Name, r.Concurrency, r.Requests, r.OK, r.GoodRPS, r.P50, r.P95, r.P99, r.Status, check)
	}
	s.WriteString("\n## Functional checks\n\n```json\n")
	checks, _ := json.MarshalIndent(b.report.Checks, "", "  ")
	s.Write(checks)
	s.WriteString("\n```\n\n## Write verification\n\n")
	for _, r := range b.report.Results {
		if r.Verification == nil {
			continue
		}
		fmt.Fprintf(&s, "### %s, concurrency %d\n\n```json\n", r.Name, r.Concurrency)
		v, _ := json.MarshalIndent(r.Verification, "", "  ")
		s.Write(v)
		s.WriteString("\n```\n\n")
	}
	reportText := s.String()
	if b.report.Environment["external_processes"] == true {
		reportText = strings.ReplaceAll(reportText, "same Windows machine for generator/API/Worker; dependencies run in Docker Desktop.", "Windows generator; externally managed API/Worker and dependencies (see compose.resolved.yml).")
	}
	_ = os.WriteFile(filepath.Join(b.out, "REPORT.md"), []byte(reportText), 0644)
}
