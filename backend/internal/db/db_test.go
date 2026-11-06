package db

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"feedsystem_video_go/internal/config"
	"feedsystem_video_go/internal/testutil"
	drivermysql "github.com/go-sql-driver/mysql"
)

// Concurrent bursts must wait for the configured pool, then reuse the same
// physical MySQL connections rather than opening one per waiting request.
func TestConcurrentBurstsReuseBoundedPool(t *testing.T) {
	isolated := testutil.NewMySQL(t)
	var database string
	if err := isolated.Raw("SELECT DATABASE()").Scan(&database).Error; err != nil {
		t.Fatal(err)
	}
	dsn, err := drivermysql.ParseDSN(os.Getenv("FEEDSYSTEM_TEST_MYSQL_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	host, portText, err := net.SplitHostPort(dsn.Addr)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("MYSQL_MAX_OPEN_CONNS", "4")
	t.Setenv("MYSQL_MAX_IDLE_CONNS", "4")
	db, err := NewDB(config.DatabaseConfig{Host: host, Port: port, User: dsn.User, Password: dsn.Passwd, DBName: database})
	if err != nil {
		t.Fatal(err)
	}
	defer CloseDB(db)
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	connectionIDs := map[uint64]bool{}
	for burst := 0; burst < 3; burst++ {
		var wg sync.WaitGroup
		start := make(chan struct{})
		ids := make(chan uint64, 40)
		failures := make(chan error, 40)
		for i := 0; i < 40; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				var id uint64
				var sleep int
				if err := db.WithContext(ctx).Raw("SELECT CONNECTION_ID(), SLEEP(0.005)").Row().Scan(&id, &sleep); err != nil {
					failures <- err
					return
				}
				ids <- id
			}()
		}
		close(start)
		wg.Wait()
		close(ids)
		close(failures)
		for err := range failures {
			t.Error(err)
		}
		for id := range ids {
			connectionIDs[id] = true
		}
		if stats := pool.Stats(); stats.OpenConnections > 4 {
			t.Fatal(fmt.Sprintf("pool exceeded budget: %+v", stats))
		}
	}
	if len(connectionIDs) == 0 || len(connectionIDs) > 4 {
		t.Fatalf("expected reuse of at most 4 physical connections, saw %d", len(connectionIDs))
	}
	if pool.Stats().WaitCount == 0 {
		t.Fatal("concurrent burst did not exercise pool backpressure")
	}
}
