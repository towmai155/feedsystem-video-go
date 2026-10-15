// Package testutil provides isolated MySQL databases for integration tests.
package testutil

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"os"
	"regexp"
	"testing"
	"time"

	drivermysql "github.com/go-sql-driver/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var testDatabaseName = regexp.MustCompile(`^feedsystem_test_[0-9a-f]{16}$`)

// NewMySQL creates and removes only its own randomly named database. The DSN
// supplies connection credentials; its original database is never used.
func NewMySQL(t testing.TB) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("FEEDSYSTEM_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set FEEDSYSTEM_TEST_MYSQL_DSN to run isolated MySQL integration tests")
	}
	cfg, err := drivermysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal("invalid FEEDSYSTEM_TEST_MYSQL_DSN")
	}
	cfg.DBName = ""
	cfg.ParseTime = true
	cfg.Loc = time.UTC
	cfg.Timeout = 3 * time.Second
	cfg.ReadTimeout = 5 * time.Second
	cfg.WriteTimeout = 5 * time.Second
	admin, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal("open MySQL test connection")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := admin.PingContext(ctx); err != nil {
		admin.Close()
		t.Fatalf("connect to MySQL test server: %v", err)
	}
	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	name := "feedsystem_test_" + hex.EncodeToString(suffix)
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE `"+name+"`"); err != nil {
		admin.Close()
		t.Fatalf("create isolated MySQL test database: %v", err)
	}
	var connection *sql.DB
	t.Cleanup(func() {
		if connection != nil {
			connection.Close()
		}
		defer admin.Close()
		if !testDatabaseName.MatchString(name) {
			t.Error("refusing to remove a database outside the test namespace")
			return
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		if _, err := admin.ExecContext(cleanupCtx, "DROP DATABASE `"+name+"`"); err != nil {
			t.Errorf("remove isolated MySQL test database: %v", err)
		}
	})
	cfg.DBName = name
	connection, err = sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal("open isolated MySQL test database")
	}
	connection.SetMaxOpenConns(8)
	db, err := gorm.Open(mysql.New(mysql.Config{Conn: connection}), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("initialize isolated MySQL test database: %v", err)
	}
	return db
}
